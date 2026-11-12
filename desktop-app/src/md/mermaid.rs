//! Offline Mermaid rendering. Frames only enqueue work and read cached results;
//! parsing, layout, font discovery and SVG rasterization run on bounded workers.

use std::cell::RefCell;
use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::Arc;

use gpui::{App, EntityId, Global, RenderImage, SharedString, SvgRenderer};
use mermaid_rs_renderer::{LayoutConfig, Theme, compute_layout, measure_svg_dimensions};

const MAX_SOURCE_BYTES: usize = 32 * 1024;
const MAX_NODES: usize = 256;
const MAX_EDGES: usize = 512;
const MAX_ENTRIES: usize = 64;
const MAX_CACHE_BYTES: usize = 64 * 1024 * 1024;
const MAX_PENDING: usize = 16;
const WORKERS: usize = 2;
const MAX_PIXELS: f32 = 8_000_000.0;
const MAX_SIDE: f32 = 4096.0;

#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub(super) struct Key {
    source: SharedString,
    dark: bool,
    font_size: u32,
    scale: u32,
}

impl Key {
    pub fn new(source: SharedString, dark: bool, font_size: f32, scale: f32) -> Self {
        Self {
            source,
            dark,
            font_size: font_size.clamp(10.0, 32.0).to_bits(),
            scale: scale.clamp(1.0, 3.0).to_bits(),
        }
    }
}

pub(super) struct Rendered {
    pub width: f32,
    pub height: f32,
    pub image: Arc<RenderImage>,
}

#[derive(Clone)]
pub(super) enum ResultState {
    Pending,
    Ready(Arc<Rendered>),
    Failed(SharedString),
}

struct Entry {
    result: ResultState,
    views: HashSet<EntityId>,
    touched: u64,
    bytes: usize,
}

#[derive(Default)]
struct Cache {
    entries: HashMap<Key, Entry>,
    queue: VecDeque<Key>,
    waiting: HashSet<EntityId>,
    clock: u64,
    bytes: usize,
    active: usize,
    pending: usize,
    retired: Vec<Arc<RenderImage>>,
}

#[derive(Default)]
struct Store(RefCell<Cache>);
impl Global for Store {}

impl Cache {
    fn request(&mut self, key: &Key, view: EntityId) -> ResultState {
        self.clock += 1;
        if let Some(entry) = self.entries.get_mut(key) {
            entry.touched = self.clock;
            if matches!(entry.result, ResultState::Pending) {
                entry.views.insert(view);
            }
            return entry.result.clone();
        }
        if self.pending >= MAX_PENDING {
            self.waiting.insert(view);
            return ResultState::Pending;
        }
        self.entries.insert(
            key.clone(),
            Entry {
                result: ResultState::Pending,
                views: HashSet::from([view]),
                touched: self.clock,
                bytes: 0,
            },
        );
        self.queue.push_back(key.clone());
        self.pending += 1;
        self.trim();
        ResultState::Pending
    }

    fn complete(&mut self, key: &Key, result: ResultState) -> HashSet<EntityId> {
        let mut notify = std::mem::take(&mut self.waiting);
        if let Some(entry) = self.entries.get_mut(key)
            && matches!(entry.result, ResultState::Pending)
        {
            notify.extend(entry.views.drain());
            entry.bytes = match &result {
                ResultState::Ready(rendered) => {
                    rendered.image.as_bytes(0).map_or(0, |bytes| bytes.len())
                }
                _ => 0,
            };
            self.bytes += entry.bytes;
            entry.result = result;
            self.pending -= 1;
        }
        self.trim();
        notify
    }

    fn trim(&mut self) {
        while self.entries.len() > MAX_ENTRIES || self.bytes > MAX_CACHE_BYTES {
            let oldest = self
                .entries
                .iter()
                .filter(|(_, entry)| !matches!(entry.result, ResultState::Pending))
                .min_by_key(|(_, entry)| entry.touched)
                .map(|(key, _)| key.clone());
            let Some(key) = oldest else { break };
            let entry = self.entries.remove(&key).unwrap();
            self.bytes -= entry.bytes;
            if let ResultState::Ready(rendered) = entry.result {
                self.retired.push(rendered.image.clone());
            }
        }
    }
}

