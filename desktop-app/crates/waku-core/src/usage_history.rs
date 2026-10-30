//! Historical token usage for the settings Usage page, scanned from
//! the provider CLIs' own on-disk session transcripts (Claude Code's
//! `~/.claude/projects` and Codex's `~/.codex/sessions`) the way T3 Code and
//! `ccusage` do it, so usage covers turns driven outside Waku too.
//!
//! Everything here blocks on the filesystem and must run on the background
//! executor. Render reads only the
//! [`UsageHistory`] snapshot the app entity stores. Transcripts are
//! append-only, so parsed records are memoised per file by `(size, mtime)` in
//! a [`ScanCache`] the caller owns; warm rescans only reparse changed files.

use std::collections::{HashMap, HashSet};
use std::io::BufRead as _;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use chrono::{Datelike as _, Local, NaiveDate, TimeZone as _, Utc};
use serde_json::Value;

pub use waku_protocol::usage_history::{
    DaySlice, MONTHLY_WINDOW, ModelSlice, MonthSlice, ProjectSlice, ProviderDay, ProviderSlice,
    TokenTotals, UsageHistory, UsageProvider, UsageWindow, WINDOW_CHOICES,
};

/// Files whose mtime predates the window start by more than this are skipped
/// without opening. The slack covers a session whose last write lands just
/// before local midnight on the window's first day.
const MTIME_SLACK: Duration = Duration::from_secs(36 * 3600);

