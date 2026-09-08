use std::{
    path::{Path, PathBuf},
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
};

use agent_client_protocol::{
    Agent, ByteStreams, Channel, Client, ConnectTo, RawJsonRpcMessage, RawJsonRpcParams,
    TransportBatchEntry, TransportFrame,
};
use agent_client_protocol_http::{AcpHttpServer, CorsOptions, ServerOptions};
use axum::{
    body::Body,
    extract::{Request, State},
    http::{StatusCode, header},
    middleware::{self, Next},
    response::{IntoResponse, Response},
};
use futures::{
    StreamExt,
    channel::oneshot,
    future::{AbortHandle, AbortRegistration, Abortable},
};
use subtle::ConstantTimeEq;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::TcpStream;
use tokio::sync::Mutex;
use tokio_util::compat::{TokioAsyncReadCompatExt, TokioAsyncWriteCompatExt};

use crate::{HANDSHAKE_VERSION, Result, load_endpoint};

#[derive(Clone)]
struct AuthState {
    token: Arc<str>,
}

#[derive(Clone)]
struct DaemonTransport {
    endpoint_path: Arc<PathBuf>,
    workspace: Arc<str>,
    supervisor: ConnectionSupervisor,
}

#[derive(Clone, Default)]
struct ConnectionSupervisor {
    inner: Arc<ConnectionSupervisorInner>,
}

#[derive(Default)]
struct ConnectionSupervisorInner {
    next_id: AtomicU64,
    replacement: Mutex<()>,
    active: Mutex<Option<ActiveConnection>>,
}

struct ActiveConnection {
    id: u64,
    abort: AbortHandle,
    done: oneshot::Receiver<()>,
}

struct ConnectionLease {
    id: u64,
    supervisor: ConnectionSupervisor,
    done: Option<oneshot::Sender<()>>,
}

impl ConnectionSupervisor {
    async fn begin(&self) -> (ConnectionLease, AbortRegistration) {
        let id = self.inner.next_id.fetch_add(1, Ordering::Relaxed) + 1;
        eprintln!("deerflow-acp: gateway external connection #{id} started");

        // The gateway intentionally exposes one logical external transport at
        // a time. Local stdio bridges can connect to the daemon independently.
        let _replacement = self.inner.replacement.lock().await;
        let previous = {
            let mut active = self.inner.active.lock().await;
            active.take()
        };
        if let Some(previous) = previous {
            eprintln!(
                "deerflow-acp: gateway external connection #{id} superseding connection #{}",
                previous.id
            );
            previous.abort.abort();
            let _ = previous.done.await;
        }

        let (abort, registration) = AbortHandle::new_pair();
        let (done, done_receiver) = oneshot::channel();
        *self.inner.active.lock().await = Some(ActiveConnection {
            id,
            abort,
            done: done_receiver,
        });

        (
            ConnectionLease {
                id,
                supervisor: self.clone(),
                done: Some(done),
            },
            registration,
        )
    }

    async fn finish(&self, id: u64) {
        let mut active = self.inner.active.lock().await;
        if active
            .as_ref()
            .is_some_and(|connection| connection.id == id)
        {
            active.take();
        }
    }
}

impl ConnectionLease {
    async fn finish(mut self) {
        self.supervisor.finish(self.id).await;
        if let Some(done) = self.done.take() {
            let _ = done.send(());
        }
    }
}

impl Drop for ConnectionLease {
    fn drop(&mut self) {
        // This path is used when the SDK aborts its task (for example after an
        // explicit DELETE). Dropping the transport already closes the daemon
        // TCP stream; notifying a pending replacement keeps it from stalling.
        if let Some(done) = self.done.take() {
            eprintln!(
                "deerflow-acp: gateway external connection #{} transport dropped",
                self.id
            );
            let supervisor = self.supervisor.clone();
            let id = self.id;
            if let Ok(runtime) = tokio::runtime::Handle::try_current() {
                drop(runtime.spawn(async move { supervisor.finish(id).await }));
            }
            let _ = done.send(());
        }
    }
}

impl ConnectTo<Client> for DaemonTransport {
    async fn connect_to(self, client: impl ConnectTo<Agent>) -> agent_client_protocol::Result<()> {
        let (lease, cancellation) = self.supervisor.begin().await;
        let id = lease.id;
        let endpoint_path = self.endpoint_path;
        let workspace = self.workspace;
        let connection = async move {
            let stream = connect_daemon(&endpoint_path)
                .await
                .map_err(internal_error)?;
            eprintln!("deerflow-acp: gateway connection #{id} connected to local daemon");
            let (reader, writer) = stream.into_split();
            let daemon_stream = ByteStreams::new(writer.compat_write(), reader.compat());
            let (daemon, daemon_future) =
                <ByteStreams<_, _> as ConnectTo<Client>>::into_channel_and_future(daemon_stream);
            let (external, external_future) = client.into_channel_and_future();

            let bridge = bridge_with_workspace(external, daemon, workspace);
            let ((), (), ()) = futures::try_join!(bridge, daemon_future, external_future)?;
            Ok(())
        };

        let result = match Abortable::new(connection, cancellation).await {
            Ok(Ok(())) => {
                eprintln!("deerflow-acp: gateway connection #{id} closed");
                Ok(())
            }
            Ok(Err(error)) => {
                eprintln!("deerflow-acp: gateway connection #{id} failed: {error}");
                Err(error)
            }
            Err(_) => {
                eprintln!("deerflow-acp: gateway connection #{id} cancelled by replacement");
                Ok(())
            }
        };
        lease.finish().await;
        result
    }
}