pub(super) fn request(key: &Key, view: EntityId, cx: &mut App) -> ResultState {
    // Do not retain arbitrarily large source strings in the cache.
    if key.source.len() > MAX_SOURCE_BYTES {
        return ResultState::Failed("Mermaid source exceeds 32 KiB".into());
    }
    if !cx.has_global::<Store>() {
        cx.set_global(Store::default());
    }
    let result = cx.global::<Store>().0.borrow_mut().request(key, view);
    retire_images(cx);
    pump(cx);
    result
}

fn retire_images(cx: &mut App) {
    let images = std::mem::take(&mut cx.global::<Store>().0.borrow_mut().retired);
    if !images.is_empty() {
        cx.defer(move |cx| {
            for image in images {
                cx.drop_image(image, None);
            }
        });
    }
}

fn pump(cx: &mut App) {
    loop {
        let key = {
            let mut cache = cx.global::<Store>().0.borrow_mut();
            if cache.active >= WORKERS {
                return;
            }
            let Some(key) = cache.queue.pop_front() else {
                return;
            };
            cache.active += 1;
            key
        };
        let renderer = cx.svg_renderer();
        let task = cx.background_executor().spawn(async move {
            let result =
                std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| render(&key, &renderer)));
            let result = match result {
                Ok(Ok(rendered)) => ResultState::Ready(Arc::new(rendered)),
                Ok(Err(error)) => ResultState::Failed(
                    error
                        .to_string()
                        .chars()
                        .take(300)
                        .collect::<String>()
                        .into(),
                ),
                Err(_) => ResultState::Failed("Unable to render this Mermaid diagram".into()),
            };
            (key, result)
        });
        cx.spawn(async move |cx| {
            let (key, result) = task.await;
            let _ = cx.update(|cx| {
                let mut cache = cx.global::<Store>().0.borrow_mut();
                cache.active -= 1;
                // Source, theme, font size and display scale identify results.
                // Old completions cannot replace a newer source/style.
                let views = cache.complete(&key, result);
                drop(cache);
                for view in views {
                    cx.notify(view);
                }
                retire_images(cx);
                pump(cx);
            });
        })
        .detach();
    }
}

fn diagram_svg(key: &Key) -> anyhow::Result<(String, f32, f32)> {
    anyhow::ensure!(
        !key.source.trim().is_empty() && key.source.len() <= MAX_SOURCE_BYTES,
        "Mermaid source must contain 1–32768 bytes"
    );
    let parsed = mermaid_rs_renderer::parse_mermaid_strict(&key.source)?;
    if parsed.graph.kind == mermaid_rs_renderer::DiagramKind::Flowchart {
        validate_flowchart_labels(&key.source)?;
    }
    anyhow::ensure!(
        parsed.graph.nodes.len() <= MAX_NODES
            && parsed.graph.edges.len() <= MAX_EDGES
            && parsed.graph.subgraphs.len() <= MAX_NODES,
        "Mermaid diagram exceeds the node/edge limit"
    );
    let mut theme = if key.dark {
        Theme::dark()
    } else {
        Theme::modern()
    };
    // Both layout measurement and SVG text use the same system font stack,
    // including CJK fonts for labels such as those in deployment flowcharts.
    theme.font_family =
        "Segoe UI, Microsoft YaHei, PingFang SC, Noto Sans CJK SC, sans-serif".into();
    theme.font_size = f32::from_bits(key.font_size);
    theme.background = if key.dark { "#18181b" } else { "#ffffff" }.into();
    let config = LayoutConfig::default();
    let layout = compute_layout(&parsed.graph, &theme, &config);
    anyhow::ensure!(
        !matches!(
            layout.diagram,
            mermaid_rs_renderer::layout::DiagramData::Error(_)
        ),
        "Unsupported or invalid Mermaid diagram"
    );
    let dimensions = measure_svg_dimensions(&layout, &config, None);
    let (width, height) = (dimensions.width, dimensions.height);
    anyhow::ensure!(
        width.is_finite() && height.is_finite() && width > 0.0 && height > 0.0,
        "Invalid Mermaid diagram dimensions"
    );
    let svg = mermaid_rs_renderer::render_svg(&layout, &theme, &config);
    Ok((svg, width, height))
}