/// One-time compatibility cleanup. This exact file contained only downloaded
/// model prices; transcripts and token scan data are never traversed.
pub(crate) fn remove_legacy_price_cache(state_directory: &Path) -> std::io::Result<()> {
    match std::fs::remove_file(state_directory.join("usage-model-rates.json")) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

#[cfg(test)]
#[test]
fn legacy_price_cleanup_preserves_other_usage_files() {
    let directory =
        std::env::temp_dir().join(format!("waku-price-migration-{}", uuid::Uuid::new_v4()));
    std::fs::create_dir(&directory).unwrap();
    std::fs::write(directory.join("usage-model-rates.json"), b"obsolete").unwrap();
    std::fs::write(directory.join("usage-history.json"), b"tokens").unwrap();
    remove_legacy_price_cache(&directory).unwrap();
    remove_legacy_price_cache(&directory).unwrap();
    assert!(!directory.join("usage-model-rates.json").exists());
    assert_eq!(
        std::fs::read(directory.join("usage-history.json")).unwrap(),
        b"tokens"
    );
    std::fs::remove_file(directory.join("usage-history.json")).unwrap();
    std::fs::remove_dir(directory).unwrap();
}

/// One usage event parsed out of a transcript line.
#[derive(Clone, Debug)]
pub struct UsageRecord {
    pub provider: UsageProvider,
    pub timestamp_ms: i64,
    pub model: String,
    pub session_id: String,
    /// The working directory the session ran in, as recorded by the CLI —
    /// the per-project view's grouping key. Empty when the transcript does
    /// not say.
    pub project: String,
    pub totals: TokenTotals,
    /// Key for cross-file de-duplication, or `None` when the record is
    /// inherently unique and needs no dedup.
    pub dedupe_key: Option<String>,
}

/// Cheap substring gate applied before JSON parsing. Transcripts are mostly
/// tool output; only a minority of lines carry usage, and skipping the rest
/// before `serde_json` sees them is worth an order of magnitude.
fn might_carry_usage(line: &str, provider: UsageProvider) -> bool {
    match provider {
        UsageProvider::Claude => line.contains("\"usage\""),
        UsageProvider::Codex => line.contains("\"token_count\""),
    }
}

/// Positive finite number as a token count; anything else is zero.
fn int(value: Option<&Value>) -> u64 {
    value
        .and_then(Value::as_f64)
        .filter(|value| value.is_finite() && *value > 0.0)
        .map(|value| value.trunc() as u64)
        .unwrap_or(0)
}

fn parse_timestamp_ms(value: Option<&Value>) -> Option<i64> {
    let text = value?.as_str()?;
    chrono::DateTime::parse_from_rfc3339(text)
        .ok()
        .map(|date| date.timestamp_millis())
}

/// Parses one line of a Claude Code transcript.
///
/// The CLI writes one record per assistant *content block*, and every one of
/// those records repeats the same complete `usage` object for the parent
/// message. Summing them overcounts severely, so callers must drop repeats by
/// `dedupe_key` and keep the first.
fn parse_claude_line(line: &str) -> Option<UsageRecord> {
    let record: Value = serde_json::from_str(line).ok()?;
    if record.get("type").and_then(Value::as_str) != Some("assistant") {
        return None;
    }
    let message = record.get("message")?.as_object()?;
    let usage = message.get("usage")?.as_object()?;
    let timestamp_ms = parse_timestamp_ms(record.get("timestamp"))?;
    let model = message.get("model").and_then(Value::as_str)?;
    if model.is_empty() {
        return None;
    }

    let message_id = message.get("id").and_then(Value::as_str);
    let request_id = record.get("requestId").and_then(Value::as_str);
    // Matches ccusage: prefer the message/request pair, fall back to whichever
    // half exists. Records with neither cannot be de-duplicated.
    let dedupe_key = (message_id.is_some() || request_id.is_some()).then(|| {
        format!(
            "{}:{}",
            message_id.unwrap_or_default(),
            request_id.unwrap_or_default()
        )
    });

    Some(UsageRecord {
        provider: UsageProvider::Claude,
        timestamp_ms,
        model: model.to_owned(),
        session_id: record
            .get("sessionId")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        project: record
            .get("cwd")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        totals: TokenTotals {
            uncached_input: int(usage.get("input_tokens")),
            cached_input: int(usage.get("cache_read_input_tokens")),
            cache_creation: int(usage.get("cache_creation_input_tokens")),
            output: int(usage.get("output_tokens")),
            // Anthropic folds thinking tokens into output without a breakout.
            reasoning: 0,
        },
        dedupe_key,
    })
}

/// Rolling state for a single Codex rollout file. `token_count` events carry
/// no model, so the model is carried forward from the most recent
/// `turn_context`; sessions that switch models mid-run attribute correctly
/// from the switch onward.
#[derive(Clone, Copy)]
enum CodexForkPrefix {
    NotForked,
    /// The first meta marks a fork, but no copied ancestor meta has confirmed
    /// that this physical rollout actually contains a replay prefix.
    AwaitingReplay {
        fork_ms: Option<i64>,
    },
    /// Leading usage after a copied ancestor meta is re-stamped parent history.
    Suppressing {
        anchor_ms: i64,
    },
    Complete,
}

struct CodexScanState {
    model: String,
    session_id: String,
    /// Carried from `session_meta`/`turn_context`, which both record the
    /// working directory.
    cwd: String,
    last_usage_signature: Option<String>,
    saw_session_meta: bool,
    fork_prefix: CodexForkPrefix,
}

impl CodexScanState {
    fn new() -> Self {
        Self {
            model: String::new(),
            session_id: String::new(),
            cwd: String::new(),
            last_usage_signature: None,
            saw_session_meta: false,
            fork_prefix: CodexForkPrefix::NotForked,
        }
    }
}

/// Copied history is written synchronously, while the child's first usage
/// follows a real model turn. This is only a fallback for rollout formats that
/// do not repeat the child's own `session_meta` at the structural boundary.
const FORK_COPY_MAX_GAP_MS: i64 = 1_000;

fn codex_session_id(payload: &serde_json::Map<String, Value>) -> Option<&str> {
    payload
        .get("id")
        .or_else(|| payload.get("session_id"))
        .and_then(Value::as_str)
}

fn is_forked_codex_session(payload: &serde_json::Map<String, Value>) -> bool {
    payload
        .get("forked_from_id")
        .and_then(Value::as_str)
        .is_some()
        || payload
            .get("source")
            .and_then(|source| source.get("subagent"))
            .and_then(|subagent| subagent.get("thread_spawn"))
            .and_then(|spawn| spawn.get("parent_thread_id"))
            .and_then(Value::as_str)
            .is_some()
}

/// Feeds one line of a Codex rollout into `state`, returning a record when the
/// line was a usage event. Deltas come from `last_token_usage`; summing those
/// across a session reconciles with the session's final `total_token_usage`
/// provided consecutive duplicate events are dropped, which this does.
fn parse_codex_line(line: &str, state: &mut CodexScanState) -> Option<UsageRecord> {
    let record: Value = serde_json::from_str(line).ok()?;
    let payload = record.get("payload")?.as_object()?;

    match record.get("type").and_then(Value::as_str) {
        Some("session_meta") => {
            let id = codex_session_id(payload);
            if !state.saw_session_meta {
                // The first meta names the physical rollout. Forks replay
                // ancestor metas after it, and those must never replace the
                // child's identity or working directory.
                state.saw_session_meta = true;
                if let Some(id) = id {
                    state.session_id = id.to_owned();
                }
                if let Some(cwd) = payload.get("cwd").and_then(Value::as_str) {
                    state.cwd = cwd.to_owned();
                }
                state.fork_prefix = if is_forked_codex_session(payload) {
                    CodexForkPrefix::AwaitingReplay {
                        fork_ms: parse_timestamp_ms(record.get("timestamp")),
                    }
                } else {
                    CodexForkPrefix::NotForked
                };
                return None;
            }

            match state.fork_prefix {
                CodexForkPrefix::AwaitingReplay { fork_ms } => {
                    if id == Some(state.session_id.as_str()) {
                        // Some Codex writers repeat the child's meta when copied
                        // history ends. This is the authoritative boundary.
                        state.fork_prefix = CodexForkPrefix::Complete;
                    } else if id.is_some() {
                        // A different meta confirms that ancestor history really
                        // follows. A fork marker alone is not enough: subagents can
                        // start with no copied prefix and may report usage quickly.
                        if let Some(anchor_ms) =
                            parse_timestamp_ms(record.get("timestamp")).or(fork_ms)
                        {
                            state.fork_prefix = CodexForkPrefix::Suppressing { anchor_ms };
                        }
                    }
                }
                CodexForkPrefix::Suppressing { anchor_ms } => {
                    if id == Some(state.session_id.as_str()) {
                        state.fork_prefix = CodexForkPrefix::Complete;
                    } else if id.is_some() {
                        state.fork_prefix = CodexForkPrefix::Suppressing {
                            anchor_ms: parse_timestamp_ms(record.get("timestamp"))
                                .unwrap_or(anchor_ms),
                        };
                    }
                }
                CodexForkPrefix::NotForked | CodexForkPrefix::Complete => {}
            }
            return None;
        }
        Some("turn_context") => {
            if let Some(model) = payload.get("model").and_then(Value::as_str) {
                state.model = model.to_owned();
            }
            if let Some(cwd) = payload.get("cwd").and_then(Value::as_str) {
                state.cwd = cwd.to_owned();
            }
            return None;
        }
        _ => {}
    }

    if payload.get("type").and_then(Value::as_str) != Some("token_count") {
        return None;
    }
    let last = payload.get("info")?.get("last_token_usage")?.as_object()?;

    // Only an event that is otherwise eligible may consume the duplicate
    // signature. A token_count arriving before its turn_context (no model yet)
    // must not poison it, or the re-emitted copy after the model is known
    // would be skipped as a duplicate and those tokens never counted.
    let timestamp_ms = parse_timestamp_ms(record.get("timestamp"))?;
    if state.model.is_empty() {
        return None;
    }

    // Codex re-emits an unchanged token_count on some stream boundaries.
    // Summing those would double count, so identical consecutive payloads are
    // skipped.
    let signature = serde_json::to_string(last).ok()?;
    if state.last_usage_signature.as_deref() == Some(signature.as_str()) {
        return None;
    }
    state.last_usage_signature = Some(signature);

    if let CodexForkPrefix::Suppressing { anchor_ms } = state.fork_prefix {
        if timestamp_ms.saturating_sub(anchor_ms) < FORK_COPY_MAX_GAP_MS {
            state.fork_prefix = CodexForkPrefix::Suppressing {
                anchor_ms: timestamp_ms,
            };
            return None;
        }
        state.fork_prefix = CodexForkPrefix::Complete;
    }

    let input = int(last.get("input_tokens"));
    let cached_input = int(last.get("cached_input_tokens"));
    let cache_creation = int(last.get("cache_write_input_tokens"));
    let output = int(last.get("output_tokens"));
    let totals = TokenTotals {
        // Codex reports `input_tokens` inclusive of the cached portion.
        uncached_input: input.saturating_sub(cached_input + cache_creation),
        cached_input,
        cache_creation,
        output,
        // Reported inside output_tokens, surfaced separately for the mix.
        reasoning: int(last.get("reasoning_output_tokens")).min(output),
    };
    if totals.total() == 0 {
        return None;
    }
    if matches!(state.fork_prefix, CodexForkPrefix::AwaitingReplay { .. }) {
        // Once this physical rollout emits real usage, a replay prefix cannot
        // begin later in the file.
        state.fork_prefix = CodexForkPrefix::Complete;
    }

    Some(UsageRecord {
        provider: UsageProvider::Codex,
        timestamp_ms,
        model: state.model.clone(),
        session_id: state.session_id.clone(),
        project: state.cwd.clone(),
        totals,
        // Fork copies were suppressed while parsing the physical rollout, so
        // surviving events are unique and need no global deduplication.
        dedupe_key: None,
    })
}

fn unix_time_ms() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|elapsed| elapsed.as_millis() as i64)
        .unwrap_or(0)
}

