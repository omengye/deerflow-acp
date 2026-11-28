//! Desktop proxy for the provider runtime owned by `waku-daemon`.

mod recovery;

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Instant;

use crate::computer_use::ComputerToolRequest;
use crate::model::{BackgroundWorkKey, DriverEvent, ProviderKind, RuntimeEventCursor};
use crossbeam_channel::{Sender, bounded, select};
use parking_lot::Mutex;
use recovery::{SessionRecovery, log_attachment, reattach};

pub use waku_client::driver::{
    DriverControl, DriverEventSender, DriverHandle, DriverStartOptions, SessionOptions,
    event_channel,
};

pub(crate) fn start_remote(
    daemon: waku_client::DaemonSupervisor,
    session_id: uuid::Uuid,
    provider: ProviderKind,
    options: DriverStartOptions,
    events: DriverEventSender,
) -> anyhow::Result<DriverHandle> {
    let client = daemon.client();
    let runtime_id = uuid::Uuid::new_v4();
    let command = waku_client::Command::Start {
        options: waku_client::WireDriverStartOptions {
            provider: waku_client::encode_enum(provider)?,
            binary: options.binary,
            cwd: options.cwd,
            mode: waku_client::encode_enum(options.mode)?,
            model: options.model,
            reasoning_effort: options.reasoning_effort,
            thinking_mode: options.thinking_mode,
            service_tier: options.service_tier,
            context_window: options.context_window,
            agent_preset: options.agent_preset,
            computer_use_enabled: options.computer_use_enabled,
            provider_cursor: options
                .provider_cursor
                .map(serde_json::to_value)
                .transpose()?,
        },
    };
    let supports_steer = match client.request(session_id, runtime_id, command) {
        Ok(waku_client::ResponsePayload::Started { supports_steer }) => supports_steer,
        Ok(_) => anyhow::bail!("Waku daemon returned an invalid start response"),
        Err(error) => return Err(error),
    };
    connect_remote(
        daemon,
        client,
        session_id,
        runtime_id,
        supports_steer,
        None,
        events,
    )
}

pub(crate) fn attach_remote(
    daemon: waku_client::DaemonSupervisor,
    client: waku_client::DaemonClient,
    session_id: uuid::Uuid,
    runtime_id: uuid::Uuid,
    supports_steer: bool,
    replay_cursor: Option<RuntimeEventCursor>,
    events: DriverEventSender,
) -> anyhow::Result<DriverHandle> {
    connect_remote(
        daemon,
        client,
        session_id,
        runtime_id,
        supports_steer,
        replay_cursor,
        events,
    )
}