// The native parser tolerates an unfinished node label (e.g. `A[unfinished`)
// as a bare node. Reject unclosed shape delimiters so a truncated response
// cannot silently turn into a different diagram. Quoted/edge labels, comments
// and configuration text are literal, and asymmetric `A>label]` is valid.
fn validate_flowchart_labels(source: &str) -> anyhow::Result<()> {
    let mut started = false;
    let mut quote = false;
    let mut edge_label = false;
    let mut delimiters = Vec::new();
    for (line_number, line) in source.lines().enumerate() {
        let line = line.trim();
        if !started {
            let lower = line.to_ascii_lowercase();
            started = lower.starts_with("flowchart ") || lower.starts_with("graph ");
            if !started {
                continue;
            }
        }
        if !quote
            && (line.starts_with("%%")
                || ["style ", "classDef ", "linkStyle ", "click "]
                    .iter()
                    .any(|prefix| line.starts_with(prefix)))
        {
            continue;
        }
        let mut chars = line.chars().peekable();
        while let Some(ch) = chars.next() {
            if ch == '"' {
                quote = !quote;
                continue;
            }
            if quote {
                continue;
            }
            if ch == '%' && chars.peek() == Some(&'%') {
                break;
            }
            if ch == '|' && delimiters.is_empty() {
                edge_label = !edge_label;
                continue;
            }
            if edge_label {
                continue;
            }
            match ch {
                '[' | '(' | '{' => delimiters.push((ch, line_number + 1)),
                ']' | ')' | '}' => {
                    if let Some((open, _)) = delimiters.last()
                        && matches!((*open, ch), ('[', ']') | ('(', ')') | ('{', '}'))
                    {
                        delimiters.pop();
                    }
                }
                _ => {}
            }
        }
    }
    if let Some((delimiter, line)) = delimiters.first() {
        anyhow::bail!("Unclosed Mermaid node label '{delimiter}' at line {line}");
    }
    anyhow::ensure!(!quote && !edge_label, "Unclosed Mermaid label");
    Ok(())
}

fn raster_scale(width: f32, height: f32, scale: f32) -> f32 {
    // GPUI rasterizes at 2x. Downsample oversized graphs before allocation;
    // normal diagrams retain the monitor's full pixel density.
    scale
        .min(MAX_SIDE / (width * 2.0))
        .min(MAX_SIDE / (height * 2.0))
        .min((MAX_PIXELS / (width * height * 4.0)).sqrt())
}

