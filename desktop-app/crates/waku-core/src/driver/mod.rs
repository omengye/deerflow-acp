//! Local provider runtime owned by `waku-daemon`.

mod acp;
mod activity;
mod support;

pub(crate) use acp::{catalog_agent, parse_acp_config_models};

use std::path::PathBuf;
use std::sync::Arc;

use crossbeam_channel::{Receiver, SendError, Sender, unbounded};

use crate::computer_use::ComputerToolRequest;
use crate::model::{
    BackgroundWorkKey, DriverEvent, ProviderKind, ProviderResumeCursor, RuntimeMode,
    UserInputAnswer,
};

/// Provider events remain synchronous to send from reader threads, while the
/// bounded wake channel lets the UI sleep until at least one event is ready.
/// Multiple provider writes coalesce into one wake without ever blocking the
/// provider or dropping the events themselves.
#[derive(Clone)]
pub struct DriverEventSender {
    events: Sender<DriverEvent>,
    wake: smol::channel::Sender<()>,
}

impl DriverEventSender {
    pub fn send(&self, event: DriverEvent) -> Result<(), SendError<DriverEvent>> {
        self.events.send(event)?;
        let _ = self.wake.try_send(());
        Ok(())
    }
}

pub(crate) trait DriverEventSink {
    fn send(&self, event: DriverEvent) -> Result<(), SendError<DriverEvent>>;
}

impl DriverEventSink for DriverEventSender {
    fn send(&self, event: DriverEvent) -> Result<(), SendError<DriverEvent>> {
        DriverEventSender::send(self, event)
    }
}

#[cfg(test)]
impl DriverEventSink for Sender<DriverEvent> {
    fn send(&self, event: DriverEvent) -> Result<(), SendError<DriverEvent>> {
        Sender::send(self, event)
    }
}

pub fn event_channel(
    wake: smol::channel::Sender<()>,
) -> (DriverEventSender, Receiver<DriverEvent>) {
    let (events, receiver) = unbounded();
    (DriverEventSender { events, wake }, receiver)
}

#[cfg(test)]
pub(crate) fn test_event_channel() -> (DriverEventSender, Receiver<DriverEvent>) {
    let (wake, _wakes) = smol::channel::bounded(1);
    event_channel(wake)
}

#[derive(Clone)]
pub struct DriverHandle {
    inner: Arc<dyn DriverControl>,
}

impl DriverHandle {
    pub fn from_control(control: Arc<dyn DriverControl>) -> Self {
        Self { inner: control }
    }

    pub fn prompt(&self, prompt: String) {
        self.inner.prompt(prompt);
    }

    pub fn prompt_with_attachments(
        &self,
        prompt: String,
        attachments: Vec<crate::model::MessageAttachment>,
    ) {
        self.inner.prompt_with_attachments(prompt, attachments);
    }

    /// Whether this transport can inject a user message into the currently
    /// running turn (steering) instead of starting a new one.
    pub fn supports_steer(&self) -> bool {
        self.inner.supports_steer()
    }

    pub fn steer(&self, prompt: String) {
        self.inner.steer(prompt);
    }

    pub fn steering_run_id(&self) -> Option<String> {
        self.inner.steering_run_id()
    }

    pub fn steer_run(&self, prompt: String, run_id: String) {
        self.inner.steer_run(prompt, run_id);
    }

    pub fn cancel(&self) {
        self.inner.cancel();
    }

    pub fn delete_session(
        &self,
        expected_native_session_id: String,
    ) -> Receiver<Result<(), String>> {
        self.inner.delete_session(expected_native_session_id)
    }

    pub fn cancel_computer_use(&self) {
        self.inner.cancel_computer_use();
    }

    pub fn refresh_background_work(&self) {
        self.inner.refresh_background_work();
    }

    pub fn deerflow_tasks(&self, command: waku_protocol::deerflow_tasks::Command) {
        self.inner.deerflow_tasks(command);
    }

    pub fn stop_background_work(&self, key: BackgroundWorkKey, control_id: String) {
        self.inner.stop_background_work(key, control_id);
    }

    pub fn respond(&self, request_id: String, option_id: String) -> Result<(), String> {
        self.inner.respond_checked(request_id, option_id)
    }

    pub fn respond_credential(&self, id: String, sealed: crate::model::SealedCredential) -> Result<(), String> {
        self.inner.respond_credential(id, sealed)
    }

    pub fn respond_user_input(&self, request_id: String, answers: Vec<UserInputAnswer>) {
        self.inner.respond_user_input(request_id, answers);
    }
    pub fn respond_user_input_checked(&self, request_id: String, answers: Vec<UserInputAnswer>) -> crate::model::LocalUserInputResult {
        self.inner.respond_user_input_checked(request_id, answers)
    }

    pub fn run_computer_tool(&self, request: ComputerToolRequest) {
        self.inner.run_computer_tool(request);
    }

    pub fn reject_computer_tool(&self, request: ComputerToolRequest, reason: String) {
        self.inner.reject_computer_tool(request, reason);
    }

    pub fn apply_options(&self, options: SessionOptions) -> bool {
        self.inner.apply_options(options)
    }

