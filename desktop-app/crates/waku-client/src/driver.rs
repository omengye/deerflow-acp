//! Provider-neutral handle implemented by the desktop RPC adapter.

use std::path::PathBuf;
use std::sync::Arc;

use crossbeam_channel::{Receiver, SendError, Sender, unbounded};
use uuid::Uuid;
use waku_protocol::computer_use::ComputerToolRequest;
use waku_protocol::model::{
    BackgroundWorkKey, DriverEvent, ProviderResumeCursor, RuntimeMode,
    UserInputAnswer,
};

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

pub fn event_channel(
    wake: smol::channel::Sender<()>,
) -> (DriverEventSender, Receiver<DriverEvent>) {
    let (events, receiver) = unbounded();
    (DriverEventSender { events, wake }, receiver)
}

#[derive(Clone)]
pub struct DriverHandle {
    inner: Arc<dyn DriverControl>,
}

impl DriverHandle {
    pub fn from_control(control: Arc<dyn DriverControl>) -> Self {
        Self { inner: control }
    }

    /// Send a prompt along with the ids this client gave the turn it opened
    /// and that turn's user message, so the daemon can publish the same
    /// identity to every other client attached to the runtime.
    pub fn prompt(&self, prompt: String, turn_id: Option<Uuid>, message_id: Option<Uuid>) {
        self.inner.prompt(prompt, turn_id, message_id);
    }

    pub fn prompt_with_attachments(
        &self,
        prompt: String,
        turn_id: Option<Uuid>,
        message_id: Option<Uuid>,
        attachments: Vec<waku_protocol::model::MessageAttachment>,
    ) {
        self.inner
            .prompt_with_attachments(prompt, turn_id, message_id, attachments);
    }

    pub fn supports_steer(&self) -> bool {
        self.inner.supports_steer()
    }

    pub fn steer(&self, prompt: String) {
        self.inner.steer(prompt);
    }

    pub fn steer_run(&self, prompt: String, run_id: String) {
        self.inner.steer_run(prompt, run_id);
    }

    pub fn cancel(&self) {
        self.inner.cancel();
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

    pub fn respond(&self, request_id: String, option_id: String) {
        self.inner.respond(request_id, option_id);
    }

    pub fn respond_credential(
        &self,
        connection: crate::DaemonClient,
        request_id: String,
        sealed: waku_protocol::model::SealedCredential,
    ) -> anyhow::Result<()> {
        self.inner.respond_credential(connection, request_id, sealed)
    }

    pub fn respond_user_input(&self, request_id: String, answers: Vec<UserInputAnswer>) {
        self.inner.respond_user_input(request_id, answers);
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

    /// Returns only after the provider has persisted the selected approval policy.
    /// Call from a background worker, never from a render or UI callback.
    pub fn set_tool_approval(&self, mode: String) -> anyhow::Result<String> {
        self.inner.set_tool_approval(mode)
    }

    pub fn close(&self) {
        self.inner.close();
    }
}

pub trait DriverControl: Send + Sync {
    fn respond_credential(
        &self,
        _connection: crate::DaemonClient,
        _request_id: String,
        _sealed: waku_protocol::model::SealedCredential,
    ) -> anyhow::Result<()> {
        anyhow::bail!("当前运行时无法提交凭据")
    }
    fn prompt(&self, prompt: String, turn_id: Option<Uuid>, message_id: Option<Uuid>);
    fn prompt_with_attachments(
        &self,
        prompt: String,
        turn_id: Option<Uuid>,
        message_id: Option<Uuid>,
        _attachments: Vec<waku_protocol::model::MessageAttachment>,
    ) {
        self.prompt(prompt, turn_id, message_id);
    }
    fn supports_steer(&self) -> bool {
        false
    }
    fn steer(&self, _prompt: String) {}
    fn steer_run(&self, _prompt: String, _run_id: String) {}
    fn cancel(&self);
    fn cancel_computer_use(&self) {}
    fn refresh_background_work(&self) {}
    fn deerflow_tasks(&self, _command: waku_protocol::deerflow_tasks::Command) {}
    fn stop_background_work(&self, _key: BackgroundWorkKey, _control_id: String) {}
    fn respond(&self, request_id: String, option_id: String);
    fn respond_user_input(&self, _request_id: String, _answers: Vec<UserInputAnswer>) {}
    fn run_computer_tool(&self, _request: ComputerToolRequest) {}
    fn reject_computer_tool(&self, _request: ComputerToolRequest, _reason: String) {}
    fn apply_options(&self, _options: SessionOptions) -> bool {
        false
    }
    fn set_tool_approval(&self, _mode: String) -> anyhow::Result<String> {
        anyhow::bail!("This provider does not support tool approval settings")
    }
    fn close(&self) {}
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

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SessionOptions {
    pub mode: RuntimeMode,
    pub model: Option<String>,
    pub reasoning_effort: Option<String>,
    pub thinking_mode: Option<String>,
    pub service_tier: Option<String>,
    pub context_window: Option<String>,
}
