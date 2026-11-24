//! Durable current-run intervention. It never submits a second ACP prompt.
use super::*;
use waku_protocol::model::SteeringUpdate;

#[derive(Default)]
pub(super) struct Availability {
    pub(super) supported: bool,
    pub(super) run_id: Option<String>,
}

#[derive(Default)]
pub(super) struct State {
    session: String,
    seen: HashMap<String, String>,
    // A pending foreground prompt masks automatic work until its own target arrives.
    foreground: Option<ForegroundPrompt>,
}

struct ForegroundPrompt {
    id: String,
    run: Option<String>,
}

pub(super) fn supported(initialize: &InitializeResponse) -> bool {
    let value = serde_json::to_value(initialize).unwrap_or(Value::Null);
    let caps = &value["_meta"]["deerflow"]["steering"];
    caps["version"] == 1
        && caps["submitMethod"] == "_deerflow/steering/submit"
        && caps["listMethod"] == "_deerflow/steering/list"
        && caps["updatedNotification"] == "_deerflow/steering/updated"
}

impl State {
    pub(super) fn new(session: &str) -> Self {
        Self {
            session: session.into(),
            ..Self::default()
        }
    }
    pub(super) fn begin_prompt(&mut self) -> String {
        let id = uuid::Uuid::new_v4().to_string();
        self.foreground = Some(ForegroundPrompt {
            id: id.clone(),
            run: None,
        });
        id
    }

    pub(super) fn bind(&mut self, value: &Value) {
        if let Some(prompt) = &mut self.foreground
            && value["sessionId"] == self.session
            && value["promptId"] == prompt.id
            && prompt.run.is_none()
            && let Some(run) = value["runId"]
                .as_str()
                .filter(|run| !run.is_empty() && run.len() <= 256)
        {
            prompt.run = Some(run.to_owned());
        }
    }

    pub(super) fn finish_prompt(&mut self, prompt_id: &str) {
        if self
            .foreground
            .as_ref()
            .is_some_and(|prompt| prompt.id == prompt_id)
        {
            self.foreground = None;
        }
    }
    pub(super) fn receive(&mut self, value: &Value, events: &impl DriverEventSink) {
        if value["sessionId"] != self.session {
            return;
        }
        let (Some(id), Some(run), Some(status), Some(text)) = (
            value["requestId"].as_str(),
            value["runId"].as_str(),
            value["status"].as_str(),
            value["text"].as_str(),
        ) else {
            return;
        };
        if uuid::Uuid::parse_str(id).is_err()
            || run.is_empty()
            || run.len() > 256
            || text.len() > 65536
            || !matches!(status, "accepted" | "applied" | "rejected")
        {
            return;
        }
        if self.seen.get(id).is_some_and(|previous| {
            previous == status || matches!(previous.as_str(), "applied" | "rejected")
        }) {
            return;
        }
        self.seen.insert(id.into(), status.into());
        let _ = events.send(DriverEvent::SteeringUpdated(SteeringUpdate {
            request_id: id.into(),
            run_id: run.into(),
            status: status.into(),
            message: text.into(),
            reason: value["reason"].as_str().unwrap_or_default().into(),
        }));
    }
}

pub(super) fn publish_target(stream: &AcpStreamState, events: &impl DriverEventSink) {
    let Some(state) = &stream.deerflow_steering else {
        return;
    };
    let run = match &state.foreground {
        Some(prompt) => prompt.run.as_deref(),
        None => stream
            .deerflow_tasks
            .as_ref()
            .and_then(|tasks| tasks.steering_target()),
    };
    let mut published = stream.steering_availability.lock();
    if published.run_id.as_deref() != run {
        published.run_id = run.map(str::to_owned);
        let _ = events.send(DriverEvent::SteeringCapability {
            supported: true,
            run_id: published.run_id.clone(),
        });
    }
}

pub(super) fn receive_target(
    stream: &mut AcpStreamState,
    value: &Value,
    events: &impl DriverEventSink,
) {
    if let Some(state) = &mut stream.deerflow_steering {
        state.bind(value);
        publish_target(stream, events);
    }
}

