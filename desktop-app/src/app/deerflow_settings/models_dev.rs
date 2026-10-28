//! Public model metadata. All entry points below perform blocking work and must
//! run on a background executor, never from a render or row-building callback.
//! Model endpoints and credentials are deliberately not accepted by this API.

use std::fs::{self, File};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard, OnceLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use serde_json::Value;

const ENDPOINT: &str = "https://models.dev/api.json";
const MAX_BYTES: usize = 20 * 1024 * 1024;
const CACHE_TTL: Duration = Duration::from_secs(24 * 60 * 60);
const FAILURE_COOLDOWN: Duration = Duration::from_secs(10 * 60);
const REQUEST_TIMEOUT: Duration = Duration::from_secs(5);

#[derive(Clone, Debug, Default)]
pub(super) struct Catalog {
    pub(super) entries: Vec<CatalogEntry>,
}

#[derive(Clone, Debug)]
pub(super) struct CatalogEntry {
    pub(super) provider_id: String,
    pub(super) provider_name: String,
    pub(super) model_id: String,
    pub(super) name: String,
    pub(super) api: Option<String>,
    pub(super) context_window: Option<u64>,
    pub(super) supports_vision: Option<bool>,
    pub(super) reasoning_efforts: Vec<String>,
}

#[derive(Clone, Debug)]
pub(super) struct CatalogSnapshot {
    pub(super) catalog: Arc<Catalog>,
    /// Unix seconds at the last successful download, not the last attempt.
    pub(super) fetched_at: u64,
    pub(super) stale: bool,
    pub(super) warning: Option<String>,
}

/// Loads the local public catalog without accessing the network. Background only.
pub(super) fn load_cached() -> Option<CatalogSnapshot> {
    service().load_cached()
}

/// Refreshes in the background, retaining the last usable catalog on any error.
/// Explicit refresh skips the 24-hour TTL, but respects the failure cooldown.
pub(super) fn refresh(force: bool) -> Result<CatalogSnapshot, String> {
    service().refresh(force)
}

fn service() -> &'static CatalogService {
    static SERVICE: OnceLock<CatalogService> = OnceLock::new();
    SERVICE.get_or_init(|| {
        let path = dirs::cache_dir().map(|directory| {
            directory
                .join(waku_protocol::identity::DATA_DIRECTORY_NAME)
                .join("models-dev")
                .join("catalog-v1.json")
        });
        CatalogService::new(path, ENDPOINT.into(), REQUEST_TIMEOUT)
    })
}

#[derive(Default)]
struct State {
    initialized: bool,
    in_flight: bool,
    snapshot: Option<CatalogSnapshot>,
    last_failure: Option<(Instant, String)>,
}

struct CatalogService {
    path: Option<PathBuf>,
    // Only the fixed public ENDPOINT is used outside this module's tests.
    endpoint: String,
    timeout: Duration,
    state: Mutex<State>,
}

impl CatalogService {
    fn new(path: Option<PathBuf>, endpoint: String, timeout: Duration) -> Self {
        Self {
            path,
            endpoint,
            timeout,
            state: Mutex::new(State::default()),
        }
    }

