use std::collections::HashMap;
use std::io;
use std::net::TcpStream;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use anyhow::{Context as _, anyhow, bail};
use crossbeam_channel::{Receiver, Sender, bounded, unbounded};
use parking_lot::Mutex;
use tungstenite::protocol::WebSocketConfig;
use tungstenite::stream::MaybeTlsStream;
use tungstenite::{Message, WebSocket};
use uuid::Uuid;

use waku_protocol::MAX_WIRE_MESSAGE_BYTES;
use waku_protocol::connection_diagnostics::Event;
use waku_protocol::{
    ClientMessage, Command, PROTOCOL_VERSION, ReplayCursor, Request, ResponseOutcome,
    ResponsePayload, SequencedEvent, ServerMessage,
};

const READ_POLL_INTERVAL: Duration = Duration::from_millis(25);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(120);
mod replay;

enum Outgoing {
    Message(ClientMessage),
    Disconnect,
    Shutdown,
}

#[derive(Debug)]
enum TransportLost {
    NotSent,
    QueueClosed,
    AwaitingResponse,
}

impl std::fmt::Display for TransportLost {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::NotSent => "Waku daemon is disconnected",
            Self::QueueClosed => "Waku daemon connection is closed",
            Self::AwaitingResponse => "Waku daemon disconnected",
        })
    }
}

impl std::error::Error for TransportLost {}

#[derive(Debug)]
struct RequestTimedOut;

impl std::fmt::Display for RequestTimedOut {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "timed out waiting for Waku daemon: {}",
            crossbeam_channel::RecvTimeoutError::Timeout
        )
    }
}

impl std::error::Error for RequestTimedOut {}

struct ClientInner {
    connection_id: Uuid,
    local_diagnostics: bool,
    secure_credential_transport: bool,
    outgoing: Sender<Outgoing>,
    pending: Mutex<HashMap<Uuid, Sender<anyhow::Result<ResponsePayload>>>>,
    delivery: Mutex<replay::Delivery>,
    task_state_subscribers: Mutex<Vec<Sender<u64>>>,
    disconnected: AtomicBool,
}

#[derive(Clone)]
pub struct DaemonClient {
    inner: Arc<ClientInner>,
}

/// A command has entered this connection's queue. Waiting is a worker operation.
pub struct PendingResponse {
    client: DaemonClient,
    request_id: Uuid,
    reply: Receiver<anyhow::Result<ResponsePayload>>,
}

impl PendingResponse {
    pub fn wait(self, timeout: Duration) -> anyhow::Result<ResponsePayload> {
        match self.reply.recv_timeout(timeout) {
            Ok(outcome) => outcome,
            Err(crossbeam_channel::RecvTimeoutError::Timeout) => Err(RequestTimedOut.into()),
            Err(crossbeam_channel::RecvTimeoutError::Disconnected) => {
                Err(TransportLost::AwaitingResponse.into())
            }
        }
    }
}

impl Drop for PendingResponse {
    fn drop(&mut self) {
        // Also clean up if an observer thread could not start after admission.
        self.client.inner.pending.lock().remove(&self.request_id);
    }
}

impl DaemonClient {
    pub fn connect(address: &str, token: String) -> anyhow::Result<Self> {
        Self::connect_with_resume(address, token, Vec::new())
    }

    pub fn connect_with_resume(
        address: &str,
        token: String,
        resume_from: Vec<ReplayCursor>,
    ) -> anyhow::Result<Self> {
        Self::connect_transport(address, token, resume_from, false)
    }

    pub fn connect_local_with_resume(
        address: &str,
        token: String,
        resume_from: Vec<ReplayCursor>,
    ) -> anyhow::Result<Self> {
        Self::connect_transport(address, token, resume_from, true)
    }