pub(super) fn restore(
    connection: &ConnectionTo<Agent>,
    session: &str,
    stream: Arc<Mutex<AcpStreamState>>,
    events: DriverEventSender,
) {
    let Ok(request) = UntypedMessage::new("_deerflow/steering/list", json!({"sessionId":session}))
    else {
        return;
    };
    let _ = connection
        .send_request(request)
        .on_receiving_result(async move |reply| {
            if let Ok(value) = reply {
                if let Some(items) = value["items"].as_array() {
                    let mut state = stream.lock();
                    if let Some(steering) = &mut state.deerflow_steering {
                        for item in items.iter().take(64) {
                            steering.receive(item, &events);
                        }
                    }
                }
            }
            Ok(())
        });
}

pub(super) fn submit(
    connection: &ConnectionTo<Agent>,
    session: &str,
    text: String,
    requested_run: Option<String>,
    stream: Arc<Mutex<AcpStreamState>>,
    events: &DriverEventSender,
) {
    let id = uuid::Uuid::new_v4().to_string();
    let active_run = stream.lock().steering_availability.lock().run_id.clone();
    let run = match requested_run {
        Some(run) if active_run.as_ref() == Some(&run) => run,
        expired => {
            let _ = events.send(DriverEvent::SteeringUpdated(SteeringUpdate {
                request_id: id,
                run_id: expired.unwrap_or_default(),
                status: "rejected".into(),
                message: text,
                reason: "The current run is no longer available for intervention.".into(),
            }));
            return;
        }
    };
    let pending = SteeringUpdate {
        request_id: id.clone(),
        run_id: run.clone(),
        status: "pending".into(),
        message: text.clone(),
        reason: String::new(),
    };
    let _ = events.send(DriverEvent::SteeringUpdated(pending.clone()));
    // Refresh the same run, never ask for whichever run is newest. Permission
    // checkpoints can advance its version while a long turn is still active.
    let request = UntypedMessage::new(
        "_deerflow/executions/get",
        json!({"sessionId":session,"runId":run}),
    );
    let Ok(request) = request else {
        return;
    };
    let callback_events = events.clone();
    let fallback = pending.clone();
    let follow = connection.clone();
    let session = session.to_owned();
    let registered = connection
        .send_request(request)
        .on_receiving_result(async move |reply| {
            match reply {
                Ok(value) => {
                    if value["sessionId"] == session && value["runId"] == pending.run_id {
                        if let Some(version) =
                            value["version"].as_u64().filter(|version| *version > 0)
                        {
                            dispatch(
                                &follow,
                                &session,
                                version,
                                pending,
                                stream,
                                &callback_events,
                            );
                        } else {
                            unknown(
                                &callback_events,
                                pending,
                                "The run version could not be verified.",
                            );
                        }
                    } else {
                        unknown(
                            &callback_events,
                            pending,
                            "The run identity could not be verified.",
                        );
                    }
                }
                Err(error) => {
                    let confirmed = stream
                        .lock()
                        .deerflow_steering
                        .as_ref()
                        .is_some_and(|state| state.seen.contains_key(&pending.request_id));
                    if !confirmed {
                        let _ =
                            callback_events.send(DriverEvent::SteeringUpdated(SteeringUpdate {
                                status: "unknown".into(),
                                reason: format!("Submission was not confirmed: {error}"),
                                ..pending
                            }));
                    }
                }
            }
            Ok(())
        });
    if let Err(error) = registered {
        let _ = events.send(DriverEvent::SteeringUpdated(SteeringUpdate {
            status: "unknown".into(),
            reason: error.to_string(),
            ..fallback
        }));
    }
}

fn unknown(events: &DriverEventSender, pending: SteeringUpdate, reason: &str) {
    let _ = events.send(DriverEvent::SteeringUpdated(SteeringUpdate {
        status: "unknown".into(),
        reason: reason.into(),
        ..pending
    }));
}