fn connect_remote(
    daemon: waku_client::DaemonSupervisor,
    initial_client: waku_client::DaemonClient,
    session_id: uuid::Uuid,
    runtime_id: uuid::Uuid,
    supports_steer: bool,
    replay_cursor: Option<RuntimeEventCursor>,
    events: DriverEventSender,
) -> anyhow::Result<DriverHandle> {
    let client_updates = daemon.subscribe_clients();
    let recovery = Arc::new(SessionRecovery::new(!daemon.is_remote()));
    let forwarding_recovery = recovery.clone();
    let active_client = Arc::new(Mutex::new(initial_client.clone()));
    let forwarding_client = active_client.clone();
    let supports_steer = Arc::new(AtomicBool::new(supports_steer));
    let forwarding_steer = supports_steer.clone();
    let closed = Arc::new(AtomicBool::new(false));
    let forwarding_closed = closed.clone();
    let (shutdown, forwarding_shutdown) = bounded(1);
    let forwarding_events = events.clone();
    let thread_initial_client = initial_client.clone();
    let spawn = std::thread::Builder::new()
        .name(format!("waku-daemon-session-{session_id}"))
        .spawn(move || {
            let mut client = thread_initial_client;
            let mut remote_events = client.subscribe(session_id, runtime_id);
            loop {
                let disconnected = loop {
                    select! {
                        recv(forwarding_shutdown) -> _ => return,
                        recv(remote_events) -> sequenced => {
                            let Ok(sequenced) = sequenced else {
                                break true;
                            };
                            if replay_cursor.is_some_and(|cursor| {
                                cursor.runtime_id == sequenced.runtime_id
                                    && cursor.epoch == sequenced.epoch
                                    && cursor.sequence >= sequenced.sequence
                            }) {
                                continue;
                            }
                            let cursor = RuntimeEventCursor {
                                runtime_id: sequenced.runtime_id,
                                epoch: sequenced.epoch,
                                sequence: sequenced.sequence,
                            };
                            let event = match waku_client::event_from_wire(sequenced.event) {
                                Ok(event) => event,
                                Err(error) => DriverEvent::Error(format!(
                                    "Waku daemon sent an invalid event: {error}"
                                )),
                            };
                            let process_exited = matches!(&event, DriverEvent::ProcessExited);
                            if let DriverEvent::SteeringCapability { supported, .. } = &event {
                                forwarding_steer.store(*supported, Ordering::Release);
                            }
                            if forwarding_events.send(event).is_err()
                                || forwarding_events
                                    .send(DriverEvent::RuntimeEventCursorAdvanced(cursor))
                                    .is_err()
                            {
                                return;
                            }
                            if process_exited {
                                break false;
                            }
                        }
                    }
                };
                client.unsubscribe(session_id, runtime_id);
                if !disconnected || forwarding_closed.load(Ordering::Acquire) {
                    break;
                }
                forwarding_recovery.begin();
                forwarding_steer.store(false, Ordering::Release);
                let _ = forwarding_events.send(DriverEvent::SteeringCapability {
                    supported: false,
                    run_id: None,
                });

                // A closed WebSocket says nothing about the provider process.
                // Wait for the supervisor's next connection, then ask that
                // daemon whether this exact runtime survived before changing
                // the task status.
                let Some((replacement, attached)) = reattach(
                    &client,
                    |candidate| daemon.client().same_connection(candidate),
                    &client_updates,
                    &forwarding_shutdown,
                    &forwarding_recovery,
                    session_id,
                    runtime_id,
                ) else {
                    return;
                };
                if forwarding_closed.load(Ordering::Acquire) {
                    return;
                }
                let attached = match attached {
                    Ok(attached) => attached,
                    Err(error) => {
                        let _ = forwarding_events.send(DriverEvent::LocalSessionAttachmentFailed(
                            error.to_string(),
                        ));
                        break;
                    }
                };
                if let waku_client::ResponsePayload::SessionRuntime {
                    pending_history: Some(operation),
                    ..
                } = &attached
                {
                    waku_client::WorkspaceClient::restore_pending_history(session_id, operation);
                }
                match attached {
                    waku_client::ResponsePayload::SessionRuntime {
                        runtime_id: Some(attached_runtime_id),
                        supports_steer,
                        steering_run_id,
                        conversation_capability,
                        deerflow_ready,
                        ..
                    } if attached_runtime_id == runtime_id => {
                        remote_events = replacement.subscribe(session_id, runtime_id);
                        log_attachment(
                            "session_reattached",
                            &replacement,
                            &forwarding_recovery,
                            session_id,
                            runtime_id,
                            None,
                            "same_runtime",
                        );
                        forwarding_recovery.install(
                            &forwarding_client,
                            &replacement,
                            session_id,
                            runtime_id,
                        );
                        forwarding_steer.store(supports_steer, Ordering::Release);
                        let _ = forwarding_events.send(DriverEvent::SteeringCapability {
                            supported: supports_steer,
                            run_id: steering_run_id,
                        });
                        if let Some(supported) = conversation_capability {
                            let _ = forwarding_events
                                .send(DriverEvent::ConversationCapability { supported });
                        }
                        if let Some(ready) = deerflow_ready {
                            let _ = forwarding_events.send(DriverEvent::DeerFlowTasks(ready));
                        }
                        client = replacement;
                    }
                    waku_client::ResponsePayload::SessionRuntime { .. } => {
                        log_attachment(
                            "session_runtime_gone",
                            &replacement,
                            &forwarding_recovery,
                            session_id,
                            runtime_id,
                            None,
                            "runtime_missing_or_replaced",
                        );
                        let _ = forwarding_events.send(DriverEvent::ProcessExited);
                        break;
                    }
                    _ => {
                        let failure = if forwarding_recovery.local {
                            log_attachment(
                                "session_reattach_failed",
                                &replacement,
                                &forwarding_recovery,
                                session_id,
                                runtime_id,
                                None,
                                "invalid_attach_response",
                            );
                            DriverEvent::LocalSessionAttachmentFailed(
                                "本地服务返回了无效的会话挂接响应。".into(),
                            )
                        } else {
                            DriverEvent::Error(
                                "Waku daemon returned an invalid runtime attachment response"
                                    .into(),
                            )
                        };
                        let _ = forwarding_events.send(failure);
                        break;
                    }
                }
            }
        });
    if let Err(error) = spawn {
        initial_client.unsubscribe(session_id, runtime_id);
        return Err(error.into());
    }
    Ok(DriverHandle::from_control(Arc::new(RemoteDriverControl {
        recovery,
        client: active_client,
        session_id,
        runtime_id,
        supports_steer,
        events,
        closed,
        shutdown,
    })))
}