/* ------------------------------------------------------------------------- */
/* Scanning                                                                  */
/* ------------------------------------------------------------------------- */

/// Parsed records for one transcript, memoised by `(size, mtime, provider)`.
/// Records are stored unfiltered by window; the aggregation applies the day
/// filter, so one cache serves every window length.
pub struct FileCacheEntry {
    size: u64,
    mtime_ms: i64,
    provider: UsageProvider,
    records: Vec<UsageRecord>,
}

pub type ScanCache = HashMap<PathBuf, FileCacheEntry>;

/// The transcript root scanned for one provider.
fn provider_root(provider: UsageProvider) -> Option<PathBuf> {
    match provider {
        UsageProvider::Claude => match std::env::var_os("CLAUDE_CONFIG_DIR") {
            Some(dir) if !dir.is_empty() => Some(PathBuf::from(dir).join("projects")),
            _ => dirs::home_dir().map(|home| home.join(".claude/projects")),
        },
        UsageProvider::Codex => match std::env::var_os("CODEX_HOME") {
            Some(dir) if !dir.is_empty() => Some(PathBuf::from(dir).join("sessions")),
            _ => dirs::home_dir().map(|home| home.join(".codex/sessions")),
        },
    }
}

/// Lists `.jsonl` transcripts under `root` modified at or after `since_ms`.
/// Errors on individual entries are swallowed: session files rotate and get
/// removed while the walk is in flight, and a partial listing beats failing
/// the page. Returns the number of files skipped by the mtime prefilter.
fn list_transcript_files(
    root: &Path,
    since_ms: i64,
    found: &mut Vec<(PathBuf, u64, i64)>,
) -> usize {
    let mut skipped = 0;
    let Ok(entries) = std::fs::read_dir(root) else {
        return 0;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let Ok(file_type) = entry.file_type() else {
            continue;
        };
        if file_type.is_dir() {
            skipped += list_transcript_files(&path, since_ms, found);
            continue;
        }
        if path.extension().and_then(|extension| extension.to_str()) != Some("jsonl") {
            continue;
        }
        let Ok(metadata) = entry.metadata() else {
            continue;
        };
        let mtime_ms = metadata
            .modified()
            .ok()
            .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
            .map(|elapsed| elapsed.as_millis() as i64)
            .unwrap_or(0);
        if mtime_ms < since_ms {
            skipped += 1;
            continue;
        }
        found.push((path, metadata.len(), mtime_ms));
    }
    skipped
}

/// Streams one transcript and returns the usage records it contains, already
/// de-duplicated within the file, or `None` when it could not be read. The
/// distinction matters to the cache: an empty transcript is a stable fact
/// worth memoising, while a transient read failure memoised under the same
/// `(size, mtime)` would silently drop the file's usage until it changes.
fn read_transcript_records(path: &Path, provider: UsageProvider) -> Option<Vec<UsageRecord>> {
    let file = std::fs::File::open(path).ok()?;
    let mut reader = std::io::BufReader::new(file);
    let mut line = String::new();
    let mut records = Vec::new();
    let mut codex_state = CodexScanState::new();
    let mut seen_in_file = HashSet::new();

    loop {
        line.clear();
        match reader.read_line(&mut line) {
            Ok(0) => break,
            Ok(_) => {}
            Err(_) => return None,
        }
        match provider {
            UsageProvider::Codex => {
                // Codex carries the active model on `turn_context` lines that
                // hold no usage of their own, so those still pass through the
                // reducer to keep model attribution correct.
                if !might_carry_usage(&line, provider)
                    && !line.contains("\"turn_context\"")
                    && !line.contains("\"session_meta\"")
                {
                    continue;
                }
                if let Some(record) = parse_codex_line(&line, &mut codex_state) {
                    records.push(record);
                }
            }
            UsageProvider::Claude => {
                if !might_carry_usage(&line, provider) {
                    continue;
                }
                if let Some(record) = parse_claude_line(&line) {
                    // Every assistant content block repeats the parent
                    // message's usage; the first record wins. Cross-file
                    // repeats (resumed or forked sessions) are handled by the
                    // aggregator's global pass.
                    if let Some(key) = &record.dedupe_key {
                        if !seen_in_file.insert(key.clone()) {
                            continue;
                        }
                    }
                    records.push(record);
                }
            }
        }
    }
    Some(records)
}

/* ------------------------------------------------------------------------- */
/* Aggregation                                                               */
/* ------------------------------------------------------------------------- */

#[derive(Default)]
struct Bucket {
    totals: TokenTotals,
    records: u64,
}

/// Per-project accumulation, keyed by the resolved project path.
#[derive(Default)]
struct ProjectAccumulator {
    total_tokens: u64,
    by_provider: [ProviderDay; 2],
    sessions: HashSet<(UsageProvider, String)>,
    /// Tokens per model, for the row's "top models" caption.
    models: HashMap<String, u64>,
    last_day: Option<NaiveDate>,
}