    fn lock(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|error| error.into_inner())
    }

    fn initialize(&self) -> bool {
        let mut state = self.lock();
        if state.initialized {
            return true;
        }
        if state.in_flight {
            // Do not consume executor workers waiting on another worker's I/O.
            return false;
        }
        state.in_flight = true;
        drop(state);
        // No filesystem or network operations occur while the mutex is held.
        let snapshot = self.path.as_deref().and_then(read_cache);
        let mut state = self.lock();
        state.snapshot = snapshot;
        state.initialized = true;
        state.in_flight = false;
        true
    }

    fn load_cached(&self) -> Option<CatalogSnapshot> {
        if !self.initialize() {
            return None;
        }
        let state = self.lock();
        let mut snapshot = state.snapshot.clone()?;
        snapshot.stale |= expired(snapshot.fetched_at);
        Some(snapshot)
    }

    fn refresh(&self, force: bool) -> Result<CatalogSnapshot, String> {
        if !self.initialize() {
            return Err("模型目录正在读取缓存，请稍后重试".into());
        }
        let mut state = self.lock();
        if state.in_flight {
            return match state.snapshot.clone() {
                Some(mut snapshot) => {
                    snapshot.stale |= expired(snapshot.fetched_at);
                    snapshot.warning = Some("模型目录正在刷新，暂时使用上次成功下载的目录".into());
                    Ok(snapshot)
                }
                None => Err("模型目录正在刷新，请稍后重试".into()),
            };
        }
        if let Some((attempt, error)) = &state.last_failure
            && attempt.elapsed() < FAILURE_COOLDOWN
        {
            return stale_or_error(&state.snapshot, error.clone());
        }
        if !force
            && let Some(snapshot) = &state.snapshot
            && !snapshot.stale
            && !expired(snapshot.fetched_at)
        {
            return Ok(snapshot.clone());
        }
        state.in_flight = true;
        drop(state);

        let fetched = download(&self.endpoint, self.timeout).map(|(catalog, source)| {
            let fetched_at = unix_time();
            let warning = match self.path.as_deref() {
                Some(path) => write_cache(path, fetched_at, source).err(),
                None => Some("无法定位本地缓存目录；本次目录仅保存在内存中".into()),
            };
            CatalogSnapshot {
                catalog: Arc::new(catalog),
                fetched_at,
                stale: false,
                warning,
            }
        });

        let mut state = self.lock();
        let result = match fetched {
            Ok(snapshot) => {
                state.snapshot = Some(snapshot.clone());
                state.last_failure = None;
                Ok(snapshot)
            }
            Err(error) => {
                state.last_failure = Some((Instant::now(), error.clone()));
                let result = stale_or_error(&state.snapshot, error);
                if let Ok(snapshot) = &result {
                    state.snapshot = Some(snapshot.clone());
                }
                result
            }
        };
        state.in_flight = false;
        result
    }
}

fn stale_or_error(
    snapshot: &Option<CatalogSnapshot>,
    error: String,
) -> Result<CatalogSnapshot, String> {
    let warning = format!("{error}；10 分钟内暂停重试");
    match snapshot {
        Some(snapshot) => {
            let mut snapshot = snapshot.clone();
            snapshot.stale = true;
            snapshot.warning = Some(format!("{warning}，正在使用上次成功下载的目录"));
            Ok(snapshot)
        }
        None => Err(warning),
    }
}

fn unix_time() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

fn expired(fetched_at: u64) -> bool {
    let now = unix_time();
    fetched_at == 0 || fetched_at > now || now - fetched_at >= CACHE_TTL.as_secs()
}

fn download(endpoint: &str, timeout: Duration) -> Result<(Catalog, Value), String> {
    let client = reqwest::blocking::Client::builder()
        .connect_timeout(Duration::from_secs(2).min(timeout))
        .timeout(timeout)
        .redirect(reqwest::redirect::Policy::none())
        .https_only(endpoint == ENDPOINT)
        .user_agent(concat!("DeerFlow-Desktop/", env!("CARGO_PKG_VERSION")))
        .build()
        .map_err(|_| "无法初始化模型目录下载".to_string())?;
    let response = client.get(endpoint).send().map_err(|error| {
        if error.is_timeout() {
            "模型目录下载超时".to_string()
        } else {
            "无法连接 models.dev".to_string()
        }
    })?;
    if !response.status().is_success() {
        return Err(format!(
            "模型目录下载失败（HTTP {}）",
            response.status().as_u16()
        ));
    }
    if response
        .content_length()
        .is_some_and(|length| length > MAX_BYTES as u64)
    {
        return Err("模型目录超过 20 MiB 大小限制".into());
    }
    let bytes =
        read_bounded(response).map_err(|_| "模型目录读取失败或超过 20 MiB 大小限制".to_string())?;
    let source: Value =
        serde_json::from_slice(&bytes).map_err(|_| "模型目录不是有效的 JSON".to_string())?;
    let catalog = parse_catalog(&source)?;
    Ok((catalog, source))
}