struct RemoteDriverControl {
    recovery: Arc<SessionRecovery>,
    client: Arc<Mutex<waku_client::DaemonClient>>,
    session_id: uuid::Uuid,
    runtime_id: uuid::Uuid,
    supports_steer: Arc<AtomicBool>,
    events: DriverEventSender,
    closed: Arc<AtomicBool>,
    shutdown: Sender<()>,
}

impl RemoteDriverControl {
    fn notify(&self, command: waku_client::Command) {
        self.report_admission(self.try_notify(command));
    }

    fn report_admission(&self, queued: bool) {
        if !queued {
            let _ = self.events.send(DriverEvent::Error(
                "本地连接正在恢复，本次操作未发送。".into(),
            ));
        }
    }

    fn try_notify(&self, command: waku_client::Command) -> bool {
        let client = self.client.lock();
        if self.recovery.since().is_some() {
            return false;
        }
        if let Err(error) = client.notify(self.session_id, self.runtime_id, command) {
            if self.recovery.local && waku_client::DaemonClient::definitely_not_sent(&error) {
                self.recovery.begin();
                return false;
            }
            let _ = self.events.send(DriverEvent::Error(format!(
                "Waku daemon command failed: {error}"
            )));
        }
        true
    }
}

impl DriverControl for RemoteDriverControl {
    fn local_reconnecting_since(&self) -> Option<Instant> {
        self.recovery.since()
    }

    fn local_connection_generation(&self) -> u64 {
        self.recovery.generation.load(Ordering::Acquire)
    }

    fn try_prompt_with_attachments(
        &self,
        prompt: String,
        turn_id: Option<uuid::Uuid>,
        message_id: Option<uuid::Uuid>,
        attachments: Vec<waku_protocol::model::MessageAttachment>,
    ) -> bool {
        let command = if attachments.is_empty() {
            waku_client::Command::Prompt {
                prompt,
                turn_id,
                message_id,
            }
        } else {
            waku_client::Command::PromptWithAttachments {
                prompt,
                turn_id,
                message_id,
                attachments,
            }
        };
        self.try_notify(command)
    }

    fn try_steer(&self, prompt: String) -> bool {
        self.try_notify(waku_client::Command::Steer {
            prompt,
            run_id: None,
        })
    }

    fn try_steer_run(&self, prompt: String, run_id: String) -> bool {
        self.try_notify(waku_client::Command::Steer {
            prompt,
            run_id: Some(run_id),
        })
    }