fn dispatch(
    connection: &ConnectionTo<Agent>,
    session: &str,
    version: u64,
    pending: SteeringUpdate,
    stream: Arc<Mutex<AcpStreamState>>,
    events: &DriverEventSender,
) {
    let Ok(request) = UntypedMessage::new(
        "_deerflow/steering/submit",
        json!({"sessionId":session,"runId":pending.run_id,"expectedVersion":version,"requestId":pending.request_id,"text":pending.message}),
    ) else {
        return;
    };
    let callback_events = events.clone();
    let fallback = pending.clone();
    let session = session.to_owned();
    let registered = connection
        .send_request(request)
        .on_receiving_result(async move |reply| {
            match reply {
                Ok(value) => {
                    let mut state = stream.lock();
                    let confirmed = state
                        .deerflow_steering
                        .as_ref()
                        .is_some_and(|state| state.seen.contains_key(&pending.request_id));
                    if value["sessionId"] == session
                        && value["requestId"] == pending.request_id
                        && value["runId"] == pending.run_id
                        && value["text"] == pending.message
                        && matches!(
                            value["status"].as_str(),
                            Some("accepted" | "applied" | "rejected")
                        )
                    {
                        if let Some(state) = &mut state.deerflow_steering {
                            state.receive(&value, &callback_events);
                        }
                    } else if !confirmed {
                        unknown(
                            &callback_events,
                            pending,
                            "The intervention response could not be matched to this request.",
                        );
                    }
                }
                Err(error) => {
                    let confirmed = stream
                        .lock()
                        .deerflow_steering
                        .as_ref()
                        .is_some_and(|state| state.seen.contains_key(&pending.request_id));
                    if !confirmed {
                        unknown(
                            &callback_events,
                            pending,
                            &format!("Submission was not confirmed: {error}"),
                        );
                    }
                }
            }
            Ok(())
        });
    if let Err(error) = registered {
        unknown(events, fallback, &error.to_string());
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn live_stream() -> Arc<Mutex<AcpStreamState>> {
        let mut stream = AcpStreamState {
            deerflow_steering: Some(State::new("session")),
            ..Default::default()
        };
        bind_test_prompt(&mut stream, "original");
        Arc::new(Mutex::new(stream))
    }

    fn bind_test_prompt(stream: &mut AcpStreamState, run: &str) {
        let state = stream.deerflow_steering.as_mut().unwrap();
        let id = state.begin_prompt();
        state.bind(&json!({"sessionId":"session","promptId":id,"runId":run}));
        publish_target(stream, &crossbeam_channel::unbounded().0);
    }

    async fn next_update(
        received: &crossbeam_channel::Receiver<DriverEvent>,
    ) -> agent_client_protocol::Result<SteeringUpdate> {
        smol::future::race(
            async {
                loop {
                    match received.try_recv() {
                        Ok(DriverEvent::SteeringUpdated(update)) => return Ok(update),
                        Ok(other) => panic!("intervention produced an unrelated event: {other:?}"),
                        Err(_) => {
                            smol::Timer::after(Duration::from_millis(2)).await;
                        }
                    }
                }
            },
            async {
                smol::Timer::after(Duration::from_secs(5)).await;
                Err(approval_error("intervention fixture timed out"))
            },
        )
        .await
    }

    async fn response_barrier(
        connection: &ConnectionTo<Agent>,
    ) -> agent_client_protocol::Result<()> {
        let (sent, received) = smol::channel::bounded(1);
        connection
            .send_request(UntypedMessage::new("_fixture/barrier", json!({}))?)
            .on_receiving_result(async move |result| {
                result?;
                sent.send(())
                    .await
                    .map_err(|_| approval_error("fixture barrier closed"))
            })?;
        smol::future::race(
            async {
                received
                    .recv()
                    .await
                    .map_err(|_| approval_error("fixture barrier closed"))
            },
            async {
                smol::Timer::after(Duration::from_secs(5)).await;
                Err(approval_error("fixture barrier timed out"))
            },
        )
        .await
    }

    #[test]
    fn foreground_prompt_carries_a_distinct_steering_identity_on_every_turn() {
        let observed = Arc::new(Mutex::new(Vec::new()));
        let prompts = observed.clone();
        let agent = Agent.builder().on_receive_request(
            async move |request: UntypedMessage, responder, connection| {
                match request.method() {
                    "session/prompt" => {
                        let id = request.params()["_meta"]["deerflow"]["promptId"].clone();
                        let text = request.params()["prompt"][0]["text"].as_str().unwrap();
                        prompts.lock().push(id.clone());
                        connection.send_notification(UntypedMessage::new(
                            "_deerflow/steering/target",
                            json!({"sessionId":"session","promptId":id,"runId":format!("run-{text}")}),
                        )?)?;
                        responder.respond(json!({"stopReason":"end_turn"}))?;
                    }
                    "_fixture/barrier" => responder.respond(json!({}))?,
                    other => panic!("unexpected request: {other}"),
                }
                Ok(Handled::Yes)
            },
            agent_client_protocol::on_receive_request!(),
        );
        let (events, received) = crate::driver::test_event_channel();
        let stream = Arc::new(Mutex::new(AcpStreamState {
            deerflow_steering: Some(State::new("session")),
            ..Default::default()
        }));
        let target_stream = stream.clone();
        let target_events = events.clone();
        let client = Client.builder().on_receive_notification(
            async move |notice: UntypedMessage, _| {
                assert_eq!(notice.method(), "_deerflow/steering/target");
                receive_target(&mut target_stream.lock(), notice.params(), &target_events);
                Ok(())
            },
            agent_client_protocol::on_receive_notification!(),
        );
        smol::block_on(client.connect_with(agent, async move |connection| {
            let pending = Arc::new(Mutex::new(PendingPrompts::default()));
            for text in ["first", "second"] {
                send_prompt(
                    &connection,
                    &SessionId::new("session"),
                    text.into(),
                    &[],
                    &pending,
                    &events,
                    "session",
                    stream.clone(),
                )?;
                response_barrier(&connection).await?;
            }
            Ok(())
        }))
        .unwrap();
        let prompts = observed.lock();
        assert_eq!(prompts.len(), 2);
        assert!(
            prompts.iter().all(|id| id
                .as_str()
                .is_some_and(|s| uuid::Uuid::parse_str(s).is_ok())),
            "foreground prompts have no steering identity: {prompts:?}"
        );
        assert_ne!(prompts[0], prompts[1]);
        let targets: Vec<_> = received
            .try_iter()
            .filter_map(|event| match event {
                DriverEvent::SteeringCapability { run_id, .. } => Some(run_id),
                _ => None,
            })
            .collect();
        assert_eq!(
            targets,
            [
                Some("run-first".into()),
                None,
                Some("run-second".into()),
                None
            ]
        );
    }

    #[test]
    fn late_targets_and_steers_cannot_bind_to_the_following_prompt() {
        let stream = live_stream();
        let (events, received) = crate::driver::test_event_channel();
        {
            let mut stream = stream.lock();
            let state = stream.deerflow_steering.as_mut().unwrap();
            let old = state.foreground.as_ref().unwrap().id.clone();
            let current = state.begin_prompt();
            state.finish_prompt(&old);
            for (session, id) in [("session", &old), ("another-session", &current)] {
                receive_target(
                    &mut stream,
                    &json!({"sessionId":session,"promptId":id,"runId":"wrong"}),
                    &events,
                );
                assert!(stream.steering_availability.lock().run_id.is_none());
            }
            let target = json!({"sessionId":"session","promptId":current,"runId":"second"});
            receive_target(&mut stream, &target, &events);
            receive_target(&mut stream, &target, &events);
            assert_eq!(
                stream.steering_availability.lock().run_id.as_deref(),
                Some("second")
            );
        }
        assert_eq!(received.try_iter().count(), 2);
        smol::block_on(
            Client
                .builder()
                .connect_with(Agent.builder(), async move |connection| {
                    submit(
                        &connection,
                        "session",
                        "keep the draft".into(),
                        Some("original".into()),
                        stream,
                        &events,
                    );
                    let rejected = next_update(&received).await?;
                    assert_eq!(
                        (
                            rejected.status.as_str(),
                            rejected.run_id.as_str(),
                            rejected.message.as_str()
                        ),
                        ("rejected", "original", "keep the draft")
                    );
                    Ok(())
                }),
        )
        .unwrap();
    }

    #[test]
    fn steering_live_refreshes_same_run_and_applied_notification_wins_accepted_reply() {
        let captured = Arc::new(Mutex::new(Vec::new()));
        let requests = captured.clone();
        let agent = Agent.builder().on_receive_request(async move |request: UntypedMessage, responder, connection| {
            requests.lock().push((request.method().to_owned(), request.params().clone()));
            match request.method() {
                "_deerflow/executions/get" => responder.respond(json!({"sessionId":"session","runId":"original","version":9,"status":"waiting_input"}))?,
                "_deerflow/steering/submit" => {
                    assert_eq!(request.params()["runId"], "original");
                    assert_eq!(request.params()["expectedVersion"], 9);
                    let mut record = json!({"sessionId":"session","runId":"original","requestId":request.params()["requestId"],"text":request.params()["text"],"status":"applied"});
                    connection.send_notification(UntypedMessage::new("_deerflow/steering/updated", record.clone())?)?;
                    record["status"] = json!("accepted");
                    responder.respond(record)?;
                }
                "_fixture/barrier" => responder.respond(json!({}))?,
                other => panic!("unexpected intervention method: {other}"),
            }
            Ok(Handled::Yes)
        }, agent_client_protocol::on_receive_request!());
        let stream = live_stream();
        let (events, received) = crate::driver::test_event_channel();
        let client = Client.builder().on_receive_notification(
            {
                let stream = stream.clone();
                let events = events.clone();
                async move |notification: UntypedMessage, _| {
                    assert_eq!(notification.method(), "_deerflow/steering/updated");
                    stream
                        .lock()
                        .deerflow_steering
                        .as_mut()
                        .unwrap()
                        .receive(notification.params(), &events);
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_notification!(),
        );
        smol::block_on(client.connect_with(agent, async move |connection| {
            submit(
                &connection,
                "session",
                "keep the change small".into(),
                Some("original".into()),
                stream.clone(),
                &events,
            );
            let pending = next_update(&received).await?;
            let applied = next_update(&received).await?;
            assert_eq!(pending.status, "pending");
            assert_eq!(applied.status, "applied");
            assert_eq!(pending.request_id, applied.request_id);
            assert_eq!(applied.message, "keep the change small");
            response_barrier(&connection).await?;
            assert!(
                received.try_recv().is_err(),
                "late accepted reply regressed applied"
            );
            assert_eq!(
                stream.lock().deerflow_steering.as_ref().unwrap().seen[&pending.request_id],
                "applied"
            );
            Ok(())
        }))
        .unwrap();
        let requests = captured.lock();
        assert_eq!(requests[0].0, "_deerflow/executions/get");
        assert_eq!(
            requests[0].1,
            json!({"sessionId":"session","runId":"original"})
        );
        assert_eq!(requests[1].0, "_deerflow/steering/submit");
        assert_eq!(requests.len(), 3);
        assert!(
            requests
                .iter()
                .all(|(method, _)| method != "session/prompt")
        );
    }

    #[test]
    fn steering_live_expired_run_is_rejected_without_retargeting_new_run() {
        let captured = Arc::new(Mutex::new(Vec::new()));
        let requests = captured.clone();
        let agent = Agent.builder().on_receive_request(async move |request: UntypedMessage, responder, connection| {
            requests.lock().push((request.method().to_owned(), request.params().clone()));
            match request.method() {
                "_deerflow/executions/get" => {
                    connection.send_notification(UntypedMessage::new("_fixture/nextPrompt", json!({"runId":"new-run"}))?)?;
                    responder.respond(json!({"sessionId":"session","runId":"original","version":11,"status":"completed"}))?;
                }
                "_deerflow/steering/submit" => {
                    assert_eq!(request.params()["runId"], "original");
                    assert_eq!(request.params()["expectedVersion"], 11);
                    responder.respond(json!({"sessionId":"session","runId":"original","requestId":request.params()["requestId"],"text":request.params()["text"],"status":"rejected","reason":"execution_ended"}))?;
                }
                other => panic!("unexpected intervention method: {other}"),
            }
            Ok(Handled::Yes)
        }, agent_client_protocol::on_receive_request!());
        let stream = live_stream();
        let (events, received) = crate::driver::test_event_channel();
        let client = Client.builder().on_receive_notification(
            {
                let stream = stream.clone();
                async move |notification: UntypedMessage, _| {
                    bind_test_prompt(
                        &mut stream.lock(),
                        notification.params()["runId"].as_str().unwrap(),
                    );
                    Ok(())
                }
            },
            agent_client_protocol::on_receive_notification!(),
        );
        smol::block_on(client.connect_with(agent, async move |connection| {
            submit(
                &connection,
                "session",
                "preserve this text".into(),
                Some("original".into()),
                stream.clone(),
                &events,
            );
            let pending = next_update(&received).await?;
            let rejected = next_update(&received).await?;
            assert_eq!(pending.status, "pending");
            assert_eq!(rejected.status, "rejected");
            assert_eq!(rejected.reason, "execution_ended");
            assert_eq!(rejected.run_id, "original");
            assert_eq!(rejected.message, pending.message);
            assert_eq!(rejected.request_id, pending.request_id);
            assert_eq!(
                stream.lock().steering_availability.lock().run_id.as_deref(),
                Some("new-run")
            );
            Ok(())
        }))
        .unwrap();
        let requests = captured.lock();
        assert_eq!(requests.len(), 2);
        assert!(requests.iter().all(|(method, params)| method != "session/prompt" && params["runId"] == "original"));
    }

    #[test]
    fn steering_live_unknown_response_restores_by_request_id_without_resubmission() {
        for mismatched in [false, true] {
            let captured = Arc::new(Mutex::new(Vec::new()));
            let requests = captured.clone();
            let durable = Arc::new(Mutex::new(Value::Null));
            let saved = durable.clone();
            let agent = Agent.builder().on_receive_request(async move |request: UntypedMessage, responder, _connection| {
                requests.lock().push((request.method().to_owned(), request.params().clone()));
                match request.method() {
                    "_deerflow/executions/get" => responder.respond(json!({"sessionId":"session","runId":"original","version":3,"status":"running"}))?,
                    "_deerflow/steering/submit" => {
                        let record = json!({"sessionId":"session","runId":"original","requestId":request.params()["requestId"],"text":request.params()["text"],"status":"applied"});
                        *saved.lock() = record.clone();
                        if mismatched {
                            let mut wrong = record;
                            wrong["runId"] = json!("another-run");
                            responder.respond(wrong)?;
                        } else {
                            responder.respond_with_internal_error("response delivery uncertain")?;
                        }
                    }
                    "_deerflow/steering/list" => {
                        assert_eq!(request.params(), &json!({"sessionId":"session"}));
                        responder.respond(json!({"sessionId":"session","items":[saved.lock().clone()]}))?;
                    }
                    other => panic!("unexpected intervention method: {other}"),
                }
                Ok(Handled::Yes)
            }, agent_client_protocol::on_receive_request!());
            let stream = live_stream();
            let (events, received) = crate::driver::test_event_channel();
            smol::block_on(
                Client
                    .builder()
                    .connect_with(agent, async move |connection| {
                        submit(
                            &connection,
                            "session",
                            "do not discard me".into(),
                            Some("original".into()),
                            stream.clone(),
                            &events,
                        );
                        let pending = next_update(&received).await?;
                        let unknown = next_update(&received).await?;
                        assert_eq!(pending.status, "pending");
                        assert_eq!(unknown.status, "unknown");
                        assert_eq!(unknown.request_id, pending.request_id);
                        assert_eq!(unknown.message, pending.message);
                        // A reattached stream starts with no in-memory confirmations.
                        stream.lock().deerflow_steering = Some(State::new("session"));
                        restore(&connection, "session", stream.clone(), events.clone());
                        let restored = next_update(&received).await?;
                        assert_eq!(restored.status, "applied");
                        assert_eq!(restored.request_id, pending.request_id);
                        assert_eq!(restored.run_id, "original");
                        assert_eq!(restored.message, pending.message);
                        Ok(())
                    }),
            )
            .unwrap();
            let requests = captured.lock();
            assert_eq!(requests.len(), 3);
            assert_eq!(requests[0].0, "_deerflow/executions/get");
            assert_eq!(requests[1].0, "_deerflow/steering/submit");
            assert_eq!(requests[2].0, "_deerflow/steering/list");
            assert!(
                requests
                    .iter()
                    .all(|(method, _)| method != "session/prompt")
            );
        }
    }

    #[test]
    fn intervention_updates_are_scoped_and_do_not_regress() {
        let (sender, receiver) = crossbeam_channel::unbounded();
        let mut state = State::new("s");
        let mut value = json!({"sessionId":"s","requestId":uuid::Uuid::new_v4().to_string(),"runId":"r","status":"applied","text":"change course"});
        state.receive(&value, &sender);
        value["status"] = json!("accepted");
        state.receive(&value, &sender);
        value["sessionId"] = json!("another");
        value["status"] = json!("rejected");
        state.receive(&value, &sender);
        assert_eq!(receiver.try_iter().count(), 1);
    }
}
