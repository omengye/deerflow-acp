# 搜索插件与即时切换

`web_search` 通过同一个 Go Provider 接口使用 Brave、DeepSeek 或 SearXNG。
三个插件随程序交付，无需安装搜索 Skill 或额外运行 Python/Node.js。
DeepSeek Fetch 同时接入 `web_fetch`，搜索与抓取的默认服务独立保存。

## 用户操作

- 打开“设置 → 工具”，点击 `web_search`，在右侧选择 Brave、DeepSeek 或 SearXNG。
- 已配置服务点击即切换并保存；未配置服务填写后点击一次“保存并启用”。无需重启，
  无需全局“保存并应用”，不显示保存成功弹窗。当前服务和配置状态直接显示在设置页。
- Brave、DeepSeek 要求 API Key，默认使用官方服务地址；SearXNG 实例地址必填，Key 选填。
- 三家独立保存凭据，切换不清空其他服务。密钥始终掩码，留空保留已有值。
- 高级设置提供服务地址、代理和超时；SearXNG 可选择 Bearer / X-API-Key 或清除 Key。
- 全局代理在“设置 → 常用 → 网络代理”配置并通过“保存更改”应用。未设置独立代理的
  搜索服务继承全局配置；服务高级设置中的独立代理优先。DeepSeek Fetch 使用 DeepSeek
  服务的代理，本地 `web_fetch` 使用其工具代理或全局代理。
- 错误显示在表单内，保存失败不会切换默认服务。并发修改导致版本冲突时重新读取后再保存。
- `web_fetch` 的本地 / DeepSeek Fetch 选择放在对应工具设置中，与搜索服务分别保存。
- 会话输入区不再提供搜索选择器和密钥表单。缺少配置的搜索显示“前往设置”，保存后
  继续原查询；停止任务取消等待。非交互 SDK 返回指向设置页的配置缺失错误。
- “这次用 DeepSeek 搜索”由模型使用 `web_search.provider`，不修改默认值。
- “以后默认用 SearXNG 搜索”由模型调用 `set_search_provider` 保存默认值。
- 模型工具不接受密钥。用户只在设置页中输入凭据。

## 初始化与兼容

发行模板的 `search` 段提供三家默认配置。第一次安装只生成通用环境变量引用，
不复制个人地址或凭据。默认 `auto` 依次选取已经配置的 Brave、DeepSeek、SearXNG；
未配置任何服务时显示 Brave 待配置，保存明确选择后不再自动更换。

自动复用来源：

| 服务 | 配置来源 |
| --- | --- |
| Brave | `search.providers.brave`、`BRAVE_API_KEY`、旧 `tools[].api_key` 与代理/超时 |
| DeepSeek | `search.providers.deepseek`、`DEEPSEEK_API_KEY`、官方 `https://api.deepseek.com` 模型凭据、旧技能文档规定的用户 `.config/deepseek-search/config.json` |
| SearXNG | `search.providers.searxng`、`SEARXNG_BASE_URL`、可选 `SEARXNG_API_KEY` 或兼容的 `SEARXNG_TOKEN` |

DeepSeek 不复用第三方中转服务的模型密钥。SearXNG 未填写 Key 时不发送鉴权头；
填写后默认使用 Bearer，也可在高级设置选择 `X-API-Key`。实例需要启用 JSON 输出。

运行中的选择及配置保存到主配置同目录的 `search-providers.json`，该文件优先于
YAML 初始化值。修改搜索不改写 `config.yaml`，不影响其他设置草稿，也不触发全局应用。
已有搜索偏好不会被后续初始化覆盖。初始化会先持久保存搜索配置，再移除旧
`tools.web_search` 中的 `api_key / https_proxy / timeout`，原文件备份为
`config.yaml.search-migration.bak`。迁移保留其他设置与注释；失败时保留旧字段以便重试。
新模板和设置界面均不再提供这些兼容字段，`max_results` 仍属于工具参数。配置 CLI 和 daemon 使用同一文件、
跨进程锁及版本检查；每个新执行读取当前搜索配置。

