use std::net::{SocketAddr, TcpStream};
use std::time::Duration;

use anyhow::{Context, bail};
use tungstenite::{Message, WebSocket};
use uuid::Uuid;
use waku_protocol::{
    ClientMessage, Command, DAEMON_TOKEN_ENV, PROTOCOL_VERSION, Request, ResponseOutcome,
    ResponsePayload, ServerMessage,
};

fn receive(socket: &mut WebSocket<TcpStream>) -> anyhow::Result<ServerMessage> {
    loop {
        match socket.read()? {
            Message::Text(text) => return Ok(serde_json::from_str(text.as_ref())?),
            Message::Close(_) => bail!("daemon closed the health check connection"),
            _ => socket.flush()?,
        }
    }
}

fn send(socket: &mut WebSocket<TcpStream>, message: &ClientMessage) -> anyhow::Result<()> {
    socket.send(Message::Text(serde_json::to_string(message)?.into()))?;
    Ok(())
}

fn main() -> anyhow::Result<()> {
    let address: SocketAddr = std::env::var("WAKU_HEALTH_ADDRESS")
        .unwrap_or_else(|_| "127.0.0.1:18787".to_owned())
        .parse()
        .context("invalid health check socket address")?;
    let token = std::env::var(DAEMON_TOKEN_ENV)
        .map_err(|_| anyhow::anyhow!("{DAEMON_TOKEN_ENV} is missing"))?;
    let timeout = Duration::from_secs(2);
    let stream = TcpStream::connect_timeout(&address, timeout)?;
    stream.set_read_timeout(Some(timeout))?;
    stream.set_write_timeout(Some(timeout))?;
    let (mut socket, _) = tungstenite::client(format!("ws://{address}/v1"), stream)?;
    send(
        &mut socket,
        &ClientMessage::Hello {
            protocol_version: PROTOCOL_VERSION,
            token,
            client_id: Uuid::new_v4(),
            resume_from: Vec::new(),
        },
    )?;
    match receive(&mut socket)? {
        ServerMessage::Hello {
            protocol_version, ..
        } if protocol_version == PROTOCOL_VERSION => {}
        _ => bail!("daemon health check authentication or protocol mismatch"),
    }
    let request_id = Uuid::new_v4();
    send(
        &mut socket,
        &ClientMessage::Request(Request {
            request_id,
            session_id: Uuid::nil(),
            runtime_id: Uuid::nil(),
            command: Command::GetSettings,
        }),
    )?;
    loop {
        if let ServerMessage::Response {
            request_id: response_id,
            outcome,
        } = receive(&mut socket)?
            && response_id == request_id
        {
            match outcome {
                ResponseOutcome::Ok {
                    payload: ResponsePayload::Settings { .. },
                } => break,
                _ => bail!("daemon rejected the settings health check"),
            }
        }
    }
    // A transport close avoids noisy resets and never requests daemon shutdown.
    socket.close(None)?;
    socket.flush()?;
    Ok(())
}