/// Folds records into `(day, provider, model)` buckets with global cross-file
/// de-duplication, then derives the page's view.
struct Aggregator {
    since_day: NaiveDate,
    until_day: NaiveDate,
    /// Known project roots, longest path first. A record whose working
    /// directory sits under a root is attributed to that root, so launches
    /// from a repo's subdirectories and worktrees inside it merge into one
    /// project row instead of splintering by cwd.
    project_roots: Vec<PathBuf>,
    buckets: HashMap<(NaiveDate, UsageProvider, String), Bucket>,
    seen: HashSet<String>,
    sessions: HashSet<(UsageProvider, String)>,
    /// Sessions per calendar month (keyed by the month's first day). A
    /// session spanning two months counts in both, which is what "sessions
    /// that month" means.
    month_sessions: HashSet<(NaiveDate, UsageProvider, String)>,
    projects: HashMap<String, ProjectAccumulator>,
}

impl Aggregator {
    fn new(since_day: NaiveDate, until_day: NaiveDate, project_roots: &[PathBuf]) -> Self {
        let mut project_roots = project_roots.to_vec();
        project_roots.sort_by_key(|root| std::cmp::Reverse(root.as_os_str().len()));
        Self {
            since_day,
            until_day,
            project_roots,
            buckets: HashMap::new(),
            seen: HashSet::new(),
            sessions: HashSet::new(),
            month_sessions: HashSet::new(),
            projects: HashMap::new(),
        }
    }

    /// The project a working directory belongs to: the longest known root
    /// containing it, else the directory itself.
    fn resolve_project(&self, cwd: &str) -> String {
        if cwd.is_empty() {
            return String::new();
        }
        let path = Path::new(cwd);
        self.project_roots
            .iter()
            .find(|root| path.starts_with(root))
            .map(|root| root.display().to_string())
            .unwrap_or_else(|| cwd.to_owned())
    }

    fn add(&mut self, record: &UsageRecord) {
        if let Some(key) = &record.dedupe_key {
            if !self.seen.insert(key.clone()) {
                return;
            }
        }
        let Some(timestamp) = Utc.timestamp_millis_opt(record.timestamp_ms).single() else {
            return;
        };
        let day = timestamp.with_timezone(&Local).date_naive();
        if day < self.since_day || day > self.until_day {
            return;
        }

        let bucket = self
            .buckets
            .entry((day, record.provider, record.model.clone()))
            .or_default();
        bucket.totals.add(&record.totals);
        bucket.records += 1;
        if !record.session_id.is_empty() {
            self.sessions
                .insert((record.provider, record.session_id.clone()));
            self.month_sessions.insert((
                first_of_month(day),
                record.provider,
                record.session_id.clone(),
            ));
        }

        let tokens = record.totals.total();
        let project_key = self.resolve_project(&record.project);
        let project = self.projects.entry(project_key).or_default();
        project.total_tokens += tokens;
        project.by_provider[record.provider.index()].total_tokens += tokens;
        if !record.session_id.is_empty() {
            project
                .sessions
                .insert((record.provider, record.session_id.clone()));
        }
        *project.models.entry(record.model.clone()).or_default() += tokens;
        project.last_day = Some(project.last_day.map_or(day, |last| last.max(day)));
    }
}

/// Scan every provider's transcripts for `window` ending today and derive
/// the page snapshot. `project_roots` are the app's known project paths, used
/// to attribute working directories to projects. Blocking; run on the
/// background executor with the caller-owned `cache`.
pub fn scan(cache: &mut ScanCache, window: UsageWindow, project_roots: &[PathBuf]) -> UsageHistory {
    let started = Instant::now();
    let (since_day, until_day) = window.bounds(Local::now().date_naive());
    // Local midnight on the window's first day; a DST gap falls back to the
    // day boundary in UTC, which the mtime slack absorbs anyway.
    let window_start_ms = since_day
        .and_hms_opt(0, 0, 0)
        .and_then(|midnight| Local.from_local_datetime(&midnight).earliest())
        .map(|midnight| midnight.timestamp_millis())
        .unwrap_or_else(|| unix_time_ms() - (until_day - since_day).num_days().max(1) * 86_400_000);
    let mtime_cutoff_ms = window_start_ms - MTIME_SLACK.as_millis() as i64;

    let mut aggregator = Aggregator::new(since_day, until_day, project_roots);
    let mut scanned_files = 0;
    let mut skipped_files = 0;
    let mut errors = Vec::new();

    for provider in UsageProvider::ALL {
        let Some(root) = provider_root(provider) else {
            continue;
        };
        if !root.is_dir() {
            // Provider never used on this machine; zero usage, not an error.
            continue;
        }
        let mut files = Vec::new();
        skipped_files += list_transcript_files(&root, mtime_cutoff_ms, &mut files);
        if files.is_empty() && std::fs::read_dir(&root).is_err() {
            errors.push(format!(
                "{} transcripts at {} could not be read.",
                provider.label(),
                root.display()
            ));
            continue;
        }
        for (path, size, mtime_ms) in files {
            scanned_files += 1;
            let cached = cache.get(&path);
            // Provider is part of the identity: if both providers were ever
            // pointed at one directory, a hit parsed by the other parser must
            // not be reused.
            let records = match cached {
                Some(entry)
                    if entry.size == size
                        && entry.mtime_ms == mtime_ms
                        && entry.provider == provider =>
                {
                    &entry.records
                }
                _ => match read_transcript_records(&path, provider) {
                    Some(records) => {
                        &cache
                            .entry(path)
                            .insert_entry(FileCacheEntry {
                                size,
                                mtime_ms,
                                provider,
                                records,
                            })
                            .into_mut()
                            .records
                    }
                    // A read failure is not an empty transcript: caching it
                    // under this (size, mtime) would silently drop the file's
                    // usage until it changes.
                    None => continue,
                },
            };
            for record in records {
                aggregator.add(record);
            }
        }
    }

    derive_history(
        aggregator,
        window,
        since_day,
        until_day,
        scanned_files,
        skipped_files,
        errors,
        started.elapsed(),
    )
}

