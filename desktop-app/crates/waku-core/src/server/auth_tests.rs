use super::super::{
    Backend, ClientMessage, ConnectionPermit, EventSink, Hub, MAX_CONNECTIONS,
    MAX_PENDING_AUTHENTICATIONS, PROTOCOL_VERSION, Request, RequestDispatcher, ResponsePayload,
    ServerMessage, ServerOptions, handle_connection,
};
use super::*;
use std::net::{SocketAddr, TcpListener};
use std::sync::atomic::AtomicUsize;
use std::sync::mpsc::{self, Receiver};
use std::thread::{self, JoinHandle};
use tungstenite::protocol::frame::Frame;
use tungstenite::protocol::frame::coding::{Data, OpCode};
use tungstenite::{Message, WebSocket};
use uuid::Uuid;

struct FixtureBackend;

impl Backend for FixtureBackend {
    fn handle(&self, _: Request, _: EventSink) -> anyhow::Result<ResponsePayload> {
        Ok(ResponsePayload::Ack)
    }
}

struct Fixture {
    address: SocketAddr,
    shutdown: Arc<AtomicBool>,
    pending: Arc<AtomicUsize>,
    active: Arc<AtomicUsize>,
    result: Receiver<anyhow::Result<()>>,
    worker: Option<JoinHandle<()>>,
}

impl Fixture {
    fn new(timeout: Duration) -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let shutdown = Arc::new(AtomicBool::new(false));
        let pending = Arc::new(AtomicUsize::new(0));
        let active = Arc::new(AtomicUsize::new(0));
        let (sent, result) = mpsc::channel();
        let worker = {
            let shutdown = shutdown.clone();
            let pending = pending.clone();
            let active = active.clone();
            thread::spawn(move || {
                let (stream, _) = listener.accept().unwrap();
                let hub = Arc::new(Hub::default());
                let dispatcher = Arc::new(RequestDispatcher::new(
                    Arc::new(FixtureBackend),
                    hub.clone(),
                ));
                let permit =
                    ConnectionPermit::acquire(&pending, MAX_PENDING_AUTHENTICATIONS).unwrap();
                let accepted_at = Instant::now();
                let outcome = handle_connection(
                    stream,
                    "fixture-token",
                    dispatcher,
                    hub,
                    shutdown,
                    &ServerOptions {
                        authentication_timeout: timeout,
                        ..ServerOptions::default()
                    },
                    accepted_at,
                    permit,
                    active,
                );
                let _ = sent.send(outcome);
            })
        };
        Self {
            address,
            shutdown,
            pending,
            active,
            result,
            worker: Some(worker),
        }
    }

    fn connect(&self) -> WebSocket<TcpStream> {
        let stream = TcpStream::connect(self.address).unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        stream
            .set_write_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        tungstenite::client(format!("ws://{}/v1", self.address), stream)
            .unwrap()
            .0
    }

    fn assert_released(&self) {
        assert_eq!(self.pending.load(Ordering::Acquire), 0);
        assert_eq!(self.active.load(Ordering::Acquire), 0);
    }
}

impl Drop for Fixture {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::Release);
        // Also wake accept if setup failed before the client connected.
        let _ = TcpStream::connect(self.address);
        if let Some(worker) = self.worker.take() {
            worker.join().unwrap();
        }
    }
}

fn send_hello(socket: &mut WebSocket<TcpStream>, token: &str) {
    socket
        .send(Message::Text(
            serde_json::to_string(&ClientMessage::Hello {
                protocol_version: PROTOCOL_VERSION,
                token: token.into(),
                client_id: Uuid::new_v4(),
                resume_from: Vec::new(),
            })
            .unwrap()
            .into(),
        ))
        .unwrap();
}

