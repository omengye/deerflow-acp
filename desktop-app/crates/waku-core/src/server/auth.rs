//! Bound the entire unauthenticated exchange, including reads hidden inside
//! tungstenite's HTTP and fragmented-message parsers. Per-read socket timeouts
//! alone can be extended forever by a peer that trickles bytes or sends Ping.

use std::io::{self, Read, Write};
use std::net::TcpStream;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

const SHUTDOWN_POLL_INTERVAL: Duration = Duration::from_millis(100);

pub(super) struct HandshakeStream {
    stream: TcpStream,
    deadline: Option<Instant>,
    shutdown: Arc<AtomicBool>,
}

impl HandshakeStream {
    pub(super) fn new(stream: TcpStream, deadline: Instant, shutdown: Arc<AtomicBool>) -> Self {
        Self {
            stream,
            deadline: Some(deadline),
            shutdown,
        }
    }

    pub(super) fn check_authentication_deadline(&self) -> io::Result<()> {
        if self.deadline.is_some() && self.shutdown.load(Ordering::Acquire) {
            return Err(io::Error::new(
                io::ErrorKind::ConnectionAborted,
                "daemon stopped during authentication",
            ));
        }
        if self
            .deadline
            .is_some_and(|deadline| Instant::now() >= deadline)
        {
            return Err(io::Error::new(
                io::ErrorKind::TimedOut,
                "daemon authentication deadline exceeded",
            ));
        }
        Ok(())
    }

    fn remaining_poll(&self) -> io::Result<Duration> {
        self.check_authentication_deadline()?;
        self.deadline
            .and_then(|deadline| deadline.checked_duration_since(Instant::now()))
            .filter(|remaining| !remaining.is_zero())
            .map(|remaining| remaining.min(SHUTDOWN_POLL_INTERVAL))
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::TimedOut,
                    "daemon authentication deadline exceeded",
                )
            })
    }

    pub(super) fn authenticated(&mut self) -> io::Result<()> {
        self.check_authentication_deadline()?;
        self.deadline = None;
        Ok(())
    }

    pub(super) fn set_read_timeout(&self, timeout: Option<Duration>) -> io::Result<()> {
        self.stream.set_read_timeout(timeout)
    }

    pub(super) fn set_write_timeout(&self, timeout: Option<Duration>) -> io::Result<()> {
        self.stream.set_write_timeout(timeout)
    }
}

fn retryable(error: &io::Error) -> bool {
    matches!(
        error.kind(),
        io::ErrorKind::WouldBlock | io::ErrorKind::TimedOut | io::ErrorKind::Interrupted
    )
}

impl Read for HandshakeStream {
    fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
        if self.deadline.is_none() {
            return self.stream.read(buffer);
        }
        loop {
            self.stream.set_read_timeout(Some(self.remaining_poll()?))?;
            match self.stream.read(buffer) {
                Err(error) if retryable(&error) => continue,
                result => {
                    self.check_authentication_deadline()?;
                    return result;
                }
            }
        }
    }
}

impl Write for HandshakeStream {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        if self.deadline.is_none() {
            return self.stream.write(buffer);
        }
        loop {
            self.stream
                .set_write_timeout(Some(self.remaining_poll()?))?;
            match self.stream.write(buffer) {
                Err(error) if retryable(&error) => continue,
                result => {
                    self.check_authentication_deadline()?;
                    return result;
                }
            }
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        self.check_authentication_deadline()?;
        self.stream.flush()
    }
}

#[cfg(test)]
#[path = "auth_tests.rs"]
mod tests;
