//! Runtime-owned replay storage. Only the small in-flight page lives in memory.
//!
//! These files are a journal for a live daemon epoch, not a second conversation
//! database. Closing/replacing the runtime drops them; a daemon restart changes
//! the epoch and the provider's normal history remains its durable authority.

use std::fs::{self, File, OpenOptions};
use std::io::{Read, Seek, SeekFrom, Write};
use std::path::PathBuf;

use anyhow::{Context as _, bail};
use uuid::Uuid;

use crate::protocol::{MAX_WIRE_MESSAGE_BYTES, SequencedEvent};

pub(super) const MAX_REPLAY_PAGE_EVENTS: usize = 256;
const MAX_REPLAY_PAGE_BYTES: u64 = 2 * 1024 * 1024;
const INDEX_ENTRY_BYTES: u64 = 24;

#[derive(Default)]
pub(super) struct ReplayLog {
    files: Option<JournalFiles>,
    failure: Option<String>,
}

struct JournalFiles {
    data: File,
    index: File,
    directory: PathBuf,
    entries: u64,
    data_bytes: u64,
}

struct DirectoryGuard(Option<PathBuf>);

impl Drop for DirectoryGuard {
    fn drop(&mut self) {
        // Every name is constant inside a fresh private directory we created.
        // Never recursively remove a path derived from a client request.
        if let Some(directory) = &self.0 {
            let _ = fs::remove_file(directory.join("events"));
            let _ = fs::remove_file(directory.join("index"));
            let _ = fs::remove_dir(directory);
        }
    }
}

impl Drop for ReplayLog {
    fn drop(&mut self) {
        if let Some(files) = self.files.take() {
            let cleanup = DirectoryGuard(Some(files.directory.clone()));
            drop(files);
            drop(cleanup);
        }
    }
}

impl JournalFiles {
    fn create() -> anyhow::Result<Self> {
        let directory = std::env::temp_dir().join(format!("waku-replay-{}", Uuid::new_v4()));
        let mut builder = fs::DirBuilder::new();
        #[cfg(unix)]
        {
            use std::os::unix::fs::DirBuilderExt as _;
            builder.mode(0o700);
        }
        builder
            .create(&directory)
            .context("could not create private replay storage")?;
        let mut cleanup = DirectoryGuard(Some(directory.clone()));
        let open = |name: &str| -> anyhow::Result<File> {
            let mut options = OpenOptions::new();
            options.create_new(true).read(true).write(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt as _;
                options.mode(0o600);
            }
            #[cfg(windows)]
            {
                use std::os::windows::fs::OpenOptionsExt as _;
                // FILE_ATTRIBUTE_TEMPORARY | FILE_FLAG_DELETE_ON_CLOSE, as
                // used by tempfile's anonymous Windows file implementation.
                // Exclusive sharing also keeps another process from opening
                // conversation contents while this daemon owns the journal.
                options.share_mode(0).custom_flags(0x0400_0100);
            }
            let file = options
                .open(directory.join(name))
                .context("could not open replay storage")?;
            #[cfg(unix)]
            fs::remove_file(directory.join(name))
                .context("could not unlink private replay storage")?;
            Ok(file)
        };
        let data = open("events")?;
        let index = open("index")?;
        // ReplayLog now owns cleanup after its file handles have closed.
        cleanup.0 = None;
        Ok(Self {
            data,
            index,
            directory,
            entries: 0,
            data_bytes: 0,
        })
    }

    fn entry(&mut self, position: u64) -> anyhow::Result<(u64, u64, u64)> {
        let offset = position
            .checked_mul(INDEX_ENTRY_BYTES)
            .context("replay index overflow")?;
        self.index.seek(SeekFrom::Start(offset))?;
        let mut bytes = [0u8; INDEX_ENTRY_BYTES as usize];
        self.index
            .read_exact(&mut bytes)
            .context("replay index is incomplete")?;
        let sequence = u64::from_le_bytes(bytes[..8].try_into().unwrap());
        let offset = u64::from_le_bytes(bytes[8..16].try_into().unwrap());
        let length = u64::from_le_bytes(bytes[16..].try_into().unwrap());
        anyhow::ensure!(
            length > 0 && length <= MAX_WIRE_MESSAGE_BYTES as u64,
            "invalid replay event size"
        );
        anyhow::ensure!(
            offset
                .checked_add(length)
                .is_some_and(|end| end <= self.data_bytes),
            "replay event is incomplete"
        );
        Ok((sequence, offset, length))
    }

    fn append(&mut self, event: &SequencedEvent, bytes: &[u8]) -> anyhow::Result<()> {
        let length = u64::try_from(bytes.len())?;
        let mut entry = [0u8; INDEX_ENTRY_BYTES as usize];
        entry[..8].copy_from_slice(&event.sequence.to_le_bytes());
        entry[8..16].copy_from_slice(&self.data_bytes.to_le_bytes());
        entry[16..].copy_from_slice(&length.to_le_bytes());
        self.data.seek(SeekFrom::Start(self.data_bytes))?;
        self.data
            .write_all(bytes)
            .context("could not retain replay event")?;
        self.index
            .seek(SeekFrom::Start(self.entries * INDEX_ENTRY_BYTES))?;
        self.index
            .write_all(&entry)
            .context("could not retain replay position")?;
        self.data_bytes = self
            .data_bytes
            .checked_add(length)
            .context("replay size overflow")?;
        self.entries = self
            .entries
            .checked_add(1)
            .context("replay count overflow")?;
        Ok(())
    }

