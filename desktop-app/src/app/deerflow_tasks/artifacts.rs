//! Artifact bytes travel through the attached ACP connection. Only a user's
//! local save-dialog path reaches the disk worker; host names are suggestions.
use super::*;
use base64::Engine as _;
use sha2::{Digest, Sha256};
use std::io::Read;
use std::io::Write;
use std::sync::atomic::{AtomicBool, Ordering};
use waku_protocol::deerflow_tasks::{Artifact, ArtifactChunk};

const CHUNK_BYTES: u32 = 1024 * 1024;
// The host enforces its configured artifact limit; the client streams bounded
// chunks to disk and verifies the advertised size and digest independently.

#[derive(Clone)]
struct Binding {
    connection: String,
    session: String,
    root: String,
    node: String,
    artifact: Artifact,
}

pub(super) struct Transfer {
    id: Uuid,
    binding: Binding,
    input: smol::channel::Sender<Result<ArtifactChunk, String>>,
    cancelled: Arc<AtomicBool>,
}
impl Drop for Transfer {
    fn drop(&mut self) {
        self.cancelled.store(true, Ordering::Release);
    }
}
impl Transfer {
    pub(super) fn cancel(&self) {
        self.cancelled.store(true, Ordering::Release);
        self.input.close();
    }
    pub(super) fn receive(&self, command: &Command, chunk: ArtifactChunk) -> anyhow::Result<()> {
        anyhow::ensure!(
            command.connection_id == self.binding.connection
                && command.session_id == self.binding.session
                && chunk.root_id == self.binding.root
                && chunk.node_id == self.binding.node
                && chunk.artifact.id == self.binding.artifact.id,
            "产物响应与当前保存请求不一致"
        );
        self.input
            .try_send(Ok(chunk))
            .map_err(|_| anyhow::anyhow!("产物保存已结束或取消"))
    }
    pub(super) fn fail(&self, message: String) {
        let _ = self.input.try_send(Err(message));
    }
}

enum Progress {
    Read(Action),
    Written(u64),
    Finished(Result<PathBuf, String>),
}

fn suggested_name(name: &str) -> String {
    let cleaned: String = name
        .chars()
        .filter(|c| {
            !c.is_control() && !matches!(c, '/' | '\\' | ':' | '*' | '?' | '"' | '<' | '>' | '|')
        })
        .take(160)
        .collect();
    let cleaned = cleaned.trim().trim_matches('.');
    if cleaned.is_empty() {
        "artifact".into()
    } else {
        cleaned.into()
    }
}

struct ChunkVerifier {
    binding: Binding,
    offset: u64,
    hash: Option<String>,
    digest: Sha256,
}
impl ChunkVerifier {
    fn action(&self) -> Action {
        Action::ArtifactRead {
            root_id: self.binding.root.clone(),
            node_id: self.binding.node.clone(),
            artifact_id: self.binding.artifact.id.clone(),
            offset: self.offset,
            limit: CHUNK_BYTES,
            expected_sha256: self.hash.clone(),
        }
    }
    fn accept(&mut self, chunk: ArtifactChunk) -> anyhow::Result<(Vec<u8>, bool)> {
        let expected = &self.binding.artifact;
        anyhow::ensure!(
            chunk.root_id == self.binding.root
                && chunk.node_id == self.binding.node
                && chunk.artifact.id == expected.id
                && chunk.artifact.size == expected.size
                && chunk.artifact.name == expected.name
                && chunk.artifact.mime_type == expected.mime_type
                && chunk.encoding == "base64"
                && digest(&chunk.sha256)
                && self.hash.as_ref().is_none_or(|hash| hash == &chunk.sha256)
                && chunk.offset == self.offset
                && chunk.data.len() <= (CHUNK_BYTES as usize).div_ceil(3) * 4,
            "产物内容身份、摘要或分块范围已变化"
        );
        let bytes = base64::engine::general_purpose::STANDARD.decode(&chunk.data)?;
        anyhow::ensure!(
            bytes.len() <= CHUNK_BYTES as usize
                && chunk.next_offset == self.offset + bytes.len() as u64
                && chunk.next_offset <= expected.size
                && chunk.eof == (chunk.next_offset == expected.size)
                && (chunk.eof || !bytes.is_empty()),
            "产物分块大小或完成标记无效"
        );
        self.digest.update(&bytes);
        self.offset = chunk.next_offset;
        self.hash = Some(chunk.sha256);
        if chunk.eof {
            anyhow::ensure!(
                format!("{:x}", self.digest.clone().finalize()) == *self.hash.as_ref().unwrap(),
                "产物完整 SHA256 校验失败"
            );
        }
        Ok((bytes, chunk.eof))
    }
}

