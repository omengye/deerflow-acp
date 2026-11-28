use std::collections::HashSet;

use serde::Deserialize;
use serde_json::{Map, Value, json};

use super::{
    DriverEvent, DriverEventSink, Handled, PendingAcpUserInput,
    PendingAcpUserInputs, PendingPromptRequests, Responder, UserInputAnswer, UserInputOption,
    UserInputQuestion, cancelled_user_input_response,
};

pub(super) const METHOD: &str = "_deerflow/ask_user_question";

pub(super) fn capability() -> Map<String, Value> {
    Map::from_iter([(
        "deerflow".into(),
        json!({
            "userInput": {"version": 1, "requestMethod": METHOD},
            "mobileCredentials": 2
        }),
    )])
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Request {
    session_id: String,
    run_id: String,
    tool_call_id: String,
    questions: Vec<Question>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Question {
    id: String,
    #[serde(default)]
    header: String,
    question: String,
    #[serde(default)]
    options: Vec<UserInputOption>,
    #[serde(default)]
    multi_select: bool,
}

fn bounded(text: &str, max: usize, required: bool) -> bool {
    text.len() <= max && !text.contains('\0') && (!required || !text.trim().is_empty())
}

pub(super) fn receive(
    params: Value,
    responder: Responder<Value>,
    prompt_requests: &PendingPromptRequests,
    pending: &PendingAcpUserInputs,
    events: &impl DriverEventSink,
) -> agent_client_protocol::Result<Handled<(super::UntypedMessage, Responder<Value>)>> {
    let prompts = prompt_requests.lock();
    let parsed = params
        .get("sessionId")
        .and_then(Value::as_str)
        .filter(|session| prompts.accepts_user_input(session))
        .and_then(|_| questions(&params));
    let Some(questions) = parsed else {
        responder.respond(cancelled_user_input_response())?;
        return Ok(Handled::Yes);
    };
    // Keep insertion ordered with prompt completion, including timeout/error,
    // so a late callback cannot revive the panel after the owning turn ends.
    let mut inputs = pending.lock();
    if !inputs.is_empty() {
        responder.respond(cancelled_user_input_response())?;
        return Ok(Handled::Yes);
    }
    let request_id = responder.id().to_string();
    inputs.insert(
        request_id.clone(),
        PendingAcpUserInput {
            params,
            responder,
        },
    );
    if events
        .send(DriverEvent::UserInputRequested {
            request_id: request_id.clone(),
            questions,
        })
        .is_err()
        && let Some(input) = inputs.remove(&request_id)
    {
        let _ = input.responder.respond(cancelled_user_input_response());
    }
    Ok(Handled::Yes)
}

pub(super) fn questions(params: &Value) -> Option<Vec<UserInputQuestion>> {
    let request: Request = serde_json::from_value(params.clone()).ok()?;
    if !bounded(&request.session_id, 256, true)
        || !bounded(&request.run_id, 256, true)
        || !bounded(&request.tool_call_id, 256, true)
        || !(1..=4).contains(&request.questions.len())
    {
        return None;
    }
    let mut ids = HashSet::new();
    let mut questions = Vec::with_capacity(request.questions.len());
    for question in request.questions {
        if !bounded(&question.id, 64, true)
            || !ids.insert(question.id.trim().to_owned())
            || !bounded(&question.header, 128, false)
            || !bounded(&question.question, 4096, true)
            || question.options.len() > 8
        {
            return None;
        }
        let mut labels = HashSet::new();
        for option in &question.options {
            if !bounded(&option.label, 256, true)
                || !labels.insert(option.label.trim())
                || option
                    .description
                    .as_ref()
                    .is_some_and(|description| !bounded(description, 1024, false))
            {
                return None;
            }
        }
        questions.push(UserInputQuestion {
            id: question.id,
            header: question.header,
            question: question.question,
            options: question.options,
            multi_select: question.multi_select,
        });
    }
    Some(questions)
}

pub(super) fn complete(
    pending: &PendingAcpUserInputs,
    request_id: &str,
    answers: &[UserInputAnswer],
) -> crate::model::LocalUserInputResult {
    use crate::model::LocalUserInputResult;
    let Some(input) = pending.lock().remove(request_id) else {
        return LocalUserInputResult::NotPending;
    };
    let reply = response(&input.params, answers);
    let accepted = reply["outcome"] == "accepted";
    if input.responder.respond(reply).is_ok() && accepted {
        LocalUserInputResult::Delivered
    } else {
        LocalUserInputResult::NotPending
    }
}

pub(super) fn response(params: &Value, submitted: &[UserInputAnswer]) -> Value {
    let cancelled = || json!({"outcome": "cancelled"});
    let Some(questions) = questions(params) else {
        return cancelled();
    };
    if submitted.len() != questions.len() {
        return cancelled();
    }
    let mut answers = Vec::with_capacity(questions.len());
    let mut total_bytes = 0;
    for question in questions {
        let matching = submitted
            .iter()
            .filter(|answer| answer.question_id == question.id)
            .collect::<Vec<_>>();
        if matching.len() != 1 {
            return cancelled();
        }
        let answer = matching[0];
        if !(1..=8).contains(&answer.answers.len())
            || !question.multi_select && answer.answers.len() != 1
        {
            return cancelled();
        }
        let mut values = HashSet::new();
        for value in &answer.answers {
            total_bytes += value.len();
            if !bounded(value, 4096, true) || !values.insert(value.trim()) || total_bytes > 16384 {
                return cancelled();
            }
        }
        answers.push(answer);
    }
    json!({"outcome": "accepted", "answers": answers})
}

#[cfg(test)]
mod tests {
    use super::super::*;
    use super::*;

    fn request() -> Value {
        json!({
            "sessionId": "session", "runId": "run", "toolCallId": "call",
            "questions": [
                {"id": "q1", "header": "部署目标", "question": "部署到哪里？", "multiSelect": false,
                 "options": [{"label": "测试环境", "description": "先验证"}]},
                {"id": "q2", "question": "补充说明？", "options": []}
            ]
        })
    }

    #[test]
    fn deerflow_user_input_preserves_headers_options_and_free_text() {
        let params = request();
        let parsed = questions(&params).unwrap();
        assert_eq!(parsed[0].header, "部署目标");
        assert_eq!(parsed[0].options[0].description.as_deref(), Some("先验证"));
        assert!(parsed[1].options.is_empty());
        let answer = response(
            &params,
            &[
                UserInputAnswer {
                    question_id: "q2".into(),
                    answers: vec!["使用华东区域".into()],
                },
                UserInputAnswer {
                    question_id: "q1".into(),
                    answers: vec!["测试环境".into()],
                },
            ],
        );
        assert_eq!(
            answer,
            json!({"outcome": "accepted", "answers": [
                {"questionId": "q1", "answers": ["测试环境"]},
                {"questionId": "q2", "answers": ["使用华东区域"]}
            ]})
        );
        assert_eq!(
            capability()["deerflow"]["userInput"],
            json!({"version":1,"requestMethod":METHOD})
        );
        assert_eq!(
            serde_json::to_value(ClientCapabilities::new().meta(capability())).unwrap()["_meta"]["deerflow"]
                ["userInput"],
            json!({"version":1,"requestMethod":METHOD})
        );
    }

    #[test]
    fn deerflow_user_input_rejects_invalid_or_unbounded_questions() {
        let mut invalid = Vec::new();
        for (pointer, value) in [
            ("/sessionId", json!("")),
            ("/runId", json!("x".repeat(257))),
            ("/toolCallId", json!("bad\0id")),
            ("/questions", json!([])),
            (
                "/questions",
                json!(vec![request()["questions"][0].clone(); 5]),
            ),
            ("/questions/1/id", json!("q1")),
            ("/questions/1/id", json!(" q1 ")),
            ("/questions/0/id", json!("x".repeat(65))),
            ("/questions/0/header", json!("界".repeat(43))),
            ("/questions/0/question", json!(" ")),
            ("/questions/0/question", json!("x".repeat(4097))),
            (
                "/questions/0/options",
                json!(vec![json!({"label":"same"}); 2]),
            ),
            (
                "/questions/0/options",
                json!([{"label":"same"},{"label":" same "}]),
            ),
            (
                "/questions/0/options",
                json!(vec![json!({"label":"label"}); 9]),
            ),
            ("/questions/0/options/0/label", json!("x".repeat(257))),
            (
                "/questions/0/options/0/description",
                json!("x".repeat(1025)),
            ),
            ("/questions/0/multiSelect", json!("true")),
        ] {
            let mut params = request();
            *params.pointer_mut(pointer).unwrap() = value;
            invalid.push(params);
        }
        for params in invalid {
            assert!(
                questions(&params).is_none(),
                "accepted invalid request: {params}"
            );
        }
    }

    #[test]
    fn deerflow_user_input_responses_require_exact_ids_and_bounded_answers() {
        let params = request();
        let valid = vec![
            UserInputAnswer {
                question_id: "q1".into(),
                answers: vec!["custom".into()],
            },
            UserInputAnswer {
                question_id: "q2".into(),
                answers: vec!["notes".into()],
            },
        ];
        for answers in [
            vec![],
            vec![valid[0].clone(), valid[0].clone()],
            vec![
                UserInputAnswer {
                    question_id: "foreign".into(),
                    answers: vec!["yes".into()],
                },
                valid[1].clone(),
            ],
            vec![
                UserInputAnswer {
                    question_id: "q1".into(),
                    answers: vec!["one".into(), "two".into()],
                },
                valid[1].clone(),
            ],
            vec![
                UserInputAnswer {
                    question_id: "q1".into(),
                    answers: vec!["x".repeat(4097)],
                },
                valid[1].clone(),
            ],
        ] {
            assert_eq!(response(&params, &answers), json!({"outcome":"cancelled"}));
        }
        assert_eq!(
            super::super::cancelled_user_input_response(),
            json!({"outcome":"cancelled"})
        );
    }

    #[test]
    fn deerflow_user_input_stop_and_prompt_failure_release_pending_responders() {
        for mode in ["accepted", "stop", "timeout", "foreign-session"] {
            let answered = Arc::new(AtomicBool::new(false));
            let backend_answered = answered.clone();
            let agent = Agent.builder().on_receive_request(
                async move |_request: PromptRequest,
                            responder: Responder<PromptResponse>,
                            connection: ConnectionTo<Client>| {
                    let mut params = request();
                    if mode == "foreign-session" {
                        params["sessionId"] = json!("foreign");
                    }
                    let question = UntypedMessage::new(METHOD, params)?;
                    let answered = backend_answered.clone();
                    if mode == "timeout" {
                        connection.send_request(question).on_receiving_result(
                            async move |result| {
                                assert_eq!(result?, json!({"outcome":"cancelled"}));
                                answered.store(true, Ordering::Release);
                                Ok(())
                            },
                        )?;
                        responder.respond_with_internal_error("question timed out")
                    } else {
                        connection
                            .send_request(question)
                            .on_receiving_result(async move |result| {
                                assert_eq!(
                                    result?["outcome"],
                                    if mode == "accepted" {
                                        "accepted"
                                    } else {
                                        "cancelled"
                                    }
                                );
                                answered.store(true, Ordering::Release);
                                responder.respond(PromptResponse::new(StopReason::EndTurn))
                            })
                    }
                },
                agent_client_protocol::on_receive_request!(),
            );
            let (events, received) = crate::driver::test_event_channel();
            let pending: PendingAcpUserInputs = Arc::new(Mutex::new(HashMap::new()));
            let prompts = Arc::new(Mutex::new(PendingPrompts {
                user_inputs: Some(pending.clone()),
                ..PendingPrompts::default()
            }));
            let input_pending = pending.clone();
            let input_prompts = prompts.clone();
            let input_events = events.clone();
            let client = Client.builder().on_receive_request(
                async move |request: UntypedMessage, responder, _connection| {
                    assert_eq!(request.method(), METHOD);
                    receive(
                        request.params().clone(),
                        responder,
                        &input_prompts,
                        &input_pending,
                        &input_events,
                    )
                },
                agent_client_protocol::on_receive_request!(),
            );
            smol::block_on(smol::future::or(
                client.connect_with(agent, async move |connection| {
                    send_prompt(
                        &connection,
                        &SessionId::new("session"),
                        "ask".into(),
                        &[],
                        &prompts,
                        &events,
                        "session",
                        Arc::new(Mutex::new(AcpStreamState::default())),
                    )?;
                    let mut finished = false;
                    let mut shown = false;
                    loop {
                        match received.try_recv() {
                            Ok(DriverEvent::UserInputRequested { request_id, .. }) => {
                                assert_ne!(mode, "foreign-session");
                                shown = true;
                                if mode == "stop" {
                                    prompts.lock().user_input_stopped = true;
                                    cancel_pending_user_inputs(&pending);
                                    assert!(!prompts.lock().accepts_user_input("session"));
                                } else if mode == "accepted" {
                                    use crate::model::LocalUserInputResult;
                                    let delivered = complete(
                                        &pending, &request_id,
                                        &[
                                            UserInputAnswer {
                                                question_id: "q1".into(),
                                                answers: vec!["测试环境".into()],
                                            },
                                            UserInputAnswer {
                                                question_id: "q2".into(),
                                                answers: vec!["补充说明".into()],
                                            },
                                        ],
                                    );
                                    assert_eq!(delivered, LocalUserInputResult::Delivered);
                                    assert_eq!(complete(&pending, &request_id, &[]), LocalUserInputResult::NotPending);
                                }
                            }
                            Ok(DriverEvent::TurnFinished { .. }) => finished = true,
                            Ok(_) => {}
                            Err(_) => {}
                        }
                        if finished && answered.load(Ordering::Acquire) {
                            break;
                        }
                        smol::Timer::after(Duration::from_millis(5)).await;
                    }
                    assert_eq!(shown, mode != "foreign-session");
                    assert!(pending.lock().is_empty());
                    assert!(prompts.lock().is_empty());
                    assert!(!prompts.lock().accepts_user_input("session"));
                    Ok(())
                }),
                async {
                    smol::Timer::after(Duration::from_secs(5)).await;
                    Err(approval_error("question cleanup test timed out"))
                },
            ))
            .unwrap();
        }
    }
}