#[test]
fn authentication_timeout_options_are_bounded() {
    assert_eq!(
        ServerOptions::default().authentication_timeout,
        Duration::from_secs(20)
    );
    for seconds in [1, 20, 60] {
        assert!(
            ServerOptions {
                authentication_timeout: Duration::from_secs(seconds),
                ..Default::default()
            }
            .validate()
            .is_ok()
        );
    }
    for timeout in [
        Duration::ZERO,
        Duration::from_millis(999),
        Duration::from_secs(61),
    ] {
        assert!(
            ServerOptions {
                authentication_timeout: timeout,
                ..Default::default()
            }
            .validate()
            .is_err()
        );
    }
}

#[test]
fn authentication_accepts_valid_hello_after_six_second_mobile_delay() {
    let fixture = Fixture::new(ServerOptions::default().authentication_timeout);
    let mut socket = fixture.connect();
    thread::sleep(Duration::from_secs(6));
    send_hello(&mut socket, "fixture-token");
    let response: ServerMessage =
        serde_json::from_str(socket.read().unwrap().to_text().unwrap()).unwrap();
    assert!(matches!(response, ServerMessage::Hello { .. }));
    socket.close(None).unwrap();
    fixture
        .result
        .recv_timeout(Duration::from_secs(1))
        .unwrap()
        .unwrap();
    fixture.assert_released();
}

#[test]
fn authentication_delayed_hello_still_expires_at_configured_deadline() {
    let fixture = Fixture::new(Duration::from_millis(300));
    let mut socket = fixture.connect();
    let failure = fixture.result.recv_timeout(Duration::from_secs(2)).unwrap();
    let error = format!("{:#}", failure.unwrap_err());
    assert!(error.contains("client Hello failed"));
    assert!(error.contains("authentication deadline exceeded"));
    assert!(socket.read().is_err());
    fixture.assert_released();
}

#[test]
fn authentication_ping_cannot_extend_the_absolute_deadline() {
    let fixture = Fixture::new(Duration::from_millis(300));
    let mut socket = fixture.connect();
    let started = Instant::now();
    let mut pings = 0;
    loop {
        if socket.send(Message::Ping(Vec::new().into())).is_err() || socket.read().is_err() {
            break;
        }
        pings += 1;
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "Ping kept unauthenticated connection alive"
        );
        thread::sleep(Duration::from_millis(15));
    }
    assert!(pings > 1);
    assert!(
        fixture
            .result
            .recv_timeout(Duration::from_secs(1))
            .unwrap()
            .is_err()
    );
    fixture.assert_released();
}

#[test]
fn authentication_fragmented_hello_cannot_extend_the_absolute_deadline() {
    let fixture = Fixture::new(Duration::from_millis(300));
    let mut socket = fixture.connect();
    socket
        .send(Message::Frame(Frame::message(
            vec![b'{'],
            OpCode::Data(Data::Text),
            false,
        )))
        .unwrap();
    let started = Instant::now();
    while socket
        .send(Message::Frame(Frame::message(
            vec![b' '],
            OpCode::Data(Data::Continue),
            false,
        )))
        .is_ok()
    {
        if let Ok(outcome) = fixture.result.try_recv() {
            assert!(outcome.is_err());
            fixture.assert_released();
            return;
        }
        assert!(
            started.elapsed() < Duration::from_secs(2),
            "fragment trickle kept authentication alive"
        );
        thread::sleep(Duration::from_millis(15));
    }
    assert!(
        fixture
            .result
            .recv_timeout(Duration::from_secs(1))
            .unwrap()
            .is_err()
    );
    fixture.assert_released();
}

#[test]
fn authentication_slow_http_upgrade_cannot_extend_the_absolute_deadline() {
    let fixture = Fixture::new(Duration::from_millis(300));
    let mut stream = TcpStream::connect(fixture.address).unwrap();
    let started = Instant::now();
    for byte in b"GET /v1 HTTP/1.1\r\nHost: fixture\r\nUpgrade: websocket\r\n" {
        if stream.write_all(&[*byte]).is_err() {
            break;
        }
        thread::sleep(Duration::from_millis(15));
    }
    assert!(
        fixture
            .result
            .recv_timeout(Duration::from_secs(1))
            .unwrap()
            .is_err()
    );
    assert!(started.elapsed() < Duration::from_secs(2));
    fixture.assert_released();
}