fn internal_error(error: impl std::fmt::Display) -> agent_client_protocol::Error {
    agent_client_protocol::Error::internal_error().data(error.to_string())
}

pub(crate) async fn connect_daemon(path: &Path) -> Result<TcpStream> {
    let endpoint = load_endpoint(path)?;
    let address = format!("{}:{}", endpoint.host, endpoint.port);
    let mut stream = TcpStream::connect(address).await?;
    stream.set_nodelay(true)?;
    stream
        .write_all(format!("{HANDSHAKE_VERSION} {} ACP\n", endpoint.token).as_bytes())
        .await?;
    stream.flush().await?;

    let mut reader = BufReader::new(stream);
    let mut response = String::new();
    tokio::time::timeout(
        std::time::Duration::from_secs(3),
        reader.read_line(&mut response),
    )
    .await
    .map_err(|_| "timed out during the internal ACP daemon handshake")??;
    let response = response.trim();
    if response != "OK" && !response.starts_with("OK ") {
        return Err(match response {
            "BUSY" => "the local ACP daemon has reached its connection limit".into(),
            "UNAUTHORIZED" => "the local ACP daemon rejected its internal token".into(),
            "" => "the local ACP daemon closed during handshake".into(),
            value => format!("the local ACP daemon rejected the connection: {value}").into(),
        });
    }
    Ok(reader.into_inner())
}

async fn bridge_with_workspace(
    external: Channel,
    daemon: Channel,
    workspace: Arc<str>,
) -> agent_client_protocol::Result<()> {
    let Channel {
        rx: mut external_rx,
        tx: external_tx,
    } = external;
    let Channel {
        rx: mut daemon_rx,
        tx: daemon_tx,
    } = daemon;

    let to_daemon = async move {
        while let Some(mut frame) = external_rx.next().await {
            rewrite_workspace(&mut frame, &workspace)?;
            daemon_tx.unbounded_send(frame).map_err(internal_error)?;
        }
        Ok::<(), agent_client_protocol::Error>(())
    };
    let to_external = async move {
        while let Some(frame) = daemon_rx.next().await {
            external_tx.unbounded_send(frame).map_err(internal_error)?;
        }
        Ok::<(), agent_client_protocol::Error>(())
    };

    futures::try_join!(to_daemon, to_external)?;
    Ok(())
}

fn rewrite_workspace(
    frame: &mut TransportFrame,
    workspace: &str,
) -> agent_client_protocol::Result<()> {
    match frame {
        TransportFrame::Single(message) => rewrite_message_workspace(message, workspace),
        TransportFrame::Malformed { .. } => Ok(()),
        TransportFrame::Batch(batch) => {
            for entry in batch.entries_mut() {
                if let TransportBatchEntry::Message(message) = entry {
                    rewrite_message_workspace(message, workspace)?;
                }
            }
            Ok(())
        }
    }
}

fn rewrite_message_workspace(
    message: &mut RawJsonRpcMessage,
    workspace: &str,
) -> agent_client_protocol::Result<()> {
    let RawJsonRpcMessage::Request(request) = message else {
        return Ok(());
    };
    if !matches!(
        request.method.as_ref(),
        "session/new" | "session/load" | "session/resume" | "session/list" | "session/fork"
    ) {
        return Ok(());
    }
    match &mut request.params {
        Some(RawJsonRpcParams::Object(params)) => {
            params.insert("cwd".to_string(), workspace.into());
            Ok(())
        }
        None => {
            request.params = Some(RawJsonRpcParams::Object(serde_json::Map::from_iter([(
                "cwd".to_string(),
                workspace.into(),
            )])));
            Ok(())
        }
        Some(RawJsonRpcParams::Array(_)) => Err(agent_client_protocol::Error::invalid_params()
            .data("session workspace rewriting requires object params")),
    }
}