    pub fn set_tool_approval(&self, mode: String) -> anyhow::Result<String> {
        self.inner.set_tool_approval(mode)
    }



    pub(crate) fn deerflow_history(
        &self,
        method: &str,
        params: serde_json::Value,
    ) -> anyhow::Result<serde_json::Value> {
        self.inner.deerflow_history(method, params)
    }
}

pub trait DriverControl: Send + Sync {
    fn deerflow_history(
        &self,
        _method: &str,
        _params: serde_json::Value,
    ) -> anyhow::Result<serde_json::Value> {
        anyhow::bail!("native conversation history is not supported by this transport")
    }
    fn prompt(&self, prompt: String);
    fn prompt_with_attachments(
        &self,
        prompt: String,
        _attachments: Vec<crate::model::MessageAttachment>,
    ) {
        self.prompt(prompt);
    }
    fn supports_steer(&self) -> bool {
        false
    }
    /// Deliver a steering message to the running turn. Implementations report
    /// the outcome asynchronously through `DriverEvent::SteerAccepted` or
    /// `DriverEvent::SteerRejected`.
    fn steer(&self, _prompt: String) {}
    fn steering_run_id(&self) -> Option<String> {
        None
    }
    fn steer_run(&self, _prompt: String, _run_id: String) {}
    fn cancel(&self);
    fn delete_session(&self, _expected_native_session_id: String) -> Receiver<Result<(), String>> {
        let (reply, result) = crossbeam_channel::bounded(1);
        let _ = reply.send(Err(
            "Session deletion is not supported by this transport".into()
        ));
        result
    }
    fn cancel_computer_use(&self) {}
    fn refresh_background_work(&self) {}
    fn deerflow_tasks(&self, _command: waku_protocol::deerflow_tasks::Command) {}
    fn stop_background_work(&self, _key: BackgroundWorkKey, _control_id: String) {}
    fn respond(&self, request_id: String, option_id: String);
    fn respond_checked(&self, request_id: String, option_id: String) -> Result<(), String> {
        self.respond(request_id, option_id);
        Ok(())
    }
    fn respond_credential(&self, _id: String, _sealed: crate::model::SealedCredential) -> Result<(), String> { Err("Provider does not support credential enrollment".into()) }
    fn respond_user_input(&self, _request_id: String, _answers: Vec<UserInputAnswer>) {}
    fn respond_user_input_checked(&self, _request_id: String, _answers: Vec<UserInputAnswer>) -> crate::model::LocalUserInputResult {
        crate::model::LocalUserInputResult::NotSent
    }
    fn run_computer_tool(&self, _request: ComputerToolRequest) {}
    fn reject_computer_tool(&self, _request: ComputerToolRequest, _reason: String) {}
    /// Applies changed turn options to the live session, returning whether the
    /// transport could do it without being restarted. A `false` answer is the
    /// driver asking to be torn down and recreated with the new options.
    fn apply_options(&self, _options: SessionOptions) -> bool {
        false
    }
    fn set_tool_approval(&self, _mode: String) -> anyhow::Result<String> {
        anyhow::bail!("tool approval changes are not supported by this provider transport")
    }
}

pub struct DriverStartOptions {
    pub binary: PathBuf,
    pub cwd: PathBuf,
    pub mode: RuntimeMode,
    pub model: Option<String>,
    pub reasoning_effort: Option<String>,
    pub thinking_mode: Option<String>,
    pub service_tier: Option<String>,
    pub context_window: Option<String>,
    pub agent_preset: Option<String>,
    pub computer_use_enabled: bool,
    pub provider_cursor: Option<ProviderResumeCursor>,
}

/// The subset of `DriverStartOptions` a user can change without starting a new
/// task. Transports that carry these per turn can absorb a change in place;
/// the rest have to be restarted.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SessionOptions {
    pub mode: RuntimeMode,
    pub model: Option<String>,
    pub reasoning_effort: Option<String>,
    pub thinking_mode: Option<String>,
    pub service_tier: Option<String>,
    pub context_window: Option<String>,
}

pub(crate) fn start_local(
    provider: ProviderKind,
    options: DriverStartOptions,
    events: DriverEventSender,
) -> anyhow::Result<DriverHandle> {
    anyhow::ensure!(provider.is_available(), "{} sessions are read-only", provider.display_name());
    let inner = Arc::new(acp::AcpDriver::start(provider, options, events)?);
    Ok(DriverHandle { inner })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn provider_events_coalesce_wakes_without_dropping_payloads() {
        let (wake, wakes) = smol::channel::bounded(1);
        let (events, received) = event_channel(wake);

        events.send(DriverEvent::TextDelta("one".into())).unwrap();
        events.send(DriverEvent::TextDelta("two".into())).unwrap();

        assert_eq!(wakes.try_recv(), Ok(()));
        assert!(matches!(
            wakes.try_recv(),
            Err(smol::channel::TryRecvError::Empty)
        ));
        assert!(matches!(received.try_recv(), Ok(DriverEvent::TextDelta(text)) if text == "one"));
        assert!(matches!(received.try_recv(), Ok(DriverEvent::TextDelta(text)) if text == "two"));
    }
}