`resources/desktop-default-config.yaml`、`desktop-app/resources/deerflow/default-config.yaml`、
`resources/default-config.yaml` 与 `config.example.yaml` 均包含相同搜索默认定义。
桌面打包脚本继续从 desktop-default-config 复制发行资源，无需安装器额外写入凭据。

## 执行与结果契约

输入保留 `query / max_results / time_range`，新增可选 `provider / language / ori_question / need_content`。
结果保留 `query / total_results / results`，新增实际执行的 `provider`。结果条目保留
`title / url / content`，DeepSeek 可返回长短摘要、正文、评分、日期等额外字段。
SearXNG 保持分数排序并按 URL 去重；各家结果均过滤无效 HTTP(S) 来源地址。

- Brave 的时间范围映射到 freshness。
- DeepSeek 将时间范围和语言转换为查询语法，总查询不得超过 128 个 Unicode 字符；
  超长时明确返回错误，不静默截断。
- SearXNG 仅支持 day/month/year；week 明确返回不支持。
- 切换不自动重试其他 Provider；401/403、402、429、网络错误保留可识别的错误信息。
- HTTP 响应最多 2 MiB，代理显式配置，请求可取消；跨源跳转移除所有搜索鉴权头。

新任务固定选择及各 Provider 的配置摘要。修改默认服务立即影响新任务，已经执行的
请求和断点恢复保持原来的服务与配置版本。只含摘要的资源引用进入执行状态；凭据保留
在宿主配置和同目录的私有 `search-providers.json.versions` 中。首次补齐缺失配置的
交互调用会使用用户刚保存的配置继续执行。

## 验证

```powershell
cd go-harness
go test ./internal/network ./internal/websearch ./internal/tools ./internal/configschema ./internal/desktopconfig ./internal/launch ./harness -count=1 -timeout 10m
go test . -run 'TestSearchPlugins|TestBuiltinPolicy|TestGoOpenCLI' -count=1 -timeout 5m
```

测试覆盖四份发行模板首次初始化、重复初始化不覆盖、无 Key 的 SearXNG、旧配置与凭据复用、
旧字段移除前持久保存、原 YAML 精确备份、迁移失败保留原字段、并发版本检查、取消等待及补齐配置后继续原查询。HTTP fixture 验证三家请求协议、结果、
凭据隔离；一个持续运行的真实 Go/Eino runtime 和同一会话连续切换三家，检查实际 HTTP
调用和工具结果，临时指定保持默认值，默认偏好工具即时保存。它们不替代外部服务在线验收。

2026-11-03 验证记录：

| 检查 | 结果 |
| --- | --- |
| `websearch/tools/configschema/desktopconfig/launch/harness` 六个 Go 包整包回归 | 535 项通过 |
| Go SDK / 主运行时整包回归 | 397 项通过 |
| Rust 搜索表单与工具参数单元测试 | 5 项通过 |
| Rust 剪贴板导入回归 | 4 项通过 |
| `go build ./...`、相关包 `go vet`、`git diff --check` | 通过 |

两份桌面默认模板的 SHA-256 相同。首次安装测试实际执行初始化、daemon 配置加载、
搜索配置保存、三家选择及重复初始化；原 YAML 不会被切换操作改写。在线服务账户、
实际安装器 GUI 和新界面的人工交互未纳入本轮自动化验收。

本轮桌面代码通过测试构建。图片粘贴采用原文件优先、位图兜底：同一剪贴板的文件导入
成功后忽略其位图；全部原文件导入失败时自动使用位图。多文件保留所有成功导入项，
部分失败显示错误，不追加文件列表的预览位图。文件读取与导入均在后台执行。
Rust 回归覆盖上述场景与位图保存失败，不执行 GUI 视觉测试。