    fn connect_transport(
        address: &str,
        token: String,
        resume_from: Vec<ReplayCursor>,
        local_diagnostics: bool,
    ) -> anyhow::Result<Self> {
        let connection_id = Uuid::new_v4();
        let delivery = replay::Delivery::new(resume_from.clone());
        let url = url::Url::parse(&daemon_url(address)?)?;
        let config = WebSocketConfig::default()
            .max_message_size(Some(MAX_WIRE_MESSAGE_BYTES))
            .max_frame_size(Some(MAX_WIRE_MESSAGE_BYTES));
        let deadline = Instant::now() + Duration::from_secs(5);
        let addresses = url
            .socket_addrs(|| None)
            .context("could not resolve Waku daemon host")?;
        let mut stream = None;
        let mut last_error = None;
        for address in addresses {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                break;
            }
            match TcpStream::connect_timeout(&address, remaining) {
                Ok(candidate) => {
                    stream = Some(candidate);
                    break;
                }
                Err(error) => last_error = Some(error),
            }
        }
        let stream = stream
            .ok_or_else(|| last_error.unwrap_or_else(|| io::Error::from(io::ErrorKind::TimedOut)))
            .context("could not reach Waku daemon within 5 seconds")?;
        let secure_credential_transport =
            url.scheme() == "wss" || stream.peer_addr()?.ip().is_loopback();
        stream.set_read_timeout(Some(Duration::from_secs(5)))?;
        stream.set_write_timeout(Some(Duration::from_secs(5)))?;
        let (mut socket, _) =
            tungstenite::client_tls_with_config(url.as_str(), stream, Some(config), None)
                .context("could not connect to Waku daemon")?;
        set_client_read_timeout(&mut socket, Some(Duration::from_secs(5)))?;
        write_json(
            &mut socket,
            &ClientMessage::Hello {
                protocol_version: PROTOCOL_VERSION,
                token,
                client_id: connection_id,
                resume_from,
            },
        )
        .context("could not send Waku daemon Hello")?;
        let hello = read_server_message(&mut socket).context("could not read Waku daemon Hello")?;
        match hello {
            ServerMessage::Hello {
                protocol_version, ..
            } if protocol_version == PROTOCOL_VERSION => {}
            ServerMessage::Hello {
                protocol_version, ..
            } => bail!(
                "daemon protocol {protocol_version} does not match desktop protocol {PROTOCOL_VERSION}"
            ),
            ServerMessage::Rejected { message } => bail!("daemon rejected connection: {message}"),
            other => bail!("daemon sent an invalid handshake response: {other:?}"),
        }
        set_client_read_timeout(&mut socket, Some(READ_POLL_INTERVAL))?;
        match socket.get_mut() {
            MaybeTlsStream::Plain(stream) => stream.set_write_timeout(None)?,
            MaybeTlsStream::Rustls(stream) => stream.sock.set_write_timeout(None)?,
            #[allow(unreachable_patterns)]
            _ => {}
        }