fn render(key: &Key, renderer: &SvgRenderer) -> anyhow::Result<Rendered> {
    let (svg, width, height) = diagram_svg(key)?;
    let scale = raster_scale(width, height, f32::from_bits(key.scale));
    anyhow::ensure!(
        scale.is_finite() && scale > 0.0,
        "Invalid Mermaid raster scale"
    );
    let image = renderer.render_single_frame(svg.as_bytes(), scale)?;
    Ok(Rendered {
        width,
        height,
        image,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    const DEPLOYMENT: &str = "flowchart LR\nA[实时盘点] --> B[冻结源码]\nB --> C[构建与回归]\nC --> D[独立数据副本验证]\nD --> E[排空与停机备份]\nE --> F[仅切换DeerFlow镜像]\nF --> G{验收}\nG -->|通过| H[恢复服务并观察]\nG -->|失败| I[兼容性回退]";

    fn key(source: &str) -> Key {
        Key::new(source.to_owned().into(), false, 16.0, 1.0)
    }

    #[test]
    fn screenshot_flowchart_preserves_chinese_labels_and_branches() {
        let (svg, width, height) = diagram_svg(&key(DEPLOYMENT)).unwrap();
        assert!(width > height && height > 20.0);
        for label in [
            "实时盘点",
            "冻结源码",
            "独立数据副本验证",
            "通过",
            "失败",
            "兼容性回退",
        ] {
            assert!(svg.contains(label), "missing diagram label: {label}");
        }
        assert!(svg.contains("<path") && svg.contains("<text"));
    }

    #[test]
    fn common_diagrams_rasterize_with_visible_content_in_both_themes() {
        let renderer = SvgRenderer::new(Arc::new(()));
        for source in [
            DEPLOYMENT,
            "sequenceDiagram\nparticipant A as 用户\nparticipant B as 服务\nA->>B: 请求\nB-->>A: 返回",
            "classDiagram\nAnimal <|-- Duck\nAnimal : +int age",
            "stateDiagram-v2\n[*] --> Ready\nReady --> Running\nRunning --> [*]",
            "pie title 测试\n\"通过\" : 8\n\"失败\" : 2",
        ] {
            let light = render(&key(source), &renderer).unwrap();
            let dark = render(
                &Key {
                    dark: true,
                    ..key(source)
                },
                &renderer,
            )
            .unwrap();
            let pixels = light.image.as_bytes(0).unwrap();
            assert!(pixels.len() > 100 && pixels.len() <= MAX_PIXELS as usize * 4 + 32768);
            assert!(pixels.chunks_exact(4).any(|pixel| pixel[3] > 0));
            assert!(pixels.chunks_exact(4).any(|pixel| pixel != &pixels[..4]));
            assert_ne!(Some(pixels), dark.image.as_bytes(0));
        }
    }

    #[test]
    fn malformed_and_excessive_diagrams_fail_without_allocating_images() {
        for source in [
            "".to_owned(),
            "not a diagram".into(),
            "flowchart LR\nA[unclosed".into(),
            "x".repeat(MAX_SOURCE_BYTES + 1),
        ] {
            assert!(
                diagram_svg(&key(&source)).is_err(),
                "accepted malformed source: {}",
                source.chars().take(100).collect::<String>()
            );
        }
        let graph = format!(
            "flowchart LR\n{}",
            (0..MAX_NODES + 1)
                .map(|n| format!("N{n}\n"))
                .collect::<String>()
        );
        assert!(diagram_svg(&key(&graph)).is_err());
        let scale = raster_scale(100_000.0, 100_000.0, 3.0);
        assert!(100_000.0 * 100_000.0 * scale * scale * 4.0 <= MAX_PIXELS + 1.0);
    }

    #[test]
    fn flowchart_label_validation_accepts_shapes_and_literal_delimiters() {
        for source in [
            "flowchart LR; A((Start)) --> B{Decision}; B -->|Yes (valid)| C[Done]",
            "graph TD\nA>Asymmetric] --> B[(Database)]\nB --> C[/Input/]",
            "flowchart LR\nA[\"literal [ ({ text\"] --> B[\"more } ) text\"]",
            "%%{init: {\"theme\": \"dark\"}}%%\nflowchart LR\n%% [ comment\nA --> B",
            "flowchart LR\nA[\"line one\nline two\"] --> B\nclick B \"https://example.com/(\"",
        ] {
            validate_flowchart_labels(source).unwrap();
        }
    }

    #[test]
    fn workers_deduplicate_bound_queue_and_cache_failures() {
        let mut cache = Cache::default();
        let key = key(DEPLOYMENT);
        let view = EntityId::from(1);
        for _ in 0..100 {
            assert!(matches!(cache.request(&key, view), ResultState::Pending));
        }
        assert_eq!(cache.queue.len(), 1);
        cache.queue.pop_front();
        assert_eq!(
            cache.complete(&key, ResultState::Failed("invalid".into())),
            HashSet::from([view])
        );
        for _ in 0..100 {
            assert!(matches!(cache.request(&key, view), ResultState::Failed(_)));
        }
        assert!(cache.queue.is_empty());
        for n in 0..MAX_PENDING * 2 {
            cache.request(
                &Key {
                    source: format!("graph LR; A-->B{n}").into(),
                    ..key.clone()
                },
                view,
            );
        }
        assert_eq!(cache.pending, MAX_PENDING);
        assert_eq!(cache.queue.len(), MAX_PENDING);
        assert!(cache.waiting.contains(&view));
    }

    #[test]
    fn cache_evicts_settled_results_and_keeps_source_and_theme_separate() {
        let mut cache = Cache::default();
        let original = key(DEPLOYMENT);
        let view = EntityId::from(1);
        cache.request(&original, view);
        for n in 0..MAX_ENTRIES * 2 {
            let next = Key {
                source: format!("graph LR; A-->B{n}").into(),
                ..original.clone()
            };
            cache.request(&next, view);
            cache.complete(&next, ResultState::Failed("invalid".into()));
            cache.queue.retain(|key| key == &original);
        }
        assert_eq!(cache.entries.len(), MAX_ENTRIES);
        assert!(matches!(
            cache.entries[&original].result,
            ResultState::Pending
        ));
        let dark = Key {
            dark: true,
            ..original.clone()
        };
        cache.request(&dark, view);
        cache.complete(&original, ResultState::Failed("old result".into()));
        assert!(matches!(cache.entries[&dark].result, ResultState::Pending));
    }
}