#[allow(clippy::too_many_arguments)]
fn derive_history(
    aggregator: Aggregator,
    window: UsageWindow,
    since_day: NaiveDate,
    until_day: NaiveDate,
    scanned_files: usize,
    skipped_files: usize,
    errors: Vec<String>,
    scan_duration: Duration,
) -> UsageHistory {
    let mut totals = TokenTotals::default();
    let mut records = 0;
    let mut providers: HashMap<UsageProvider, u64> = HashMap::new();
    let mut models: HashMap<(UsageProvider, String), u64> = HashMap::new();
    let mut daily: HashMap<NaiveDate, DaySlice> = HashMap::new();
    let mut month_models: HashMap<(NaiveDate, String), u64> = HashMap::new();

    for ((day, provider, model), bucket) in &aggregator.buckets {
        let tokens = bucket.totals.total();
        totals.add(&bucket.totals);
        records += bucket.records;
        *providers.entry(*provider).or_default() += tokens;
        *models.entry((*provider, model.clone())).or_default() += tokens;
        let day_entry = daily.entry(*day).or_insert_with(|| DaySlice {
            day: *day,
            total_tokens: 0,
            by_provider: [ProviderDay::default(); 2],
        });
        day_entry.total_tokens += tokens;
        day_entry.by_provider[provider.index()].total_tokens += tokens;
        *month_models
            .entry((first_of_month(*day), model.clone()))
            .or_default() += tokens;
    }

    let total_tokens = totals.total();
    let share = |part: u64| {
        if total_tokens == 0 {
            0.0
        } else {
            part as f64 / total_tokens as f64
        }
    };
    let mut provider_slices: Vec<ProviderSlice> = providers
        .into_iter()
        .map(|(provider, tokens)| ProviderSlice {
            provider,
            total_tokens: tokens,
            token_share: share(tokens),
        })
        .collect();
    provider_slices.sort_by(|a, b| {
        b.total_tokens
            .cmp(&a.total_tokens)
            .then(a.provider.index().cmp(&b.provider.index()))
    });
    let mut model_slices: Vec<ModelSlice> = models
        .into_iter()
        .map(|((provider, model), tokens)| ModelSlice {
            provider,
            model,
            total_tokens: tokens,
            token_share: share(tokens),
        })
        .collect();
    model_slices.sort_by(|a, b| {
        b.total_tokens
            .cmp(&a.total_tokens)
            .then(a.model.cmp(&b.model))
            .then(a.provider.index().cmp(&b.provider.index()))
    });

    let mut day_slices: Vec<DaySlice> = daily.into_values().collect();
    day_slices.sort_by_key(|slice| slice.day);

    // Months fold over the day slices; only sessions and the model caption
    // need their own record-level accumulators.
    let mut months: HashMap<NaiveDate, MonthSlice> = HashMap::new();
    for day in &day_slices {
        let month = months
            .entry(first_of_month(day.day))
            .or_insert_with(|| MonthSlice {
                first_day: first_of_month(day.day),
                total_tokens: 0,
                by_provider: [ProviderDay::default(); 2],
                sessions: 0,
                active_days: 0,
                top_models: Vec::new(),
            });
        month.total_tokens += day.total_tokens;
        for index in 0..month.by_provider.len() {
            month.by_provider[index].total_tokens += day.by_provider[index].total_tokens;
        }
        if day.total_tokens > 0 {
            month.active_days += 1;
        }
    }
    for (first_day, _, _) in &aggregator.month_sessions {
        if let Some(month) = months.get_mut(first_day) {
            month.sessions += 1;
        }
    }
    for ((first_day, model), model_tokens) in month_models {
        if let Some(month) = months.get_mut(&first_day) {
            month.top_models.push((model, model_tokens));
        }
    }
    let mut month_slices: Vec<MonthSlice> = months.into_values().collect();
    month_slices.sort_by_key(|slice| slice.first_day);
    for month in &mut month_slices {
        month
            .top_models
            .sort_by(|a, b| b.1.cmp(&a.1).then(a.0.cmp(&b.0)));
    }

    let mut project_slices: Vec<ProjectSlice> = aggregator
        .projects
        .into_iter()
        .map(|(path, project)| {
            let mut top_models: Vec<(String, u64)> = project.models.into_iter().collect();
            top_models.sort_by(|a, b| b.1.cmp(&a.1).then(a.0.cmp(&b.0)));
            ProjectSlice {
                path,
                total_tokens: project.total_tokens,
                by_provider: project.by_provider,
                sessions: project.sessions.len() as u64,
                token_share: share(project.total_tokens),
                last_day: project.last_day,
                top_models,
            }
        })
        .collect();
    project_slices.sort_by(|a, b| {
        b.total_tokens
            .cmp(&a.total_tokens)
            .then(a.path.cmp(&b.path))
    });

    UsageHistory {
        window,
        since_day,
        until_day,
        totals,
        total_tokens,
        records,
        sessions: aggregator.sessions.len() as u64,
        providers: provider_slices,
        models: model_slices,
        daily: day_slices,
        months: month_slices,
        projects: project_slices,
        scanned_files,
        skipped_files,
        errors,
        scan_duration,
    }
}

/// Inclusive day list between the window bounds, oldest first — the chart's
/// x-axis, including days with no activity.
pub fn enumerate_days(since_day: NaiveDate, until_day: NaiveDate) -> Vec<NaiveDate> {
    let mut days = Vec::new();
    let mut cursor = since_day;
    while cursor <= until_day {
        days.push(cursor);
        cursor = cursor + chrono::Days::new(1);
    }
    days
}

/// The first day of `day`'s calendar month.
pub fn first_of_month(day: NaiveDate) -> NaiveDate {
    day.with_day(1).unwrap_or(day)
}

/// Inclusive first-of-month list between the bounds' months, oldest first —
/// the statement view's rows, including months with no activity.
pub fn enumerate_months(since_day: NaiveDate, until_day: NaiveDate) -> Vec<NaiveDate> {
    let mut months = Vec::new();
    let mut cursor = first_of_month(since_day);
    let last = first_of_month(until_day);
    while cursor <= last {
        months.push(cursor);
        let Some(next) = cursor.checked_add_months(chrono::Months::new(1)) else {
            break;
        };
        cursor = next;
    }
    months
}