        let (outgoing, outgoing_rx) = unbounded();
        let inner = Arc::new(ClientInner {
            connection_id,
            local_diagnostics,
            secure_credential_transport,
            outgoing,
            pending: Mutex::new(HashMap::new()),
            delivery: Mutex::new(delivery),
            task_state_subscribers: Mutex::new(Vec::new()),
            disconnected: AtomicBool::new(false),
        });
        let thread_inner = inner.clone();
        std::thread::Builder::new()
            .name("waku-daemon-client".into())
            .spawn(move || run_client(socket, outgoing_rx, thread_inner))
            .context("could not start Waku daemon client thread")?;
        Ok(Self { inner })
    }

    pub fn subscribe(&self, session_id: Uuid, runtime_id: Uuid) -> Receiver<SequencedEvent> {
        let (receiver, request) = self.inner.delivery.lock().subscribe(session_id, runtime_id);
        if self
            .inner
            .outgoing
            .send(Outgoing::Message(ClientMessage::Request(request)))
            .is_err()
        {
            self.inner
                .delivery
                .lock()
                .unsubscribe(session_id, runtime_id);
        }
        receiver
    }

    /// Whether two handles send through the same WebSocket connection.
    ///
    /// The daemon supervisor publishes replacement clients after a managed
    /// restart. Runtime adapters use this identity check to ignore the
    /// subscription's initial snapshot and wait for an actual replacement.
    pub fn same_connection(&self, other: &Self) -> bool {
        Arc::ptr_eq(&self.inner, &other.inner)
    }

    pub fn connection_id(&self) -> Uuid {
        self.inner.connection_id
    }

    pub fn is_transport_lost(error: &anyhow::Error) -> bool {
        error.is::<TransportLost>()
    }

    pub fn definitely_not_sent(error: &anyhow::Error) -> bool {
        matches!(
            error.downcast_ref::<TransportLost>(),
            Some(TransportLost::NotSent | TransportLost::QueueClosed)
        )
    }

    pub fn is_request_timeout(error: &anyhow::Error) -> bool {
        error.is::<RequestTimedOut>()
    }

    /// Return only fixed diagnostic codes. Handshake errors can include peer-controlled text.
    pub fn connection_error_category(error: &anyhow::Error) -> &'static str {
        let message = error.to_string();
        if message.starts_with("daemon protocol ") {
            return "protocol_mismatch";
        }
        if message == "daemon rejected connection: authentication failed" {
            return "authentication_rejected";
        }
        if message.starts_with("daemon rejected connection:") {
            return "handshake_rejected";
        }
        match message.as_str() {
            "could not resolve Waku daemon host" => "resolve_failed",
            "could not reach Waku daemon within 5 seconds" => {
                match error.downcast_ref::<io::Error>().map(io::Error::kind) {
                    Some(io::ErrorKind::TimedOut) => "connect_timeout",
                    Some(io::ErrorKind::ConnectionRefused) => "connect_refused",
                    _ => "connect_failed",
                }
            }
            "could not connect to Waku daemon" => "websocket_upgrade_failed",
            "could not send Waku daemon Hello" => "hello_write_failed",
            "could not read Waku daemon Hello" => "hello_read_failed",
            _ if error.downcast_ref::<io::Error>().is_some() => "socket_io",
            _ if Self::is_transport_lost(error) => "connection_lost",
            _ => "connection_failed",
        }
    }

    pub fn allows_credential_enrollment(&self) -> bool {
        self.inner.secure_credential_transport && !self.is_disconnected()
    }

    pub fn is_disconnected(&self) -> bool {
        self.inner.disconnected.load(Ordering::Acquire)
    }

    pub fn unsubscribe(&self, session_id: Uuid, runtime_id: Uuid) {
        self.inner
            .delivery
            .lock()
            .unsubscribe(session_id, runtime_id);
    }

    pub fn subscribe_task_state(&self) -> Receiver<u64> {
        let (events, receiver) = unbounded();
        self.inner.task_state_subscribers.lock().push(events);
        receiver
    }

    pub fn request(
        &self,
        session_id: Uuid,
        runtime_id: Uuid,
        command: Command,
    ) -> anyhow::Result<ResponsePayload> {
        self.request_with_timeout(session_id, runtime_id, command, REQUEST_TIMEOUT)
    }

    pub fn request_with_timeout(
        &self,
        session_id: Uuid,
        runtime_id: Uuid,
        command: Command,
        timeout: Duration,
    ) -> anyhow::Result<ResponsePayload> {
        self.begin_request(session_id, runtime_id, command)?
            .wait(timeout)
    }

    pub fn begin_request(
        &self,
        session_id: Uuid,
        runtime_id: Uuid,
        command: Command,
    ) -> anyhow::Result<PendingResponse> {
        let request_id = Uuid::new_v4();
        let (response, response_rx) = bounded(1);
        {
            let mut pending = self.inner.pending.lock();
            if self.is_disconnected() {
                return Err(TransportLost::NotSent.into());
            }
            pending.insert(request_id, response);
        }
        let message = ClientMessage::Request(Request {
            request_id,
            session_id,
            runtime_id,
            command,
        });
        if self
            .inner
            .outgoing
            .send(Outgoing::Message(message))
            .is_err()
        {
            self.inner.pending.lock().remove(&request_id);
            return Err(TransportLost::QueueClosed.into());
        }
        Ok(PendingResponse {
            client: self.clone(),
            request_id,
            reply: response_rx,
        })
    }

    pub fn notify(
        &self,
        session_id: Uuid,
        runtime_id: Uuid,
        command: Command,
    ) -> anyhow::Result<()> {
        if self.inner.disconnected.load(Ordering::Acquire) {
            return Err(TransportLost::NotSent.into());
        }
        self.inner
            .outgoing
            .send(Outgoing::Message(ClientMessage::Request(Request {
                // The nil request id is reserved for fire-and-forget controls;
                // the daemon executes them in the runtime mailbox but does
                // not allocate or send a response.
                request_id: Uuid::nil(),
                session_id,
                runtime_id,
                command,
            })))
            .map_err(|_| TransportLost::QueueClosed.into())
    }

    pub fn last_sequences(&self) -> Vec<ReplayCursor> {
        self.inner.delivery.lock().cursors()
    }

    /// Close only this client transport; the externally managed daemon remains running.
    pub fn disconnect(&self) {
        let _ = self.inner.outgoing.send(Outgoing::Disconnect);
    }

    pub fn shutdown(&self) {
        let _ = self.inner.outgoing.send(Outgoing::Shutdown);
    }
}