async fn save_file(
    binding: Binding,
    destination: PathBuf,
    input: smol::channel::Receiver<Result<ArtifactChunk, String>>,
    progress: &smol::channel::Sender<Progress>,
    cancelled: Arc<AtomicBool>,
) -> anyhow::Result<PathBuf> {
    anyhow::ensure!(
        destination.is_absolute() && destination.file_name().is_some(),
        "保存路径或产物大小无效"
    );
    let directory = destination
        .parent()
        .ok_or_else(|| anyhow::anyhow!("请选择完整保存路径"))?;
    let mut temporary = tempfile::Builder::new()
        .prefix(".waku-artifact-")
        .suffix(".part")
        .tempfile_in(directory)?;
    let mut verifier = ChunkVerifier {
        binding,
        offset: 0,
        hash: None,
        digest: Sha256::new(),
    };
    loop {
        anyhow::ensure!(!cancelled.load(Ordering::Acquire), "产物保存已取消");
        progress.send(Progress::Read(verifier.action())).await?;
        let chunk = smol::future::or(
            async { Ok::<_, anyhow::Error>(input.recv().await?) },
            async {
                smol::Timer::after(Duration::from_secs(30)).await;
                Err(anyhow::anyhow!("等待产物分块超时，请重新保存"))
            },
        )
        .await?
        .map_err(anyhow::Error::msg)?;
        let (bytes, eof) = verifier.accept(chunk)?;
        anyhow::ensure!(!cancelled.load(Ordering::Acquire), "产物保存已取消");
        temporary.write_all(&bytes)?;
        progress.send(Progress::Written(verifier.offset)).await?;
        if eof {
            break;
        }
    }
    temporary.as_file_mut().sync_all()?;
    anyhow::ensure!(!cancelled.load(Ordering::Acquire), "产物保存已取消");
    temporary
        .persist(&destination)
        .map_err(|error| error.error)?;
    Ok(destination)
}