#[test]
fn authentication_rejection_releases_capacity() {
    let fixture = Fixture::new(Duration::from_secs(2));
    let mut socket = fixture.connect();
    send_hello(&mut socket, "wrong-token");
    let response: ServerMessage =
        serde_json::from_str(socket.read().unwrap().to_text().unwrap()).unwrap();
    assert!(matches!(response, ServerMessage::Rejected { .. }));
    fixture
        .result
        .recv_timeout(Duration::from_secs(1))
        .unwrap()
        .unwrap();
    fixture.assert_released();
}

#[test]
fn authenticated_connection_releases_pending_capacity_and_outlives_deadline() {
    let fixture = Fixture::new(Duration::from_millis(400));
    let mut socket = fixture.connect();
    send_hello(&mut socket, "fixture-token");
    let response: ServerMessage =
        serde_json::from_str(socket.read().unwrap().to_text().unwrap()).unwrap();
    assert!(matches!(response, ServerMessage::Hello { .. }));
    let deadline = Instant::now() + Duration::from_secs(1);
    while fixture.pending.load(Ordering::Acquire) != 0 && Instant::now() < deadline {
        thread::yield_now();
    }
    assert_eq!(fixture.pending.load(Ordering::Acquire), 0);
    assert_eq!(fixture.active.load(Ordering::Acquire), 1);
    thread::sleep(Duration::from_millis(450));
    socket.send(Message::Ping(Vec::new().into())).unwrap();
    assert!(matches!(socket.read().unwrap(), Message::Pong(_)));
    socket.close(None).unwrap();
    fixture
        .result
        .recv_timeout(Duration::from_secs(1))
        .unwrap()
        .unwrap();
    fixture.assert_released();
}

#[test]
fn authentication_shutdown_interrupts_an_idle_handshake() {
    let fixture = Fixture::new(Duration::from_secs(20));
    let _stream = TcpStream::connect(fixture.address).unwrap();
    fixture.shutdown.store(true, Ordering::Release);
    assert!(
        fixture
            .result
            .recv_timeout(Duration::from_millis(500))
            .unwrap()
            .is_err()
    );
    fixture.assert_released();
}

#[test]
fn authentication_pending_and_authenticated_limits_are_independent() {
    let pending = Arc::new(AtomicUsize::new(0));
    let active = Arc::new(AtomicUsize::new(0));
    let pending_permits = (0..MAX_PENDING_AUTHENTICATIONS)
        .map(|_| ConnectionPermit::acquire(&pending, MAX_PENDING_AUTHENTICATIONS).unwrap())
        .collect::<Vec<_>>();
    assert!(ConnectionPermit::acquire(&pending, MAX_PENDING_AUTHENTICATIONS).is_none());
    let authenticated_permits = (0..MAX_CONNECTIONS)
        .map(|_| ConnectionPermit::acquire(&active, MAX_CONNECTIONS).unwrap())
        .collect::<Vec<_>>();
    assert!(ConnectionPermit::acquire(&active, MAX_CONNECTIONS).is_none());
    drop(pending_permits);
    assert_eq!(pending.load(Ordering::Acquire), 0);
    assert_eq!(active.load(Ordering::Acquire), MAX_CONNECTIONS);
    drop(authenticated_permits);
    assert_eq!(active.load(Ordering::Acquire), 0);
}

#[test]
fn authentication_deadline_also_bounds_a_peer_that_stops_reading() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let _peer = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (socket, _) = listener.accept().unwrap();
    let started = Instant::now();
    let mut stream = HandshakeStream::new(
        socket,
        started + Duration::from_millis(300),
        Arc::new(AtomicBool::new(false)),
    );
    let buffer = vec![0_u8; 128 * 1024];
    let failure = loop {
        if let Err(error) = stream.write_all(&buffer) {
            break error;
        }
        assert!(started.elapsed() < Duration::from_secs(2));
    };
    assert_eq!(failure.kind(), io::ErrorKind::TimedOut);
    assert!(started.elapsed() < Duration::from_secs(2));
}