fn daemon_url(address: &str) -> anyhow::Result<String> {
    let normalized = if address.starts_with("ws://") || address.starts_with("wss://") {
        address.to_owned()
    } else {
        format!("ws://{address}")
    };
    let mut url = url::Url::parse(&normalized).context("Waku daemon address is invalid")?;
    url.set_path("/v1");
    url.set_query(None);
    url.set_fragment(None);
    Ok(url.into())
}

fn run_client(
    mut socket: WebSocket<MaybeTlsStream<TcpStream>>,
    outgoing: Receiver<Outgoing>,
    inner: Arc<ClientInner>,
) {
    let closed = |reason| Event::new("connection_closed", inner.connection_id).because(reason);
    let mut end = 'connection: loop {
        if let Some((session, runtime, elapsed)) = inner.delivery.lock().expired_page() {
            let mut event = closed("replay_timeout");
            event.session_id = Some(session);
            event.runtime_id = Some(runtime);
            event.elapsed_ms = Some(elapsed.as_millis() as u64);
            break event;
        }
        while let Ok(message) = outgoing.try_recv() {
            match message {
                Outgoing::Message(message) => {
                    if let Err(error) = write_json(&mut socket, &message) {
                        break 'connection closed("socket_write_failed").with_error(error.as_ref());
                    }
                }
                Outgoing::Disconnect => {
                    let _ = socket.close(None);
                    let _ = socket.flush();
                    break 'connection closed("local_disconnect");
                }
                Outgoing::Shutdown => {
                    let _ = write_json(&mut socket, &ClientMessage::Shutdown);
                    let _ = socket.flush();
                    break 'connection closed("local_shutdown");
                }
            }
        }

        match socket.read() {
            Ok(Message::Text(text)) => {
                let Ok(message) = serde_json::from_str::<ServerMessage>(text.as_ref()) else {
                    continue;
                };
                match message {
                    ServerMessage::Response {
                        request_id,
                        outcome,
                    } => {
                        let runtime = inner.delivery.lock().requested_runtime(request_id);
                        if let Some((session_id, runtime_id)) = runtime {
                            let rejection = match &outcome {
                                ResponseOutcome::Error { error } => {
                                    Some(replay_rejection(&error.message))
                                }
                                _ => None,
                            };
                            let next = inner.delivery.lock().page(request_id, outcome);
                            match next {
                                Ok(Some(request)) => {
                                    if let Err(error) =
                                        write_json(&mut socket, &ClientMessage::Request(request))
                                    {
                                        break 'connection closed("replay_write_failed")
                                            .with_error(error.as_ref());
                                    }
                                }
                                Ok(None) => {}
                                Err(error) => {
                                    eprintln!(
                                        "Waku runtime replay failed: {error}; reconnecting without skipping events"
                                    );
                                    let mut event = closed(
                                        rejection.unwrap_or_else(|| replay_validation(&error)),
                                    );
                                    event.session_id = Some(session_id);
                                    event.runtime_id = Some(runtime_id);
                                    break 'connection event;
                                }
                            }
                        } else if let Some(pending) = inner.pending.lock().remove(&request_id) {
                            let result = match outcome {
                                ResponseOutcome::Ok { payload } => Ok(payload),
                                ResponseOutcome::Error { error } => Err(anyhow!(error.message)),
                            };
                            let _ = pending.send(result);
                        }
                    }
                    ServerMessage::Event(event) => {
                        let (session_id, runtime_id) = (event.session_id, event.runtime_id);
                        if let Err(error) = inner.delivery.lock().live(event) {
                            eprintln!(
                                "Waku runtime replay failed: {error}; reconnecting without skipping events"
                            );
                            let mut end = closed(replay_validation(&error));
                            end.session_id = Some(session_id);
                            end.runtime_id = Some(runtime_id);
                            break 'connection end;
                        }
                    }
                    ServerMessage::TaskStateChanged { revision } => {
                        inner
                            .task_state_subscribers
                            .lock()
                            .retain(|subscriber| subscriber.send(revision).is_ok());
                    }
                    ServerMessage::ShuttingDown => break closed("daemon_shutdown"),
                    ServerMessage::Hello { .. } | ServerMessage::Rejected { .. } => {}
                }
            }
            Ok(Message::Close(frame)) => {
                let mut event = closed("peer_close");
                event.close_code = frame.map(|close| close.code.into());
                break event;
            }
            Ok(Message::Ping(_)) => {
                let _ = socket.flush();
            }
            Ok(_) => {}
            Err(tungstenite::Error::Io(error)) if retryable_io(&error) => {}
            Err(tungstenite::Error::ConnectionClosed | tungstenite::Error::AlreadyClosed) => {
                break closed("peer_closed");
            }
            Err(tungstenite::Error::Protocol(
                tungstenite::error::ProtocolError::ResetWithoutClosingHandshake,
            )) => break closed("peer_eof_without_close"),
            Err(tungstenite::Error::Capacity(_)) => break closed("wire_limit_exceeded"),
            Err(error) => break closed("socket_read_failed").with_error(&error),
        }
    };

    inner.disconnected.store(true, Ordering::Release);
    let pending = std::mem::take(&mut *inner.pending.lock());
    if inner.local_diagnostics {
        end.pending_requests = Some(pending.len());
        end.write_desktop();
    }
    for (_, response) in pending {
        let _ = response.send(Err(TransportLost::AwaitingResponse.into()));
    }
    // Closing the desktop transport is not evidence that a daemon-owned
    // provider exited. Drop the subscription senders so runtime adapters can
    // hand off to a replacement client and ask the daemon whether the same
    // runtime still exists. Real provider exits arrive through the replayable
    // `processExited` event emitted by the daemon.
    inner.delivery.lock().close();
    inner.task_state_subscribers.lock().clear();
}

