use super::*;
use std::net::{TcpListener, TcpStream};
use std::time::Duration;
use tungstenite::{Message, WebSocket};
use uuid::Uuid;
use waku_protocol::{
    ClientMessage, Command, Request, ResponseOutcome, ResponsePayload, ServerMessage,
};

fn handshake(listener: &TcpListener) -> WebSocket<TcpStream> {
    let (stream, _) = listener.accept().unwrap();
    stream
        .set_read_timeout(Some(Duration::from_secs(8)))
        .unwrap();
    let mut socket = tungstenite::accept(stream).unwrap();
    let hello: ClientMessage =
        serde_json::from_str(socket.read().unwrap().to_text().unwrap()).unwrap();
    assert!(matches!(hello, ClientMessage::Hello { .. }));
    send(
        &mut socket,
        ServerMessage::Hello {
            protocol_version: waku_protocol::PROTOCOL_VERSION,
            daemon_version: "fixture".into(),
        },
    );
    socket
}

fn send(socket: &mut WebSocket<TcpStream>, message: ServerMessage) {
    socket
        .send(Message::Text(
            serde_json::to_string(&message).unwrap().into(),
        ))
        .unwrap();
}

fn next_request(socket: &mut WebSocket<TcpStream>) -> Request {
    let msg = socket.read().unwrap();
    let ClientMessage::Request(req) = serde_json::from_str(msg.to_text().unwrap()).unwrap() else {
        panic!("expected a session request");
    };
    req
}

#[test]
fn local_attach_timeout_retries_same_socket_without_replaying_user_controls() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap().to_string();
    let session = Uuid::new_v4();
    let runtime = Uuid::new_v4();
    let server = std::thread::spawn(move || {
        // Lose the initial connection, but keep the same runtime in the fixture.
        drop(handshake(&listener));
        let mut replacement = handshake(&listener);
        let first = next_request(&mut replacement);
        assert!(matches!(first.command, Command::AttachSession));
        let delayed_reply = Instant::now() + Duration::from_secs(3);
        let retry = next_request(&mut replacement);
        assert!(matches!(retry.command, Command::AttachSession));
        assert_ne!(retry.request_id, first.request_id);
        // The socket is healthy, but this session remains unattached beyond the UI grace period.
        std::thread::sleep(delayed_reply.saturating_duration_since(Instant::now()));
        send(
            &mut replacement,
            ServerMessage::Response {
                request_id: retry.request_id,
                outcome: ResponseOutcome::Ok {
                    payload: ResponsePayload::SessionRuntime {
                        runtime_id: Some(runtime),
                        supports_steer: false,
                        steering_run_id: None,
                        conversation_capability: None,
                        deerflow_ready: None,
                        pending_history: None,
                    },
                },
            },
        );
        assert!(matches!(
            next_request(&mut replacement).command,
            Command::RefreshBackgroundWork
        ));
        assert!(
            matches!(next_request(&mut replacement).command, Command::Prompt { prompt, .. } if prompt == "after recovery")
        );
        assert!(matches!(replacement.read().unwrap(), Message::Close(_)));
    });

    let old = waku_client::DaemonClient::connect(&address, "fixture".into()).unwrap();
    let deadline = Instant::now() + Duration::from_secs(5);
    while !old.is_disconnected() {
        assert!(Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(10));
    }
    let new = waku_client::DaemonClient::connect(&address, "fixture".into()).unwrap();
    let (updates_tx, updates) = bounded(1);
    updates_tx.send(new.clone()).unwrap();
    let (shutdown, shutdown_rx) = bounded(1);
    let (wake, _wake_rx) = smol::channel::bounded(1);
    let (events, received) = event_channel(wake);
    let driver = RemoteDriverControl {
        recovery: Arc::new(SessionRecovery::new(true)),
        client: Arc::new(Mutex::new(old.clone())),
        session_id: session,
        runtime_id: runtime,
        supports_steer: Arc::new(AtomicBool::new(false)),
        events,
        closed: Arc::new(AtomicBool::new(false)),
        shutdown,
    };
    for _ in 0..6 {
        driver.refresh_background_work();
    }
    let recovery_start = driver.local_reconnecting_since().unwrap();
    assert!(recovery_start.elapsed() < Duration::from_secs(3));
    assert!(!driver.try_cancel());
    assert!(!driver.try_prompt_with_attachments("unsent".into(), None, None, vec![]));
    assert!(received.is_empty());
    let (attached, response) = reattach(
        &old,
        |candidate| candidate.same_connection(&new),
        &updates,
        &shutdown_rx,
        &driver.recovery,
        session,
        runtime,
    )
    .unwrap();
    assert!(
        matches!(response.unwrap(), ResponsePayload::SessionRuntime {runtime_id: Some(id), ..} if id == runtime)
    );
    assert_eq!(driver.local_reconnecting_since(), Some(recovery_start));
    assert!(recovery_start.elapsed() >= Duration::from_secs(3));
    driver
        .recovery
        .install(&driver.client, &attached, session, runtime);
    assert_eq!(driver.local_connection_generation(), 1);
    assert!(driver.local_reconnecting_since().is_none());
    assert!(driver.try_prompt_with_attachments("after recovery".into(), None, None, vec![]));
    new.disconnect();
    server.join().unwrap();
}