    fn page(
        &mut self,
        after: u64,
        through: u64,
        limit: usize,
    ) -> anyhow::Result<(Vec<SequencedEvent>, bool)> {
        // Search the on-disk fixed-width index; there is no in-memory offset
        // vector whose size grows with a long-running/offline conversation.
        let (mut low, mut high) = (0, self.entries);
        while low < high {
            let middle = low + (high - low) / 2;
            if self.entry(middle)?.0 <= after {
                low = middle + 1;
            } else {
                high = middle;
            }
        }
        let mut events = Vec::with_capacity(limit);
        let mut bytes = 0u64;
        for position in low..self.entries {
            let (sequence, offset, length) = self.entry(position)?;
            if sequence > through {
                break;
            }
            if events.len() == limit
                || (!events.is_empty() && bytes + length > MAX_REPLAY_PAGE_BYTES)
            {
                return Ok((events, true));
            }
            let mut encoded = vec![0u8; usize::try_from(length)?];
            self.data.seek(SeekFrom::Start(offset))?;
            self.data
                .read_exact(&mut encoded)
                .context("replay event is incomplete")?;
            let event: SequencedEvent =
                serde_json::from_slice(&encoded).context("replay event is invalid")?;
            anyhow::ensure!(
                event.sequence == sequence && sequence > after,
                "replay event position changed"
            );
            events.push(event);
            bytes += length;
        }
        Ok((events, false))
    }
}

impl ReplayLog {
    pub(super) fn append(&mut self, event: &SequencedEvent) {
        if self.failure.is_some() {
            return;
        }
        let result = (|| {
            let bytes = serde_json::to_vec(event)?;
            // Leave room for the replay response's envelope and framing.
            anyhow::ensure!(
                bytes.len() <= MAX_WIRE_MESSAGE_BYTES / 2,
                "replay event exceeds wire limit"
            );
            if self.files.is_none() {
                self.files = Some(JournalFiles::create()?);
            }
            self.files.as_mut().unwrap().append(event, &bytes)
        })();
        if let Err(error) = result {
            // Keep forwarding live output, but never answer a future replay
            // with an incomplete prefix and pretend its cursor is current.
            self.failure = Some(format!("runtime replay storage failed: {error:#}"));
        }
    }

    pub(super) fn page(
        &mut self,
        after: u64,
        through: u64,
        limit: usize,
    ) -> anyhow::Result<(Vec<SequencedEvent>, bool)> {
        if let Some(error) = &self.failure {
            bail!("{error}");
        }
        anyhow::ensure!(
            after <= through && (1..=MAX_REPLAY_PAGE_EVENTS).contains(&limit),
            "invalid replay page bounds"
        );
        let Some(files) = &mut self.files else {
            return Ok((Vec::new(), false));
        };
        let result = files.page(after, through, limit);
        if let Err(error) = &result {
            self.failure = Some(format!("runtime replay storage failed: {error:#}"));
        }
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::WireDriverEvent;

    fn event(sequence: u64, payload: serde_json::Value) -> SequencedEvent {
        SequencedEvent {
            session_id: Uuid::nil(),
            runtime_id: Uuid::nil(),
            epoch: Uuid::nil(),
            sequence,
            replayable: true,
            event: WireDriverEvent::new("textDelta", payload),
        }
    }

    #[test]
    fn replay_log_pages_beyond_the_old_memory_window_and_keeps_a_fixed_watermark() {
        let mut log = ReplayLog::default();
        for sequence in 1..=5000 {
            log.append(&event(sequence, serde_json::json!(sequence)));
        }
        let mut next = 0;
        let mut count = 0;
        loop {
            let (page, more) = log.page(next, 4097, 256).unwrap();
            assert!(page.len() <= 256);
            for e in &page {
                assert_eq!(e.sequence, next + 1);
                next = e.sequence;
            }
            count += page.len();
            if !more {
                break;
            }
        }
        assert_eq!(count, 4097);
        assert_eq!(next, 4097);
        assert_eq!(log.page(next, 5000, 256).unwrap().0[0].sequence, 4098);
    }

    #[test]
    fn replay_log_preserves_intentional_ephemeral_sequence_holes() {
        let mut log = ReplayLog::default();
        log.append(&event(2, serde_json::json!("a")));
        log.append(&event(5, serde_json::json!("b")));
        let (page, more) = log.page(0, 8, 256).unwrap();
        assert_eq!(page.iter().map(|e| e.sequence).collect::<Vec<_>>(), [2, 5]);
        assert!(!more);
        assert!(log.page(5, 8, 256).unwrap().0.is_empty());
    }

    #[test]
    fn replay_log_bounds_page_bytes_without_losing_large_events() {
        let mut log = ReplayLog::default();
        log.append(&event(1, serde_json::json!("x".repeat(1_200_000))));
        log.append(&event(2, serde_json::json!("y".repeat(1_200_000))));
        let (page, more) = log.page(0, 2, 256).unwrap();
        assert_eq!(page.len(), 1);
        assert!(more);
        assert_eq!(log.page(1, 2, 256).unwrap().0[0].sequence, 2);
    }

    #[test]
    fn replay_log_corruption_is_an_error_never_a_short_successful_page() {
        let mut log = ReplayLog::default();
        log.append(&event(1, serde_json::json!("a")));
        log.files.as_mut().unwrap().data.set_len(0).unwrap();
        assert!(log.page(0, 1, 256).is_err());
        log.append(&event(2, serde_json::json!("b")));
        assert!(log.page(1, 2, 256).is_err());
    }

    #[test]
    fn replay_log_removes_private_files_when_runtime_is_released() {
        let mut log = ReplayLog::default();
        log.append(&event(1, serde_json::json!("a")));
        let directory = log.files.as_ref().unwrap().directory.clone();
        assert!(directory.is_dir());
        drop(log);
        assert!(!directory.exists());
    }
}