fn replay_rejection(message: &str) -> &'static str {
    match message {
        "runtime replay is no longer available" => "replay_runtime_unavailable",
        "replay cursor is outside this runtime" => "replay_cursor_out_of_range",
        "replay daemon epoch changed" => "replay_epoch_changed",
        _ if message.starts_with("runtime replay storage failed:") => "replay_storage_failed",
        _ => "replay_rejected",
    }
}

fn replay_validation(error: &anyhow::Error) -> &'static str {
    match error.to_string().as_str() {
        "runtime epoch changed during replay" | "runtime epoch changed between replay pages" => {
            "replay_epoch_changed"
        }
        "replay boundary changed" => "replay_boundary_changed",
        "invalid replay page bounds" => "replay_invalid_bounds",
        "replay page made no progress" => "replay_page_stalled",
        "replay page acknowledged an event it did not supply" => "replay_missing_event",
        "replay ended before its boundary" => "replay_page_incomplete",
        "replay event identity or order mismatch" => "replay_identity_or_order",
        "live events exceeded the catch-up buffer; reconnect from the confirmed cursor" => {
            "replay_buffer_full"
        }
        _ => "replay_validation_failed",
    }
}

fn set_client_read_timeout(
    socket: &mut WebSocket<MaybeTlsStream<TcpStream>>,
    timeout: Option<Duration>,
) -> io::Result<()> {
    match socket.get_mut() {
        MaybeTlsStream::Plain(stream) => stream.set_read_timeout(timeout),
        MaybeTlsStream::Rustls(stream) => stream.sock.set_read_timeout(timeout),
        #[allow(unreachable_patterns)]
        _ => Ok(()),
    }
}

fn retryable_io(error: &io::Error) -> bool {
    retryable_error(error)
}