async fn authorize(State(state): State<AuthState>, request: Request<Body>, next: Next) -> Response {
    let authorized = request
        .headers()
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.strip_prefix("Bearer "))
        .is_some_and(|candidate| candidate.as_bytes().ct_eq(state.token.as_bytes()).into());
    if !authorized {
        eprintln!(
            "deerflow-acp: gateway authorization rejected for {} {}",
            request.method(),
            request.uri().path()
        );
        return (
            StatusCode::UNAUTHORIZED,
            [(header::WWW_AUTHENTICATE, "Bearer")],
            "Unauthorized",
        )
            .into_response();
    }
    next.run(request).await
}

pub(crate) async fn run(
    listen: std::net::SocketAddr,
    endpoint_path: PathBuf,
    workspace: PathBuf,
    token: String,
    path: String,
) -> Result<()> {
    let workspace = workspace
        .to_str()
        .ok_or("gateway workspace path is not valid UTF-8")?
        .to_owned();
    let transport = DaemonTransport {
        endpoint_path: Arc::new(endpoint_path),
        workspace: Arc::from(workspace),
        supervisor: ConnectionSupervisor::default(),
    };
    let router = AcpHttpServer::new(move || transport.clone())
        .with_options(ServerOptions {
            path,
            cors: CorsOptions::disabled(),
            health_endpoint: true,
        })
        .into_router()
        .layer(middleware::from_fn_with_state(
            AuthState {
                token: Arc::from(token),
            },
            authorize,
        ));
    let listener = tokio::net::TcpListener::bind(listen).await?;
    eprintln!("deerflow-acp: gateway listening on http://{listen}");
    axum::serve(listener, router).await?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use agent_client_protocol::schema::v1::RequestId;
    use axum::{Router, body::Body, http::Request, middleware, routing::get};
    use tower::ServiceExt;

    fn session_request(method: &str, cwd: &str) -> TransportFrame {
        TransportFrame::Single(
            RawJsonRpcMessage::request(
                method.to_string(),
                serde_json::json!({"cwd": cwd}),
                RequestId::Number(1),
            )
            .unwrap(),
        )
    }

    fn cwd(frame: &TransportFrame) -> Option<&str> {
        let TransportFrame::Single(RawJsonRpcMessage::Request(request)) = frame else {
            return None;
        };
        let Some(RawJsonRpcParams::Object(params)) = &request.params else {
            return None;
        };
        params.get("cwd").and_then(serde_json::Value::as_str)
    }

    #[test]
    fn rewrites_remote_session_cwd() {
        let mut frame = session_request("session/new", "/remote/workspace");
        rewrite_workspace(&mut frame, r"D:\local\workspace").unwrap();
        assert_eq!(cwd(&frame), Some(r"D:\local\workspace"));
    }

    #[test]
    fn rewrites_v2_resume_cwd() {
        let mut frame = session_request("session/resume", "/remote/workspace");
        rewrite_workspace(&mut frame, r"D:\local\workspace").unwrap();
        assert_eq!(cwd(&frame), Some(r"D:\local\workspace"));
    }

    #[test]
    fn leaves_non_session_requests_unchanged() {
        let mut frame = TransportFrame::Single(
            RawJsonRpcMessage::request(
                "session/prompt".to_string(),
                serde_json::json!({"sessionId": "one", "cwd": "/remote"}),
                RequestId::Number(2),
            )
            .unwrap(),
        );
        rewrite_workspace(&mut frame, "/local").unwrap();
        assert_eq!(cwd(&frame), Some("/remote"));
    }

    #[tokio::test]
    async fn bearer_token_is_required() {
        let app = Router::new()
            .route("/acp", get(|| async { StatusCode::OK }))
            .layer(middleware::from_fn_with_state(
                AuthState {
                    token: Arc::from("correct-token"),
                },
                authorize,
            ));

        let missing = app
            .clone()
            .oneshot(Request::builder().uri("/acp").body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(missing.status(), StatusCode::UNAUTHORIZED);

        let wrong = app
            .clone()
            .oneshot(
                Request::builder()
                    .uri("/acp")
                    .header(header::AUTHORIZATION, "Bearer wrong-token")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(wrong.status(), StatusCode::UNAUTHORIZED);

        let allowed = app
            .oneshot(
                Request::builder()
                    .uri("/acp")
                    .header(header::AUTHORIZATION, "Bearer correct-token")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(allowed.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn a_new_connection_cancels_the_previous_transport() {
        let supervisor = ConnectionSupervisor::default();
        let (first_lease, first_cancellation) = supervisor.begin().await;
        let first = tokio::spawn(async move {
            let result = Abortable::new(std::future::pending::<()>(), first_cancellation).await;
            first_lease.finish().await;
            result
        });

        let (second_lease, _second_cancellation) =
            tokio::time::timeout(std::time::Duration::from_secs(1), supervisor.begin())
                .await
                .expect("replacement should not wait indefinitely");

        assert!(first.await.unwrap().is_err());
        second_lease.finish().await;
        assert!(supervisor.inner.active.lock().await.is_none());
    }
}