    fn try_cancel(&self) -> bool {
        self.try_notify(waku_client::Command::Cancel)
    }

    fn try_respond(&self, request_id: String, option_id: String) -> bool {
        self.try_notify(waku_client::Command::Respond {
            request_id,
            option_id,
        })
    }

    fn try_respond_user_input(
        &self,
        request_id: String,
        answers: Vec<waku_protocol::model::UserInputAnswer>,
    ) -> bool {
        use waku_protocol::model::LocalUserInputResult;
        let command = waku_client::Command::RespondUserInput {
            request_id: request_id.clone(),
            answers,
            confirm_delivery: self.recovery.local,
        };
        if !self.recovery.local {
            return self.try_notify(command);
        }
        let client = self.client.lock();
        if self.recovery.since().is_some() {
            return false;
        }
        let pending = match client.begin_request(self.session_id, self.runtime_id, command) {
            Ok(pending) => pending,
            Err(_) => {
                self.recovery.begin();
                return false;
            }
        };
        drop(client);
        let events = self.events.clone();
        let submitted = request_id.clone();
        let waiter = std::thread::Builder::new().name("deerflow-answer-receipt".into())
            .spawn(move || {
                let result = match pending.wait(std::time::Duration::from_secs(15)) {
                    Ok(waku_client::ResponsePayload::UserInputReceipt { result }) => result,
                    // Older daemons Ack the queue operation. Only the specific
                    // checked receipt confirms that the ACP responder accepted it.
                    _ => LocalUserInputResult::Unknown,
                };
                let _ = events.send(DriverEvent::LocalUserInputResult { request_id: submitted, result });
            });
        if waiter.is_err() {
            let _ = self.events.send(DriverEvent::LocalUserInputResult { request_id, result: LocalUserInputResult::Unknown });
        }
        true
    }

    fn try_run_computer_tool(&self, request: ComputerToolRequest) -> bool {
        self.try_notify(waku_client::Command::RunComputerTool {
            request: waku_client::WireComputerToolRequest {
                call_id: request.call_id,
                tool: request.tool,
                arguments: request.arguments,
            },
        })
    }

    fn try_reject_computer_tool(&self, request: ComputerToolRequest, reason: String) -> bool {
        self.try_notify(waku_client::Command::RejectComputerTool {
            request: waku_client::WireComputerToolRequest {
                call_id: request.call_id,
                tool: request.tool,
                arguments: request.arguments,
            },
            reason,
        })
    }
    fn respond_credential(
        &self,
        client: waku_client::DaemonClient,
        request_id: String,
        sealed: waku_protocol::model::SealedCredential,
    ) -> anyhow::Result<()> {
        anyhow::ensure!(
            self.client.lock().same_connection(&client),
            "连接已变更，请重新打开凭据表单"
        );
        anyhow::ensure!(
            client.allows_credential_enrollment(),
            "请通过 WSS 或本机回环连接录入凭据"
        );
        let response = client.request(
            self.session_id,
            self.runtime_id,
            waku_client::Command::RespondCredential { request_id, sealed },
        )?;
        anyhow::ensure!(
            matches!(response, waku_client::ResponsePayload::Ack),
            "凭据提交状态待核对"
        );
        Ok(())
    }

    fn prompt_with_attachments(
        &self,
        prompt: String,
        turn_id: Option<uuid::Uuid>,
        message_id: Option<uuid::Uuid>,
        attachments: Vec<waku_protocol::model::MessageAttachment>,
    ) {
        self.report_admission(self.try_prompt_with_attachments(
            prompt,
            turn_id,
            message_id,
            attachments,
        ));
    }

    fn prompt(&self, prompt: String, turn_id: Option<uuid::Uuid>, message_id: Option<uuid::Uuid>) {
        self.prompt_with_attachments(prompt, turn_id, message_id, Vec::new());
    }

    fn supports_steer(&self) -> bool {
        self.supports_steer.load(Ordering::Acquire)
    }