fn retryable_error(error: &(dyn std::error::Error + 'static)) -> bool {
    if let Some(error) = error.downcast_ref::<io::Error>() {
        if matches!(
            error.kind(),
            io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut | io::ErrorKind::Interrupted
        ) {
            return true;
        }
        #[cfg(unix)]
        if error.raw_os_error() == Some(libc::EAGAIN)
            || error.raw_os_error() == Some(libc::EWOULDBLOCK)
        {
            return true;
        }
    }
    error.source().is_some_and(retryable_error)
}

fn write_json<S: io::Read + io::Write, T: serde::Serialize>(
    socket: &mut WebSocket<S>,
    value: &T,
) -> anyhow::Result<()> {
    let payload = serde_json::to_string(value)?;
    socket.send(Message::Text(payload.into()))?;
    Ok(())
}

fn read_server_message(
    socket: &mut WebSocket<MaybeTlsStream<TcpStream>>,
) -> anyhow::Result<ServerMessage> {
    loop {
        match socket.read()? {
            Message::Text(text) => return Ok(serde_json::from_str(text.as_ref())?),
            Message::Ping(_) => socket.flush()?,
            Message::Close(_) => bail!("Waku daemon closed during handshake"),
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn daemon_endpoint_accepts_addresses_and_secure_urls() {
        assert_eq!(
            daemon_url("127.0.0.1:4312").unwrap(),
            "ws://127.0.0.1:4312/v1"
        );
        assert_eq!(
            daemon_url("wss://waku.example.test/old?ignored=1").unwrap(),
            "wss://waku.example.test/v1"
        );
    }

    #[test]
    fn abrupt_peer_loss_distinguishes_in_flight_from_unsent_requests() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let server = std::thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            let mut socket = tungstenite::accept(stream).unwrap();
            let hello: ClientMessage =
                serde_json::from_str(socket.read().unwrap().to_text().unwrap()).unwrap();
            let ClientMessage::Hello { client_id, .. } = hello else {
                panic!("expected Hello")
            };
            write_json(
                &mut socket,
                &ServerMessage::Hello {
                    protocol_version: PROTOCOL_VERSION,
                    daemon_version: "fixture".into(),
                },
            )
            .unwrap();
            assert!(matches!(socket.read().unwrap(), Message::Text(_)));
            client_id
        });
        let client = DaemonClient::connect(&address.to_string(), "fixture".into()).unwrap();
        let error = client
            .request_with_timeout(
                Uuid::nil(),
                Uuid::nil(),
                Command::GetSettings,
                Duration::from_secs(3),
            )
            .unwrap_err();
        assert!(DaemonClient::is_transport_lost(&error));
        assert!(!DaemonClient::definitely_not_sent(&error));
        let error = client
            .notify(Uuid::nil(), Uuid::nil(), Command::GetSettings)
            .unwrap_err();
        assert!(DaemonClient::definitely_not_sent(&error));
        assert_eq!(server.join().unwrap(), client.connection_id());
        assert!(!DaemonClient::is_transport_lost(&anyhow!(
            "Waku daemon disconnected"
        )));
    }

    #[test]
    fn requests_can_time_out_and_disconnect_without_stopping_the_server() {
        use std::net::TcpListener;
        use std::sync::mpsc;
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let (closed, closure) = mpsc::channel();
        let server = std::thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            stream
                .set_read_timeout(Some(Duration::from_secs(2)))
                .unwrap();
            let mut socket = tungstenite::accept(stream).unwrap();
            assert!(matches!(socket.read().unwrap(), Message::Text(_)));
            write_json(
                &mut socket,
                &ServerMessage::Hello {
                    protocol_version: PROTOCOL_VERSION,
                    daemon_version: "test".into(),
                },
            )
            .unwrap();
            assert!(matches!(socket.read().unwrap(), Message::Text(_)));
            assert!(matches!(socket.read().unwrap(), Message::Close(_)));
            closed.send(()).unwrap();
        });
        let client = DaemonClient::connect(&address.to_string(), "test-token".into()).unwrap();
        assert!(client.allows_credential_enrollment());
        let started = Instant::now();
        let timeout = client
            .request_with_timeout(
                Uuid::nil(),
                Uuid::nil(),
                Command::GetSettings,
                Duration::from_millis(60),
            )
            .unwrap_err();
        assert!(DaemonClient::is_request_timeout(&timeout));
        assert!(!DaemonClient::is_transport_lost(&timeout));
        assert!(started.elapsed() < Duration::from_secs(1));
        client.disconnect();
        closure.recv_timeout(Duration::from_secs(2)).unwrap();
        server.join().unwrap();
    }
}