/// Number of days in `first_day`'s month.
pub fn days_in_month(first_day: NaiveDate) -> u32 {
    first_day
        .checked_add_months(chrono::Months::new(1))
        .and_then(|next| next.pred_opt())
        .map(|last| last.day())
        .unwrap_or(31)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn codex_meta(
        timestamp: &str,
        id: &str,
        cwd: &str,
        forked_from_id: Option<&str>,
        parent_thread_id: Option<&str>,
    ) -> String {
        let mut payload = serde_json::json!({ "id": id, "cwd": cwd });
        if let Some(parent) = forked_from_id {
            payload["forked_from_id"] = Value::String(parent.to_owned());
        }
        if let Some(parent) = parent_thread_id {
            payload["source"] = serde_json::json!({
                "subagent": { "thread_spawn": { "parent_thread_id": parent } }
            });
        }
        serde_json::json!({
            "timestamp": timestamp,
            "type": "session_meta",
            "payload": payload,
        })
        .to_string()
    }

    fn codex_context(timestamp: &str, cwd: &str) -> String {
        serde_json::json!({
            "timestamp": timestamp,
            "type": "turn_context",
            "payload": { "model": "gpt-5.3-codex", "cwd": cwd },
        })
        .to_string()
    }

    fn codex_count(timestamp: &str, input: u64, output: u64) -> String {
        serde_json::json!({
            "timestamp": timestamp,
            "type": "event_msg",
            "payload": {
                "type": "token_count",
                "info": {
                    "last_token_usage": {
                        "input_tokens": input,
                        "cached_input_tokens": 0,
                        "cache_write_input_tokens": 0,
                        "output_tokens": output,
                        "reasoning_output_tokens": 0,
                    }
                }
            },
        })
        .to_string()
    }

    #[test]
    fn claude_lines_parse_usage_and_dedupe_key() {
        // Shape captured from a live transcript on 2026-08-08.
        let line = r#"{"type":"assistant","timestamp":"2026-08-08T15:18:37.487Z",
            "requestId":"req_1","sessionId":"session-1","costUSD":123.45,
            "cwd":"/Users/me/dev/waku",
            "message":{"id":"msg_1","model":"claude-fable-5",
            "usage":{"input_tokens":2,"cache_creation_input_tokens":50700,
            "cache_read_input_tokens":0,"output_tokens":1238}}}"#
            .replace('\n', " ");
        let record = parse_claude_line(&line).expect("the line carries usage");
        assert_eq!(record.provider, UsageProvider::Claude);
        assert_eq!(record.model, "claude-fable-5");
        assert_eq!(record.session_id, "session-1");
        assert_eq!(record.project, "/Users/me/dev/waku");
        assert_eq!(record.dedupe_key.as_deref(), Some("msg_1:req_1"));
        assert_eq!(record.totals.uncached_input, 2);
        assert_eq!(record.totals.cache_creation, 50700);
        assert_eq!(record.totals.output, 1238);
        assert_eq!(record.totals.total(), 2 + 50700 + 1238);

        // Non-assistant lines and lines without usage parse to nothing.
        assert!(parse_claude_line(r#"{"type":"user","message":{}}"#).is_none());
    }

    #[test]
    fn codex_lines_carry_model_forward_and_skip_duplicates() {
        let mut state = CodexScanState::new();
        let meta = r#"{"timestamp":"2026-08-06T16:31:19.166Z","type":"session_meta",
            "payload":{"id":"codex-session","cwd":"/Users/me/dev/waku"}}"#
            .replace('\n', " ");
        let context = r#"{"timestamp":"2026-08-06T16:31:20.000Z","type":"turn_context",
            "payload":{"model":"gpt-5.3-codex"}}"#
            .replace('\n', " ");
        let count = r#"{"timestamp":"2026-08-06T16:31:25.000Z","type":"event_msg",
            "payload":{"type":"token_count","info":{"last_token_usage":{
            "input_tokens":21047,"cached_input_tokens":1000,"cache_write_input_tokens":47,
            "output_tokens":280,"reasoning_output_tokens":165}}}}"#
            .replace('\n', " ");

        // A token_count before any turn_context has no model and is dropped
        // without consuming the duplicate signature.
        assert!(parse_codex_line(&count, &mut state).is_none());
        assert!(parse_codex_line(&meta, &mut state).is_none());
        assert!(parse_codex_line(&context, &mut state).is_none());

        let record = parse_codex_line(&count, &mut state).expect("model is known now");
        assert_eq!(record.model, "gpt-5.3-codex");
        assert_eq!(record.session_id, "codex-session");
        assert_eq!(record.project, "/Users/me/dev/waku");
        // input_tokens includes the cached portion.
        assert_eq!(record.totals.uncached_input, 21047 - 1000 - 47);
        assert_eq!(record.totals.cached_input, 1000);
        assert_eq!(record.totals.cache_creation, 47);
        assert_eq!(record.totals.reasoning, 165);

        // The identical re-emitted event is a duplicate, not more usage.
        assert!(parse_codex_line(&count, &mut state).is_none());
    }

    #[test]
    fn codex_fork_uses_session_metas_as_the_copy_boundary() {
        let mut state = CodexScanState::new();
        parse_codex_line(
            &codex_meta(
                "2026-08-06T16:31:19.000Z",
                "child",
                "/child",
                Some("parent"),
                None,
            ),
            &mut state,
        );
        // Copied history is re-stamped to the fork instant. Its ancestor meta
        // confirms that the following usage is replay, while leaving the
        // physical child's identity and cwd intact.
        parse_codex_line(
            &codex_meta("2026-08-06T16:31:19.001Z", "parent", "/parent", None, None),
            &mut state,
        );
        parse_codex_line(
            &codex_context("2026-08-06T16:31:19.002Z", "/parent"),
            &mut state,
        );
        assert!(
            parse_codex_line(
                &codex_count("2026-08-06T16:31:19.003Z", 100, 10),
                &mut state,
            )
            .is_none()
        );

        // This writer repeats the child's meta at the exact structural
        // boundary. Genuine child usage must count even inside the fallback's
        // one-second window.
        parse_codex_line(
            &codex_meta(
                "2026-08-06T16:31:19.004Z",
                "child",
                "/child",
                Some("parent"),
                None,
            ),
            &mut state,
        );
        parse_codex_line(
            &codex_context("2026-08-06T16:31:19.050Z", "/child"),
            &mut state,
        );
        let child = parse_codex_line(
            &codex_count("2026-08-06T16:31:19.100Z", 300, 30),
            &mut state,
        )
        .expect("usage after the child's meta is genuine");
        assert_eq!(child.session_id, "child");
        assert_eq!(child.project, "/child");
        assert_eq!(child.totals.total(), 330);
    }

    #[test]
    fn codex_subagent_without_an_ancestor_meta_counts_fast_usage() {
        let mut state = CodexScanState::new();
        parse_codex_line(
            &codex_meta(
                "2026-08-06T16:31:19.000Z",
                "child",
                "/child",
                None,
                Some("parent"),
            ),
            &mut state,
        );
        parse_codex_line(
            &codex_context("2026-08-06T16:31:19.050Z", "/child"),
            &mut state,
        );
        let child = parse_codex_line(
            &codex_count("2026-08-06T16:31:19.100Z", 100, 10),
            &mut state,
        )
        .expect("a fork marker alone does not prove history was copied");
        assert_eq!(child.session_id, "child");
    }

    #[test]
    fn codex_fork_falls_back_to_the_gap_when_no_child_meta_repeats() {
        let mut state = CodexScanState::new();
        parse_codex_line(
            &codex_meta(
                "2026-08-06T16:31:19.000Z",
                "child",
                "/child",
                Some("parent"),
                None,
            ),
            &mut state,
        );
        parse_codex_line(
            &codex_meta("2026-08-06T16:31:19.001Z", "parent", "/parent", None, None),
            &mut state,
        );
        parse_codex_line(
            &codex_context("2026-08-06T16:31:19.001Z", "/parent"),
            &mut state,
        );
        assert!(
            parse_codex_line(
                &codex_count("2026-08-06T16:31:19.002Z", 100, 10),
                &mut state,
            )
            .is_none()
        );
        assert!(
            parse_codex_line(
                &codex_count("2026-08-06T16:31:19.040Z", 200, 20),
                &mut state,
            )
            .is_none()
        );

        let child = parse_codex_line(
            &codex_count("2026-08-06T16:31:20.040Z", 300, 30),
            &mut state,
        )
        .expect("a full-second gap ends copy suppression");
        assert_eq!(child.session_id, "child");
        assert_eq!(child.totals.total(), 330);
    }

    #[test]
    fn aggregation_dedupes_across_files_and_counts_tokens() {
        let record = UsageRecord {
            provider: UsageProvider::Claude,
            timestamp_ms: Local::now().timestamp_millis(),
            model: "claude-fable-5".to_owned(),
            session_id: "session-1".to_owned(),
            project: "/Users/me/dev/waku/crates/ui".to_owned(),
            totals: TokenTotals {
                uncached_input: 1_000,
                cached_input: 10_000,
                cache_creation: 2_000,
                output: 500,
                reasoning: 0,
            },
            dedupe_key: Some("msg:req".to_owned()),
        };
        let today = Local::now().date_naive();
        let roots = [PathBuf::from("/Users/me/dev/waku")];
        let mut aggregator = Aggregator::new(today - chrono::Days::new(29), today, &roots);
        aggregator.add(&record);
        // The same message copied into a forked session's transcript.
        aggregator.add(&record);

        let history = derive_history(
            aggregator,
            UsageWindow::TrailingDays(30),
            today - chrono::Days::new(29),
            today,
            1,
            0,
            Vec::new(),
            Duration::ZERO,
        );
        assert_eq!(history.records, 1);
        assert_eq!(history.sessions, 1);
        assert_eq!(history.total_tokens, 13_500);
        assert_eq!(history.daily.len(), 1);
        assert_eq!(history.providers.len(), 1);

        // The subdirectory cwd resolved to its containing project root, and
        // the month fold carries the same totals as the single active day.
        assert_eq!(history.projects.len(), 1);
        let project = &history.projects[0];
        assert_eq!(project.path, "/Users/me/dev/waku");
        assert_eq!(project.sessions, 1);
        assert_eq!(project.total_tokens, 13_500);
        assert!((project.token_share - 1.0).abs() < f64::EPSILON);
        assert_eq!(project.last_day, Some(today));
        assert_eq!(project.top_models[0].0, "claude-fable-5");
        assert_eq!(history.months.len(), 1);
        let month = &history.months[0];
        assert_eq!(month.first_day, first_of_month(today));
        assert_eq!(month.total_tokens, 13_500);
        assert_eq!(month.sessions, 1);
        assert_eq!(month.active_days, 1);
        assert_eq!(month.top_models[0].0, "claude-fable-5");
    }

    #[test]
    fn token_rankings_preserve_cache_and_reasoning_without_monetary_fields() {
        let today = Local::now().date_naive();
        let mut aggregator = Aggregator::new(today, today, &[]);
        for (provider, model, project, input, cached, output, reasoning) in [
            (
                UsageProvider::Claude,
                "claude-fable-5",
                "/small",
                100,
                200,
                50,
                0,
            ),
            (
                UsageProvider::Codex,
                "gpt-5.3-codex",
                "/large",
                400,
                500,
                100,
                40,
            ),
        ] {
            aggregator.add(&UsageRecord {
                provider,
                timestamp_ms: Local::now().timestamp_millis(),
                model: model.to_owned(),
                session_id: project.to_owned(),
                project: project.to_owned(),
                totals: TokenTotals {
                    uncached_input: input,
                    cached_input: cached,
                    output,
                    reasoning,
                    ..TokenTotals::default()
                },
                dedupe_key: None,
            });
        }
        let history = derive_history(
            aggregator,
            UsageWindow::TrailingDays(1),
            today,
            today,
            2,
            0,
            Vec::new(),
            Duration::ZERO,
        );
        assert_eq!(
            history.total_tokens, 1_350,
            "reasoning is already included in output"
        );
        assert_eq!(history.totals.cached_input, 700);
        assert_eq!(history.totals.reasoning, 40);
        assert_eq!(history.providers[0].provider, UsageProvider::Codex);
        assert_eq!(history.models[0].model, "gpt-5.3-codex");
        assert_eq!(history.projects[0].path, "/large");
        assert_eq!(
            history.months[0].top_models[0],
            ("gpt-5.3-codex".to_owned(), 1_000)
        );
        assert!(
            (history
                .providers
                .iter()
                .map(|entry| entry.token_share)
                .sum::<f64>()
                - 1.0)
                .abs()
                < 1e-9
        );
        let serialized = serde_json::to_string(&history).unwrap();
        for removed in [
            "costUsd",
            "costShare",
            "pricing",
            "quality",
            "cacheSavingsUsd",
        ] {
            assert!(!serialized.contains(removed), "obsolete field {removed}");
        }
    }

    #[test]
    fn months_and_projects_split_across_boundaries() {
        let today = Local::now().date_naive();
        // Two months back so both records always land inside the window even
        // on the first of a month.
        let since = first_of_month(today)
            .checked_sub_months(chrono::Months::new(2))
            .unwrap();
        let roots: [PathBuf; 0] = [];
        let mut aggregator = Aggregator::new(since, today, &roots);
        let record = |days_ago: i64, session: &str, project: &str| UsageRecord {
            provider: UsageProvider::Claude,
            timestamp_ms: Local::now().timestamp_millis() - days_ago * 86_400_000,
            model: "claude-fable-5".to_owned(),
            session_id: session.to_owned(),
            project: project.to_owned(),
            totals: TokenTotals {
                uncached_input: 100,
                ..TokenTotals::default()
            },
            dedupe_key: None,
        };
        aggregator.add(&record(0, "session-now", "/a"));
        aggregator.add(&record(45, "session-then", "/b"));

        let history = derive_history(
            aggregator,
            MONTHLY_WINDOW,
            since,
            today,
            0,
            0,
            Vec::new(),
            Duration::ZERO,
        );
        assert_eq!(history.months.len(), 2, "45 days apart spans two months");
        assert!(history.months[0].first_day < history.months[1].first_day);
        assert_eq!(history.months[1].sessions, 1);
        assert_eq!(history.projects.len(), 2);
        // Equal token totals are ordered by path; both carry half the total each.
        assert!((history.projects[0].token_share - 0.5).abs() < 1e-9);
        assert!(
            history
                .month(first_of_month(today))
                .is_some_and(|month| month.total_tokens == 100)
        );
    }

    #[test]
    fn out_of_window_records_are_excluded() {
        let today = Local::now().date_naive();
        let roots: [PathBuf; 0] = [];
        let mut aggregator = Aggregator::new(today, today, &roots);
        let mut record = UsageRecord {
            provider: UsageProvider::Codex,
            timestamp_ms: Local::now().timestamp_millis(),
            model: "gpt-5.3-codex".to_owned(),
            session_id: String::new(),
            project: String::new(),
            totals: TokenTotals {
                uncached_input: 100,
                ..TokenTotals::default()
            },
            dedupe_key: None,
        };
        aggregator.add(&record);
        // Ten days ago falls outside a one-day window.
        record.timestamp_ms -= 10 * 86_400_000;
        aggregator.add(&record);

        let history = derive_history(
            aggregator,
            UsageWindow::TrailingDays(1),
            today,
            today,
            0,
            0,
            Vec::new(),
            Duration::ZERO,
        );
        assert_eq!(history.records, 1);
        assert_eq!(history.sessions, 0, "an empty session id is not a session");
        assert_eq!(history.total_tokens, 100);
    }

    #[test]
    fn windows_and_month_enumeration_cover_calendars() {
        let day = NaiveDate::from_ymd_opt(2026, 8, 9).unwrap();
        let ymd =
            |year, month, day_of_month| NaiveDate::from_ymd_opt(year, month, day_of_month).unwrap();
        assert_eq!(
            UsageWindow::TrailingDays(7).bounds(day),
            (ymd(2026, 8, 3), day)
        );
        // Twelve months = this month plus the eleven before it.
        assert_eq!(UsageWindow::Months(12).bounds(day), (ymd(2025, 9, 1), day));
        assert_eq!(UsageWindow::ThisMonth.bounds(day), (ymd(2026, 8, 1), day));
        // The whole previous month, ending before today — across a year
        // boundary too.
        assert_eq!(
            UsageWindow::LastMonth.bounds(day),
            (ymd(2026, 7, 1), ymd(2026, 7, 31))
        );
        assert_eq!(
            UsageWindow::LastMonth.bounds(ymd(2026, 1, 15)),
            (ymd(2025, 12, 1), ymd(2025, 12, 31))
        );
        let months = enumerate_months(
            NaiveDate::from_ymd_opt(2025, 11, 15).unwrap(),
            NaiveDate::from_ymd_opt(2026, 2, 1).unwrap(),
        );
        assert_eq!(months.len(), 4);
        assert_eq!(months[0], NaiveDate::from_ymd_opt(2025, 11, 1).unwrap());
        assert_eq!(months[3], NaiveDate::from_ymd_opt(2026, 2, 1).unwrap());
        assert_eq!(
            days_in_month(NaiveDate::from_ymd_opt(2026, 2, 1).unwrap()),
            28
        );
        assert_eq!(
            days_in_month(NaiveDate::from_ymd_opt(2024, 2, 1).unwrap()),
            29
        );
    }

    #[test]
    fn day_enumeration_is_inclusive() {
        let since = NaiveDate::from_ymd_opt(2026, 8, 1).unwrap();
        let until = NaiveDate::from_ymd_opt(2026, 8, 3).unwrap();
        let days = enumerate_days(since, until);
        assert_eq!(days.len(), 3);
        assert_eq!(days[0], since);
        assert_eq!(days[2], until);
        assert_eq!(enumerate_days(until, since), Vec::<NaiveDate>::new());
    }
}