fn read_bounded(reader: impl Read) -> std::io::Result<Vec<u8>> {
    let mut bytes = Vec::new();
    reader.take(MAX_BYTES as u64 + 1).read_to_end(&mut bytes)?;
    if bytes.len() > MAX_BYTES {
        return Err(std::io::Error::other("model catalog too large"));
    }
    Ok(bytes)
}

fn parse_catalog(source: &Value) -> Result<Catalog, String> {
    let providers = source
        .as_object()
        .ok_or_else(|| "模型目录结构不兼容".to_string())?;
    let mut catalog = Catalog::default();
    for (provider_id, provider) in providers {
        if !valid_identifier(provider_id) {
            continue;
        }
        let Some(models) = provider.get("models").and_then(Value::as_object) else {
            continue;
        };
        let provider_name = display_name(provider.get("name"), provider_id);
        let provider_api = provider.get("api").and_then(parse_api);
        for (model_id, model) in models {
            if !valid_identifier(model_id) || !model.is_object() {
                continue;
            }
            // An explicit model-level endpoint wins. A malformed override must
            // never silently associate this model with the provider's endpoint.
            let api = match model.get("provider") {
                None => provider_api.clone(),
                Some(override_provider) => {
                    let Some(override_provider) = override_provider.as_object() else {
                        continue;
                    };
                    match override_provider.get("api") {
                        None => provider_api.clone(),
                        Some(value) => {
                            let Some(api) = parse_api(value) else {
                                continue;
                            };
                            Some(api)
                        }
                    }
                }
            };
            let context_window = model
                .pointer("/limit/context")
                .and_then(Value::as_u64)
                .filter(|size| *size > 0 && *size <= 1 << 30);
            let supports_vision = model
                .pointer("/modalities/input")
                .and_then(Value::as_array)
                .filter(|inputs| !inputs.is_empty() && inputs.iter().all(Value::is_string))
                .map(|inputs| {
                    inputs.iter().filter_map(Value::as_str).any(|input| {
                        input == "image"
                            || input
                                .strip_prefix("image/")
                                .is_some_and(|kind| !kind.is_empty())
                    })
                });
            // A reasoning boolean says nothing about the accepted effort values.
            let mut reasoning_efforts = Vec::new();
            if let Some(options) = model.get("reasoning_options").and_then(Value::as_array) {
                for option in options {
                    if option.get("type").and_then(Value::as_str) != Some("effort") {
                        continue;
                    }
                    if let Some(values) = option.get("values").and_then(Value::as_array) {
                        for value in values.iter().filter_map(Value::as_str) {
                            if valid_identifier(value)
                                && value.len() <= 64
                                && !reasoning_efforts.iter().any(|old| old == value)
                            {
                                reasoning_efforts.push(value.to_owned());
                            }
                        }
                    }
                }
            }
            catalog.entries.push(CatalogEntry {
                provider_id: provider_id.clone(),
                provider_name: provider_name.clone(),
                model_id: model_id.clone(),
                name: display_name(model.get("name"), model_id),
                api,
                context_window,
                supports_vision,
                reasoning_efforts,
            });
        }
    }
    if catalog.entries.is_empty() {
        return Err("模型目录没有可用的模型条目".into());
    }
    Ok(catalog)
}

fn valid_identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && !value.chars().any(char::is_whitespace)
        && !value.chars().any(char::is_control)
}

fn parse_api(value: &Value) -> Option<String> {
    let api = value.as_str()?;
    let url = url::Url::parse(api).ok()?;
    (matches!(url.scheme(), "https" | "http")
        && url.host_str().is_some()
        && url.username().is_empty()
        && url.password().is_none()
        && api.len() <= 4096)
        .then(|| api.to_owned())
}

fn display_name(value: Option<&Value>, fallback: &str) -> String {
    value
        .and_then(Value::as_str)
        .filter(|name| {
            !name.trim().is_empty() && name.len() <= 4096 && !name.chars().any(char::is_control)
        })
        .unwrap_or(fallback)
        .to_owned()
}

#[derive(Serialize, Deserialize)]
struct CacheFile {
    version: u32,
    fetched_at: u64,
    source: Value,
}