    fn steer(&self, prompt: String) {
        self.report_admission(self.try_steer(prompt));
    }

    fn steer_run(&self, prompt: String, run_id: String) {
        self.report_admission(self.try_steer_run(prompt, run_id));
    }

    fn cancel(&self) {
        self.report_admission(self.try_cancel());
    }

    fn cancel_computer_use(&self) {
        // Turn-finished/process-exited cleanup is scoped to the old turn.
        // Its cancellation must never be replayed against a later turn.
        self.try_notify(waku_client::Command::CancelComputerUse);
    }

    fn refresh_background_work(&self) {
        self.try_notify(waku_client::Command::RefreshBackgroundWork);
    }

    fn deerflow_tasks(&self, command: waku_protocol::deerflow_tasks::Command) {
        // Task management errors stay on the task surface, never fail a chat turn.
        let client = self.client.lock().clone();
        if client
            .notify(
                self.session_id,
                self.runtime_id,
                waku_client::Command::DeerFlowTasks {
                    command: command.clone(),
                },
            )
            .is_err()
        {
            let _ = self.events.send(DriverEvent::DeerFlowTasks(
                waku_protocol::deerflow_tasks::Event::Failed {
                    command,
                    code: "connection_closed".into(),
                    message: "任务连接已断开，请重新连接后刷新。".into(),
                },
            ));
        }
    }

    fn stop_background_work(&self, key: BackgroundWorkKey, control_id: String) {
        match serde_json::to_value(key) {
            Ok(key) => self.notify(waku_client::Command::StopBackgroundWork { key, control_id }),
            Err(error) => {
                let _ = self.events.send(DriverEvent::Error(format!(
                    "could not encode background-work command: {error}"
                )));
            }
        }
    }

    fn respond(&self, request_id: String, option_id: String) {
        self.report_admission(self.try_respond(request_id, option_id));
    }

    fn respond_user_input(
        &self,
        request_id: String,
        answers: Vec<waku_protocol::model::UserInputAnswer>,
    ) {
        self.report_admission(self.try_respond_user_input(request_id, answers));
    }

    fn run_computer_tool(&self, request: ComputerToolRequest) {
        self.report_admission(self.try_run_computer_tool(request));
    }

    fn reject_computer_tool(&self, request: ComputerToolRequest, reason: String) {
        self.report_admission(self.try_reject_computer_tool(request, reason));
    }

    fn apply_options(&self, options: SessionOptions) -> bool {
        let options = (|| {
            Ok::<_, anyhow::Error>(waku_client::WireSessionOptions {
                mode: waku_client::encode_enum(options.mode)?,
                model: options.model,
                reasoning_effort: options.reasoning_effort,
                thinking_mode: options.thinking_mode,
                service_tier: options.service_tier,
                context_window: options.context_window,
            })
        })();
        let Ok(options) = options else {
            return false;
        };
        let client = self.client.lock().clone();
        matches!(
            client.request(
                self.session_id,
                self.runtime_id,
                waku_client::Command::ApplyOptions { options }
            ),
            Ok(waku_client::ResponsePayload::OptionsApplied { applied: true })
        )
    }

    fn set_tool_approval(&self, mode: String) -> anyhow::Result<String> {
        let client = self.client.lock().clone();
        match client.request(
            self.session_id,
            self.runtime_id,
            waku_client::Command::SetToolApproval { mode },
        )? {
            waku_client::ResponsePayload::ToolApproval { mode } => Ok(mode),
            _ => anyhow::bail!("DeerFlow returned an invalid tool approval response"),
        }
    }

    fn close(&self) {
        self.notify(waku_client::Command::CloseSession);
    }
}

impl Drop for RemoteDriverControl {
    fn drop(&mut self) {
        self.closed.store(true, Ordering::Release);
        let _ = self.shutdown.try_send(());
        let client = self.client.lock().clone();
        client.unsubscribe(self.session_id, self.runtime_id);
    }
}

#[cfg(test)]
mod recovery_tests;
