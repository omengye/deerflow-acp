use super::*;
use crate::protocol::WireDriverStartOptions;
use serde_json::json;
use std::path::PathBuf;
use std::time::Instant;
use waku_client::DaemonClient;

fn page(
    hub: &Hub,
    session: Uuid,
    runtime: Uuid,
    epoch: Option<Uuid>,
    after: u64,
    through: Option<u64>,
    limit: usize,
) -> (Uuid, Vec<SequencedEvent>, u64, u64, bool) {
    let ResponsePayload::ReplayEvents {
        epoch,
        events,
        through_sequence,
        next_sequence,
        has_more,
    } = hub
        .replay_page(session, runtime, epoch, after, through, limit)
        .unwrap()
    else {
        panic!("expected replay page");
    };
    (epoch, events, through_sequence, next_sequence, has_more)
}

#[test]
fn replay_pages_cover_every_event_beyond_the_old_tail_and_isolate_new_live_events() {
    let hub = Arc::new(Hub::default());
    let session = Uuid::new_v4();
    let runtime = Uuid::new_v4();
    hub.begin_runtime(session, runtime);
    let sink = hub.event_sink(session, runtime);
    for n in 1..=2051 {
        sink.send(WireDriverEvent::new("textDelta", json!(n)))
            .unwrap();
    }
    let (epoch, first, through, mut next, mut more) =
        page(&hub, session, runtime, Some(hub.epoch), 1, None, 256);
    assert_eq!(through, 2051);
    assert_eq!(first[0].sequence, 2);
    let mut recovered = first;
    // Appends during pagination belong after the fixed catch-up watermark.
    sink.send(WireDriverEvent::new("textDelta", json!(2052)))
        .unwrap();
    while more {
        let (_, events, fixed, end, has_more) = page(
            &hub,
            session,
            runtime,
            Some(epoch),
            next,
            Some(through),
            256,
        );
        assert_eq!(fixed, through);
        recovered.extend(events);
        next = end;
        more = has_more;
    }
    assert_eq!(recovered.len(), 2050);
    assert_eq!(next, 2051);
    for (offset, event) in recovered.iter().enumerate() {
        assert_eq!(event.sequence, offset as u64 + 2);
    }
    assert_eq!(
        page(&hub, session, runtime, Some(epoch), next, None, 256).1[0].sequence,
        2052
    );
}

#[test]
fn replay_subscription_waits_for_explicit_pages_and_preserves_ephemeral_holes() {
    let hub = Arc::new(Hub::default());
    let session = Uuid::new_v4();
    let runtime = Uuid::new_v4();
    hub.begin_runtime(session, runtime);
    let sink = hub.event_sink(session, runtime);
    sink.send(WireDriverEvent::new("textDelta", json!("a")))
        .unwrap();
    sink.send_ephemeral(WireDriverEvent::new("terminalOutput", json!("transient")))
        .unwrap();
    sink.send(WireDriverEvent::new("textDelta", json!("b")))
        .unwrap();
    sink.send_ephemeral(WireDriverEvent::new("terminalOutput", json!("transient")))
        .unwrap();
    let (tx, rx) = bounded(1);
    hub.subscribe(&[], Subscriber::new(tx).0);
    assert!(
        rx.try_recv().is_err(),
        "handshake must not push history into an unsubscribed client"
    );
    let (_, events, through, next, more) = page(&hub, session, runtime, None, 0, None, 1);
    assert_eq!(through, 4);
    assert_eq!(events[0].sequence, 1);
    assert_eq!(next, 1);
    assert!(more);
    let (_, events, _, next, more) = page(
        &hub,
        session,
        runtime,
        Some(hub.epoch),
        next,
        Some(through),
        1,
    );
    assert_eq!(events[0].sequence, 3);
    assert_eq!(next, 4);
    assert!(!more);
}

#[test]
fn replay_cursor_rejects_wrong_runtime_epoch_and_out_of_range_watermarks() {
    let hub = Arc::new(Hub::default());
    let session = Uuid::new_v4();
    let runtime = Uuid::new_v4();
    assert_eq!(page(&hub, session, runtime, None, 0, None, 256).3, 0);
    hub.begin_runtime(session, runtime);
    hub.event_sink(session, runtime)
        .send(WireDriverEvent::new("textDelta", json!("a")))
        .unwrap();
    assert!(
        hub.replay_page(session, Uuid::new_v4(), None, 0, None, 256)
            .is_err()
    );
    assert!(
        hub.replay_page(session, runtime, Some(Uuid::new_v4()), 0, Some(1), 256)
            .is_err()
    );
    assert!(
        hub.replay_page(session, runtime, Some(hub.epoch), 0, Some(2), 256)
            .is_err()
    );
    assert!(
        hub.replay_page(session, runtime, Some(hub.epoch), 2, None, 256)
            .is_err()
    );
    for limit in [0, 257] {
        assert!(
            hub.replay_page(session, runtime, None, 0, None, limit)
                .is_err()
        );
    }
    hub.end_runtime(session, Some(runtime));
    assert!(
        hub.replay_page(session, runtime, Some(hub.epoch), 0, None, 256)
            .is_err()
    );
}