impl Waku {
    pub(super) fn preview_saved_deerflow_artifact(
        &mut self,
        session: Uuid,
        path: PathBuf,
        cx: &mut Context<Self>,
    ) {
        let Some(browser) = self.deerflow_tasks.get_mut(&session) else {
            return;
        };
        if browser.saved_artifact.as_ref() != Some(&path) {
            return;
        }
        browser.artifact_preview_generation += 1;
        let generation = browser.artifact_preview_generation;
        let connection = browser.connection.clone();
        browser.artifact_preview = Some("正在读取纯文本预览…".into());
        let task = cx
            .background_executor()
            .spawn(async move { passive_preview(&path) });
        cx.spawn(async move |this, cx| {
            let result = task.await;
            let _ = this.update(cx, |this, cx| {
                let Some(browser) = this.deerflow_tasks.get_mut(&session) else {
                    return;
                };
                if browser.artifact_preview_generation != generation
                    || browser.connection != connection
                {
                    return;
                }
                browser.artifact_preview = Some(match result {
                    Ok(text) => text.into(),
                    Err(error) => format!("无法预览：{error}").into(),
                });
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    pub(super) fn save_deerflow_task_artifact(
        &mut self,
        session: Uuid,
        root: String,
        node: String,
        artifact: Artifact,
        cx: &mut Context<Self>,
    ) {
        let Some(browser) = self.deerflow_tasks.get_mut(&session) else {
            return;
        };
        if browser.stale
            || browser.controls_busy()
            || browser.transfer.is_some()
            || browser.save_selecting
            || !browser.capabilities.artifact_read
        {
            return;
        }
        let binding = Binding {
            connection: browser.connection.clone(),
            session: browser.session.clone(),
            root,
            node,
            artifact,
        };
        browser.save_selecting = true;
        let name = suggested_name(&binding.artifact.name);
        let directory = dirs::download_dir()
            .or_else(dirs::home_dir)
            .unwrap_or_else(|| PathBuf::from("."));
        let receiver = cx.prompt_for_new_path(&directory, Some(&name));
        cx.spawn(async move |this, cx| {
            let path = receiver.await;
            let _ = this.update(cx, |this, cx| {
                let Some(browser) = this.deerflow_tasks.get_mut(&session) else {
                    return;
                };
                if browser.connection != binding.connection || browser.session != binding.session {
                    return;
                }
                browser.save_selecting = false;
                match path {
                    Ok(Ok(Some(path))) => {
                        this.start_deerflow_artifact_transfer(session, binding, path, cx)
                    }
                    Ok(Ok(None)) => cx.notify(),
                    _ => {
                        browser.artifact_notice = Some("无法打开保存文件对话框。".into());
                        cx.notify();
                    }
                }
            });
        })
        .detach();
        cx.notify();
    }

    fn start_deerflow_artifact_transfer(
        &mut self,
        session: Uuid,
        binding: Binding,
        destination: PathBuf,
        cx: &mut Context<Self>,
    ) {
        let Some(browser) = self.deerflow_tasks.get_mut(&session) else {
            return;
        };
        if browser.controls_busy()
            || browser.stale
            || browser.connection != binding.connection
            || browser.session != binding.session
            || browser.transfer.is_some()
            || !browser
                .nodes
                .get(&(binding.root.clone(), binding.node.clone()))
                .is_some_and(|node| {
                    browser.artifacts_for(&node.node).iter().any(|artifact| {
                        artifact.id == binding.artifact.id
                            && artifact.size == binding.artifact.size
                            && artifact.name == binding.artifact.name
                            && artifact.mime_type == binding.artifact.mime_type
                    })
                })
            || !self.runtimes.contains_key(&session)
        {
            browser.artifact_notice = Some("任务连接或产物已变化，请刷新后重新保存。".into());
            cx.notify();
            return;
        }
        let id = Uuid::new_v4();
        let cancelled = Arc::new(AtomicBool::new(false));
        let (input, receiver) = smol::channel::bounded(1);
        let (progress, updates) = smol::channel::bounded(2);
        browser.transfer = Some(Transfer {
            id,
            binding: binding.clone(),
            input,
            cancelled: cancelled.clone(),
        });
        browser.artifact_notice = Some("正在读取产物…".into());
        browser.saved_artifact = None;
        browser.artifact_preview = None;
        browser.artifact_preview_generation += 1;
        cx.background_executor()
            .spawn(async move {
                let result = save_file(binding, destination, receiver, &progress, cancelled)
                    .await
                    .map_err(|error| error.to_string());
                let _ = progress.send(Progress::Finished(result)).await;
            })
            .detach();
        cx.spawn(async move |this, cx| {
            while let Ok(progress) = updates.recv().await {
                let finished = matches!(progress, Progress::Finished(_));
                let result = this.update(cx, |this, cx| {
                    let Some(browser) = this.deerflow_tasks.get_mut(&session) else {
                        return false;
                    };
                    if browser.transfer.as_ref().is_none_or(|transfer| {
                        transfer.id != id || transfer.binding.connection != browser.connection
                    }) {
                        return false;
                    }
                    match progress {
                        Progress::Read(action) => this.request_deerflow_task(session, action, cx),
                        Progress::Written(bytes) => {
                            browser.artifact_notice =
                                Some(format!("已校验并写入 {} 字节…", bytes).into());
                            cx.notify();
                        }
                        Progress::Finished(result) => {
                            let cancelled = browser
                                .transfer
                                .as_ref()
                                .is_some_and(|transfer| transfer.cancelled.load(Ordering::Acquire));
                            browser.transfer = None;
                            match result {
                                Ok(path) => {
                                    browser.artifact_notice =
                                        Some(format!("产物已完整保存：{}", path.display()).into());
                                    browser.saved_artifact = Some(path);
                                }
                                Err(error) => {
                                    if cancelled {
                                        browser.artifact_notice = Some("产物保存已取消。".into());
                                    } else {
                                        browser.artifact_notice = Some(
                                            format!(
                                                "保存未完成：{}",
                                                error.chars().take(1000).collect::<String>()
                                            )
                                            .into(),
                                        );
                                    }
                                }
                            }
                            cx.notify();
                        }
                    }
                    true
                });
                if finished || !matches!(result, Ok(true)) {
                    break;
                }
            }
        })
        .detach();
        cx.notify();
    }
}

fn passive_preview(path: &std::path::Path) -> anyhow::Result<String> {
    // Passive text only: never open HTML/SVG in a privileged browser origin.
    let extension = path
        .extension()
        .and_then(|s| s.to_str())
        .unwrap_or("")
        .to_ascii_lowercase();
    if matches!(extension.as_str(), "pdf" | "docx" | "xlsx") {
        let executable = std::env::current_exe()?.with_file_name(if cfg!(windows) {
            "deerflow-control-go.exe"
        } else {
            "deerflow-control-go"
        });
        let mut command = std::process::Command::new(executable);
        command
            .arg("--preview-file")
            .arg(path)
            .stdin(std::process::Stdio::null());
        #[cfg(windows)]
        {
            use std::os::windows::process::CommandExt;
            command.creation_flags(0x0800_0000);
        }
        let output = command.output()?;
        anyhow::ensure!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
                .chars()
                .take(1000)
                .collect::<String>()
        );
        anyhow::ensure!(output.stdout.len() <= 64 * 1024, "预览响应超过限制");
        let value: serde_json::Value = serde_json::from_slice(&output.stdout)?;
        let text = value["text"]
            .as_str()
            .ok_or_else(|| anyhow::anyhow!("预览响应无效"))?;
        anyhow::ensure!(text.len() <= 8192, "预览内容超过限制");
        let mut text = text
            .chars()
            .filter(|c| !c.is_control() || matches!(c, '\n' | '\r' | '\t'))
            .collect::<String>();
        if value["truncated"].as_bool() == Some(true) {
            text.push_str("\n[仅预览前 8 KiB；完整内容请打开已保存文件]");
        }
        return Ok(text);
    }
    anyhow::ensure!(
        matches!(
            extension.as_str(),
            "txt" | "md" | "markdown" | "csv" | "tsv" | "json" | "yaml" | "yml" | "log"
        ),
        "此格式不支持纯文本预览"
    );
    let file = std::fs::File::open(path)?;
    anyhow::ensure!(file.metadata()?.is_file(), "预览目标必须是文件");
    let mut bytes = Vec::new();
    file.take(8193).read_to_end(&mut bytes)?;
    let truncated = bytes.len() > 8192;
    bytes.truncate(8192);
    let mut text = String::from_utf8_lossy(&bytes)
        .chars()
        .filter(|c| !c.is_control() || matches!(c, '\n' | '\r' | '\t'))
        .collect::<String>();
    if truncated {
        text.push_str("\n[仅预览前 8 KiB；完整内容请打开已保存文件]");
    }
    Ok(text)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn deerflow_artifact_preview_is_passive_and_bounded() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("report.md");
        std::fs::write(&path, "<script>not executed</script>\n".repeat(2000)).unwrap();
        let text = passive_preview(&path).unwrap();
        assert!(text.contains("仅预览前 8 KiB"));
        assert!(text.len() < 8500);
        let html = dir.path().join("active.html");
        std::fs::write(&html, "<script>bad()</script>").unwrap();
        assert!(passive_preview(&html).is_err());
    }
    #[test]
    fn deerflow_artifact_accepts_host_sizes_above_legacy_limit() {
        let total = 600 * 1024 * 1024;
        let mut verifier = ChunkVerifier {
            binding: binding(total),
            offset: 0,
            hash: None,
            digest: Sha256::new(),
        };
        assert_eq!(
            verifier
                .accept(chunk(b"hello", total, 0, &"a".repeat(64)))
                .unwrap(),
            (b"hello".to_vec(), false)
        );
    }
    fn binding(size: u64) -> Binding {
        Binding {
            connection: "connection".into(),
            session: "session".into(),
            root: "root".into(),
            node: "node".into(),
            artifact: Artifact {
                id: "artifact".into(),
                name: "report.txt".into(),
                mime_type: "text/plain".into(),
                size,
            },
        }
    }
    fn chunk(data: &[u8], total: u64, offset: u64, hash: &str) -> ArtifactChunk {
        ArtifactChunk {
            root_id: "root".into(),
            node_id: "node".into(),
            artifact: binding(total).artifact,
            sha256: hash.into(),
            encoding: "base64".into(),
            data: base64::engine::general_purpose::STANDARD.encode(data),
            offset,
            next_offset: offset + data.len() as u64,
            eof: offset + data.len() as u64 == total,
        }
    }
    #[test]
    fn deerflow_artifact_chunks_pin_identity_hash_offsets_and_complete_bytes() {
        let hash = format!("{:x}", Sha256::digest(b"hello"));
        let mut verifier = ChunkVerifier {
            binding: binding(5),
            offset: 0,
            hash: None,
            digest: Sha256::new(),
        };
        assert_eq!(
            verifier.accept(chunk(b"he", 5, 0, &hash)).unwrap(),
            (b"he".to_vec(), false)
        );
        assert!(matches!(
            verifier.action(),
            Action::ArtifactRead {
                offset: 2,
                expected_sha256: Some(_),
                ..
            }
        ));
        let mut wrong = chunk(b"llo", 5, 2, &hash);
        wrong.node_id = "foreign".into();
        assert!(verifier.accept(wrong).is_err());
        assert!(verifier.accept(chunk(b"llo", 5, 1, &hash)).is_err());
        assert!(
            verifier
                .accept(chunk(b"llo", 5, 2, &"a".repeat(64)))
                .is_err()
        );
        assert_eq!(
            verifier.accept(chunk(b"llo", 5, 2, &hash)).unwrap(),
            (b"llo".to_vec(), true)
        );
        let mut corrupt = ChunkVerifier {
            binding: binding(5),
            offset: 0,
            hash: None,
            digest: Sha256::new(),
        };
        assert!(corrupt.accept(chunk(b"wrong", 5, 0, &hash)).is_err());
        assert!(!suggested_name("../bad\\path:name.txt").contains(['/', '\\', ':']));
    }
    #[test]
    fn deerflow_artifact_save_is_atomic_and_failure_preserves_existing_file() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("saved.txt");
        std::fs::write(&path, b"old").unwrap();
        let hash = format!("{:x}", Sha256::digest(b"hello"));
        for success in [false, true] {
            let (input, receiver) = smol::channel::bounded(1);
            input
                .try_send(Ok(chunk(
                    if success { b"hello" } else { b"wrong" },
                    5,
                    0,
                    &hash,
                )))
                .unwrap();
            // Keep the receiver alive while the disk worker posts progress.
            let (updates, progress) = smol::channel::bounded(4);
            let result = smol::block_on(save_file(
                binding(5),
                path.clone(),
                receiver,
                &updates,
                Arc::new(AtomicBool::new(false)),
            ));
            assert_eq!(result.is_ok(), success);
            drop(progress);
            assert_eq!(
                std::fs::read(&path).unwrap(),
                if success {
                    b"hello".as_slice()
                } else {
                    b"old".as_slice()
                }
            );
            assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 1);
        }
    }
}