fn read_cache(path: &Path) -> Option<CatalogSnapshot> {
    let file = File::open(path).ok()?;
    if file.metadata().ok()?.len() > MAX_BYTES as u64 {
        return None;
    }
    let bytes = read_bounded(file).ok()?;
    let cache: CacheFile = serde_json::from_slice(&bytes).ok()?;
    if cache.version != 1 || cache.fetched_at == 0 {
        return None;
    }
    Some(CatalogSnapshot {
        catalog: Arc::new(parse_catalog(&cache.source).ok()?),
        fetched_at: cache.fetched_at,
        stale: expired(cache.fetched_at),
        warning: None,
    })
}

fn write_cache(path: &Path, fetched_at: u64, source: Value) -> Result<(), String> {
    let persist = || -> Result<(), Box<dyn std::error::Error>> {
        let bytes = serde_json::to_vec(&CacheFile {
            version: 1,
            fetched_at,
            source,
        })?;
        if bytes.len() > MAX_BYTES {
            return Err("cache too large".into());
        }
        let parent = path.parent().ok_or("missing cache parent")?;
        fs::create_dir_all(parent)?;
        let mut temporary = tempfile::Builder::new()
            .prefix(".catalog-")
            .tempfile_in(parent)?;
        temporary.write_all(&bytes)?;
        temporary.as_file().sync_all()?;
        temporary.persist(path)?;
        Ok(())
    };
    persist().map_err(|_| "模型目录已下载，但无法写入本地缓存；本次目录仅保存在内存中".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::VecDeque;
    use std::net::TcpListener;
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
    use std::thread;

    fn fixture() -> Value {
        serde_json::json!({
            "vendor": {
                "name": "Vendor",
                "api": "https://api.example.test/v1",
                "models": {
                    "vision-model": {
                        "name": "Vision Model",
                        "limit": { "context": 128000 },
                        "modalities": { "input": ["text", "image/png"] },
                        "reasoning": true,
                        "reasoning_options": [
                            { "type": "toggle" },
                            { "type": "effort", "values": ["minimal", "low", "high", "high", 4] }
                        ],
                        "future_field": { "ignored": true }
                    },
                    "text-model": {
                        "reasoning": true,
                        "modalities": { "input": ["text"] }
                    }
                }
            }
        })
    }

    #[test]
    fn parses_capabilities_without_inventing_reasoning_controls() {
        let catalog = parse_catalog(&fixture()).unwrap();
        let vision = catalog
            .entries
            .iter()
            .find(|entry| entry.model_id == "vision-model")
            .unwrap();
        assert_eq!(vision.provider_id, "vendor");
        assert_eq!(vision.provider_name, "Vendor");
        assert_eq!(vision.name, "Vision Model");
        assert_eq!(vision.api.as_deref(), Some("https://api.example.test/v1"));
        assert_eq!(vision.context_window, Some(128000));
        assert_eq!(vision.supports_vision, Some(true));
        // The mapping layer, not the catalog reader, intersects runtime values.
        assert_eq!(vision.reasoning_efforts, ["minimal", "low", "high"]);
        let text = catalog
            .entries
            .iter()
            .find(|entry| entry.model_id == "text-model")
            .unwrap();
        assert_eq!(text.supports_vision, Some(false));
        assert_eq!(text.context_window, None);
        assert!(text.reasoning_efforts.is_empty());
    }

    #[test]
    fn model_endpoint_override_wins_and_invalid_overrides_do_not_fall_back() {
        let source = serde_json::json!({
            "vendor": {
                "api": "https://api.vendor.test/v1",
                "models": {
                    "inherited": {},
                    "unknown-provider-field": {"provider":{"future_field":true}},
                    "overridden": {"provider":{"api":"https://proxy.vendor.test/v1"}},
                    "bad-url": {"provider":{"api":"not-a-url"}},
                    "credentials": {"provider":{"api":"https://secret@api.vendor.test/v1"}},
                    "bad-type": {"provider":{"api":false}},
                    "explicit-null": {"provider":{"api":null}},
                    "malformed-provider": {"provider":[]}
                }
            },
            "no-root-endpoint": {
                "models": {"override-only":{"provider":{"api":"https://standalone.vendor.test/v1"}}}
            }
        });
        let catalog = parse_catalog(&source).unwrap();
        assert_eq!(catalog.entries.len(), 4);
        let endpoint = |id: &str| {
            catalog
                .entries
                .iter()
                .find(|entry| entry.model_id == id)
                .unwrap()
                .api
                .as_deref()
        };
        assert_eq!(endpoint("inherited"), Some("https://api.vendor.test/v1"));
        assert_eq!(
            endpoint("unknown-provider-field"),
            Some("https://api.vendor.test/v1")
        );
        assert_eq!(endpoint("overridden"), Some("https://proxy.vendor.test/v1"));
        assert_eq!(
            endpoint("override-only"),
            Some("https://standalone.vendor.test/v1")
        );
    }

    #[test]
    fn isolates_bad_entries_and_fields_and_recognizes_legacy_images() {
        let source = serde_json::json!({
            "malformed-provider": [],
            "vendor": {
                "name": false,
                "api": "https://secret@example.test/v1",
                "models": {
                    "bad-entry": [],
                    "invalid id": {},
                    "missing": {},
                    "invalid": {
                        "name": 7,
                        "limit": {"context": -4},
                        "modalities": {"input": ["image", 7]},
                        "reasoning_options": [{"type":"effort", "values": "high"}]
                    },
                    "fractional": {"limit":{"context":1.5}},
                    "huge": {"limit":{"context":1073741825}},
                    "legacy": {"modalities":{"input":["image"]}},
                    "mime": {"modalities":{"input":["image/*"]}}
                }
            }
        });
        let catalog = parse_catalog(&source).unwrap();
        assert_eq!(catalog.entries.len(), 6);
        for entry in &catalog.entries {
            assert_eq!(entry.provider_name, "vendor");
            assert!(entry.api.is_none());
            assert!(entry.context_window.is_none());
            assert_eq!(
                entry.supports_vision,
                if matches!(entry.model_id.as_str(), "legacy" | "mime") {
                    Some(true)
                } else {
                    None
                }
            );
        }
        assert!(parse_catalog(&serde_json::json!([])).is_err());
        assert!(parse_catalog(&serde_json::json!({})).is_err());
    }

    #[test]
    fn cache_round_trip_and_invalid_cache_do_not_create_capabilities() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("catalog.json");
        assert!(read_cache(&path).is_none());
        write_cache(&path, unix_time(), fixture()).unwrap();
        assert!(!read_cache(&path).unwrap().stale);
        let old = unix_time() - CACHE_TTL.as_secs() - 1;
        write_cache(&path, old, fixture()).unwrap();
        let cached = read_cache(&path).unwrap();
        assert!(cached.stale);
        assert_eq!(cached.fetched_at, old);
        fs::write(&path, b"broken").unwrap();
        assert!(read_cache(&path).is_none());
        let file = File::create(&path).unwrap();
        file.set_len(MAX_BYTES as u64 + 1).unwrap();
        assert!(read_cache(&path).is_none());
        assert!(expired(unix_time() + 1000));
    }

    struct Reply {
        status: u16,
        body: Vec<u8>,
        delay: Duration,
        declared_length: Option<usize>,
    }

    impl Reply {
        fn json(value: Value) -> Self {
            Self {
                status: 200,
                body: serde_json::to_vec(&value).unwrap(),
                delay: Duration::ZERO,
                declared_length: None,
            }
        }
    }

    struct Server {
        endpoint: String,
        requests: Arc<AtomicUsize>,
        stop: Arc<AtomicBool>,
        worker: Option<thread::JoinHandle<()>>,
    }

    impl Server {
        fn start(replies: Vec<Reply>) -> Self {
            let listener = TcpListener::bind("127.0.0.1:0").unwrap();
            listener.set_nonblocking(true).unwrap();
            let endpoint = format!("http://{}/api.json", listener.local_addr().unwrap());
            let requests = Arc::new(AtomicUsize::new(0));
            let stop = Arc::new(AtomicBool::new(false));
            let worker_requests = requests.clone();
            let worker_stop = stop.clone();
            let worker = thread::spawn(move || {
                let mut replies: VecDeque<_> = replies.into();
                while !worker_stop.load(Ordering::SeqCst) {
                    let (mut socket, _) = match listener.accept() {
                        Ok(connection) => connection,
                        Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                            thread::sleep(Duration::from_millis(5));
                            continue;
                        }
                        Err(error) => panic!("fixture accept failed: {error}"),
                    };
                    // Windows accepted sockets inherit the listener's
                    // nonblocking flag; requests may not have arrived yet.
                    socket.set_nonblocking(false).unwrap();
                    socket
                        .set_read_timeout(Some(Duration::from_secs(1)))
                        .unwrap();
                    let mut request = [0; 4096];
                    let length = socket.read(&mut request).unwrap();
                    let request = String::from_utf8_lossy(&request[..length]).to_lowercase();
                    assert!(request.starts_with("get /api.json http/1.1"));
                    assert!(!request.contains("authorization:"));
                    worker_requests.fetch_add(1, Ordering::SeqCst);
                    let reply = replies
                        .pop_front()
                        .expect("unexpected extra catalog request");
                    thread::sleep(reply.delay);
                    let header = format!(
                        "HTTP/1.1 {} Fixture\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        reply.status,
                        reply.declared_length.unwrap_or(reply.body.len())
                    );
                    let _ = socket.write_all(header.as_bytes());
                    let _ = socket.write_all(&reply.body);
                }
            });
            Self {
                endpoint,
                requests,
                stop,
                worker: Some(worker),
            }
        }

        fn service(&self, path: PathBuf) -> CatalogService {
            CatalogService::new(Some(path), self.endpoint.clone(), Duration::from_secs(1))
        }
    }

    impl Drop for Server {
        fn drop(&mut self) {
            self.stop.store(true, Ordering::SeqCst);
            let result = self.worker.take().unwrap().join();
            if !thread::panicking() {
                result.expect("catalog fixture worker failed");
            }
        }
    }

    #[test]
    fn fresh_cache_avoids_network_and_failed_refresh_preserves_last_good() {
        let server = Server::start(vec![
            Reply::json(fixture()),
            Reply {
                status: 503,
                ..Reply::json(fixture())
            },
            Reply::json(fixture()),
        ]);
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("catalog.json");
        let service = server.service(path.clone());
        assert!(service.load_cached().is_none());
        let first = service.refresh(false).unwrap();
        let disk = fs::read(&path).unwrap();
        assert_eq!(server.requests.load(Ordering::SeqCst), 1);
        assert!(Arc::ptr_eq(
            &first.catalog,
            &service.refresh(false).unwrap().catalog
        ));
        assert_eq!(server.requests.load(Ordering::SeqCst), 1);
        let failed = service.refresh(true).unwrap();
        assert!(failed.stale);
        assert!(failed.warning.unwrap().contains("503"));
        assert_eq!(failed.fetched_at, first.fetched_at);
        assert_eq!(disk, fs::read(&path).unwrap());
        // Forced refresh cannot hammer an unavailable endpoint.
        assert!(service.refresh(true).unwrap().stale);
        assert_eq!(server.requests.load(Ordering::SeqCst), 2);
        assert!(service.load_cached().unwrap().stale);
        // Once the cooldown expires, even a previously fresh snapshot retries.
        service.lock().last_failure.as_mut().unwrap().0 =
            Instant::now() - FAILURE_COOLDOWN - Duration::from_secs(1);
        assert!(!service.refresh(false).unwrap().stale);
        assert_eq!(server.requests.load(Ordering::SeqCst), 3);
        assert!(server.service(path).load_cached().is_some());
    }

    #[test]
    fn invalid_and_oversized_responses_never_replace_valid_disk_cache() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("catalog.json");
        write_cache(&path, unix_time(), fixture()).unwrap();
        let disk = fs::read(&path).unwrap();
        let replies = vec![
            Reply {
                body: b"bad json".to_vec(),
                ..Reply::json(fixture())
            },
            Reply::json(serde_json::json!({})),
            Reply {
                declared_length: Some(MAX_BYTES + 1),
                ..Reply::json(fixture())
            },
        ];
        for reply in replies {
            let server = Server::start(vec![reply]);
            let result = server.service(path.clone()).refresh(true).unwrap();
            assert!(result.stale);
            assert!(result.warning.is_some());
            assert_eq!(disk, fs::read(&path).unwrap());
        }
        let reader = std::io::repeat(b'x').take(MAX_BYTES as u64 + 1);
        assert!(read_bounded(reader).is_err());
    }

    #[test]
    fn timeout_is_bounded_and_cools_down_without_a_cache() {
        let server = Server::start(vec![Reply {
            delay: Duration::from_millis(250),
            ..Reply::json(fixture())
        }]);
        let directory = tempfile::tempdir().unwrap();
        let service = CatalogService::new(
            Some(directory.path().join("catalog.json")),
            server.endpoint.clone(),
            Duration::from_millis(80),
        );
        let started = Instant::now();
        assert!(service.refresh(false).unwrap_err().contains("超时"));
        assert!(started.elapsed() < Duration::from_secs(1));
        assert!(service.refresh(true).is_err());
        assert_eq!(server.requests.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn disk_failure_keeps_successful_data_in_memory() {
        let server = Server::start(vec![Reply::json(fixture())]);
        let directory = tempfile::tempdir().unwrap();
        let blocked = directory.path().join("parent-is-file");
        fs::write(&blocked, b"keep").unwrap();
        let service = server.service(blocked.join("catalog.json"));
        let result = service.refresh(false).unwrap();
        assert!(!result.stale);
        assert!(result.warning.unwrap().contains("缓存"));
        assert_eq!(result.catalog.entries.len(), 2);
        assert!(service.refresh(false).is_ok());
        assert_eq!(server.requests.load(Ordering::SeqCst), 1);
        assert_eq!(fs::read(blocked).unwrap(), b"keep");
    }

    #[test]
    fn concurrent_force_refreshes_do_not_wait_or_duplicate_the_download() {
        let server = Server::start(vec![Reply {
            delay: Duration::from_millis(150),
            ..Reply::json(fixture())
        }]);
        let directory = tempfile::tempdir().unwrap();
        let service = Arc::new(server.service(directory.path().join("catalog.json")));
        let barrier = Arc::new(std::sync::Barrier::new(5));
        let workers: Vec<_> = (0..4)
            .map(|_| {
                let service = service.clone();
                let barrier = barrier.clone();
                thread::spawn(move || {
                    barrier.wait();
                    service.refresh(true)
                })
            })
            .collect();
        barrier.wait();
        let results: Vec<_> = workers
            .into_iter()
            .map(|worker| worker.join().unwrap())
            .collect();
        assert_eq!(server.requests.load(Ordering::SeqCst), 1);
        assert_eq!(results.iter().filter(|result| result.is_ok()).count(), 1);
        assert_eq!(results.iter().filter(|result| result.is_err()).count(), 3);
        assert_eq!(service.load_cached().unwrap().catalog.entries.len(), 2);
    }

    #[test]
    fn in_progress_initialization_and_refresh_return_without_waiting() {
        let directory = tempfile::tempdir().unwrap();
        let service = CatalogService::new(
            Some(directory.path().join("catalog.json")),
            "http://127.0.0.1:1/api.json".into(),
            Duration::from_secs(1),
        );
        // Simulate the intervals while another worker is outside the mutex.
        service.lock().in_flight = true;
        assert!(service.load_cached().is_none());
        assert!(service.refresh(true).unwrap_err().contains("读取缓存"));
        {
            let mut state = service.lock();
            state.initialized = true;
            state.snapshot = Some(CatalogSnapshot {
                catalog: Arc::new(parse_catalog(&fixture()).unwrap()),
                fetched_at: unix_time(),
                stale: false,
                warning: None,
            });
        }
        let snapshot = service.refresh(true).unwrap();
        assert!(snapshot.warning.unwrap().contains("正在刷新"));
        assert_eq!(snapshot.catalog.entries.len(), 2);
    }
}