#[test]
fn terminal_live_events_are_marked_ephemeral_even_before_the_initial_replay_page() {
    let hub = Arc::new(Hub::default());
    let session = Uuid::new_v4();
    let runtime = Uuid::new_v4();
    let (tx, rx) = bounded(4);
    hub.subscribe(&[], Subscriber::new(tx).0);
    hub.begin_runtime(session, runtime);
    let sink = hub.event_sink(session, runtime);
    // A shell can emit output and exit while its subscriber's first replay RPC
    // is still in flight. These live-only events must survive an empty page
    // whose watermark already includes their sequence numbers.
    sink.send_ephemeral(WireDriverEvent::new("terminalOutput", json!("ready")))
        .unwrap();
    sink.send_ephemeral(WireDriverEvent::new("terminalExited", json!(0)))
        .unwrap();
    for expected in 1..=2 {
        let ServerMessage::Event(event) = rx.try_recv().unwrap() else {
            panic!("expected terminal event");
        };
        assert_eq!(event.sequence, expected);
        assert!(!event.replayable);
    }
    let (_, events, through, next, more) = page(&hub, session, runtime, None, 0, None, 256);
    assert!(events.is_empty());
    assert_eq!((through, next, more), (2, 2, false));
}

#[derive(Default)]
struct ReplayBackend {
    sink: Mutex<Option<EventSink>>,
}

impl Backend for ReplayBackend {
    fn handle(&self, request: Request, events: EventSink) -> anyhow::Result<ResponsePayload> {
        match request.command {
            Command::Start { .. } => {
                *self.sink.lock() = Some(events.clone());
                events.send(WireDriverEvent::new("textDelta", json!("anchor")))?;
                Ok(ResponsePayload::Started {
                    supports_steer: false,
                })
            }
            Command::ReplayEvents { .. } => panic!("replay must never enter the provider backend"),
            _ => Ok(ResponsePayload::Ack),
        }
    }
}

#[test]
fn disconnected_client_recovers_beyond_old_replay_and_queue_limits_with_live_events() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap().to_string();
    let backend = Arc::new(ReplayBackend::default());
    let shutdown = Arc::new(AtomicBool::new(false));
    let worker = {
        let backend = backend.clone();
        let shutdown = shutdown.clone();
        std::thread::spawn(move || {
            serve(
                listener,
                "replay-fixture".into(),
                backend,
                shutdown,
                ServerOptions::default(),
            )
            .unwrap()
        })
    };
    let session = Uuid::new_v4();
    let runtime = Uuid::new_v4();
    let first = DaemonClient::connect(&address, "replay-fixture".into()).unwrap();
    first
        .request(
            session,
            runtime,
            Command::Start {
                options: WireDriverStartOptions {
                    provider: "deerflow".into(),
                    binary: PathBuf::from("unused-fixture"),
                    cwd: PathBuf::from("."),
                    mode: "default".into(),
                    model: None,
                    reasoning_effort: None,
                    thinking_mode: None,
                    service_tier: None,
                    context_window: None,
                    agent_preset: None,
                    computer_use_enabled: false,
                    provider_cursor: None,
                },
            },
        )
        .unwrap();
    let initial = first.subscribe(session, runtime);
    assert_eq!(
        initial
            .recv_timeout(Duration::from_secs(5))
            .unwrap()
            .sequence,
        1
    );
    let cursors = first.last_sequences();
    first.disconnect();
    let disconnect_deadline = Instant::now() + Duration::from_secs(2);
    while !first.is_disconnected() && Instant::now() < disconnect_deadline {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert!(first.is_disconnected());
    let sink = backend.sink.lock().clone().unwrap();
    // Exceed both the old 2048-event journal and the 16384-message outgoing
    // queue without allocating either complete history in memory.
    for n in 2..=20051 {
        sink.send(WireDriverEvent::new("textDelta", json!(n)))
            .unwrap();
    }
    let resumed =
        DaemonClient::connect_with_resume(&address, "replay-fixture".into(), cursors).unwrap();
    let events = resumed.subscribe(session, runtime);
    // These may arrive either inside the initial watermark or in the bounded
    // live buffer while its pages are in flight. Both paths must agree.
    for n in 20052..=20551 {
        sink.send(WireDriverEvent::new("textDelta", json!(n)))
            .unwrap();
    }
    for expected in 2..=20551 {
        assert_eq!(
            events
                .recv_timeout(Duration::from_secs(5))
                .unwrap()
                .sequence,
            expected
        );
    }
    sink.send(WireDriverEvent::new("textDelta", json!(20552)))
        .unwrap();
    assert_eq!(
        events
            .recv_timeout(Duration::from_secs(5))
            .unwrap()
            .sequence,
        20552
    );
    assert!(events.recv_timeout(Duration::from_millis(100)).is_err());
    assert!(!resumed.is_disconnected());
    assert_eq!(resumed.last_sequences()[0].sequence, 20552);
    resumed.disconnect();
    shutdown.store(true, Ordering::Release);
    worker.join().unwrap();
}
