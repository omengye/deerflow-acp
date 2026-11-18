# 记忆协议升级与回退运行手册

适用目标：阶段 0 核实的当前 Windows 本机安装。前次阶段 1–5 与模型指引修正已发布，实际数据库为 schema 5；历史部署见[前次发布报告](memory-desktop-release-report.md)。本轮已修复审查 B1–B6 和 Terra 饮品证据问题，正式发布与本机升级验收已完成，实际结果见[审查修复与发布报告](memory-review-fix-report.md)。此前 Terra 15/16 失败记录保留；本轮完整质量/对抗 16/16 通过，安装程序生命周期亦通过；不沿用旧结果宣称新包通过。所有升级检查针对实际数据目录的副本，真实模型使用合成 profile。

## 协议与行为

| schema | 增量内容 |
|---|---|
| 1 | 不可变输入来源、scope 删除代次、显式操作来源、旧 job 处置 |
| 2 | 字段写入策略、provenance、原始提取结果/ordinal、每日认领上限 |
| 3 | 来源 root 下的独立 memory_job member/attempt/fence、租约与计费恢复 |
| 4 | revision/model 索引失败次数、终态及服务退避 |
| 5 | 新受验证 revision 的结构化值身份；新输入的 memory-relearn-v1 授权条件 |

v5 不回填历史值，不修改前四次迁移 SQL/checksum，不重编码事实、旧操作或回执。没有可信值身份的旧 tombstone 需要删除后新的明确记住/更正请求或直接管理操作。新事实被删除后，只有不同的受支持结构化值能自动恢复，详细规则见[写入协议](memory-write-policy.md#删除后的安全重新学习阶段-5)。

本轮审查补修不新增数据库迁移：新执行使用 memory-tools-v5、memoryGuidanceVersion=1，参数 schema 仍是 V3；固定 V3/V4 与缺省 guidance 的旧执行保留原指引和工具契约。自动提取的新提示词仍走既有来源快照、请求摘要及预算预留；已保存结果继续原样重放，不因指引变化重新调用模型。启动会按完整执行/来源/预算证据修复旧 postrun_skipped 决策；该变化允许修改决策列，不允许改写输入 manifest、历史事实和回执。

## 发布前验收

1. 保存实际构建所用源码快照、提交号、未提交差异、配置和二进制 SHA256；不能只用工作区基线提交号标识带修改的程序。
2. 在目标安装的数据库一致性副本上验证迁移中断回滚、重复升级、历史字段摘要、旧回执重放、旧队列处置和不覆盖升级后事实的回退。
3. 在支持 CGO/race 的环境完成受影响包的 race 检查；完成真实模型质量/对抗与真实 embedding 验收，实际模型调用仍受预算约束。跳过的测试单独记录。
4. 完成完整桌面/Bridge/配置工具交付包构建与安装冒烟。单独 daemon 编译、无网络的 SDK 打开副本和普通单元测试不替代这些发布门槛。

本轮副本测试从通过只读 SQLite backup API 取得的快照再复制到测试临时目录，环境变量指定的文件只读。复跑时从 `go-harness` 目录使用相应 Go 工具链：

~~~powershell
$env:MEMORY_STAGE5_DATABASE_COPY = 'C:\offline-rehearsal\harness-backup.db'
$env:MEMORY_STAGE5_SCHEMA4_PROBE = 'C:\offline-rehearsal\schema4-probe.exe'
go test . ./internal/memory -run '^TestMemoryStage5' -timeout=180s -json -count=1
~~~

`schema4-probe.exe` 是从升级前阶段 4 源码构建的最小存储打开程序，只执行 SQLite 打开与 memory.New，不启动 worker；必须保留来源与二进制摘要。缺少这些外部材料时对应集成测试明确 skip，不能把结果记成迁移/回退通过。真实安装旧程序可能连 schema 账本都不认识，不能用阶段 4 探针拒绝 v5 的结果证明它也安全。

### Windows 本机补充验收

以下路径是 2026-11-17 核实的本机路径；其他机器先核实工具链。阶段 5 当时未发现的 GCC 实际位于仓库构建缓存中，无需修改系统配置。新开 PowerShell，在 `go-harness` 目录执行；先设置上面的只读副本/探针变量。

~~~powershell
$env:GOROOT = 'D:\GO_ENV\GOPATH\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.8.windows-amd64'
$env:GOTOOLCHAIN = 'local'
$env:CC = 'D:\Tools\deerflow-api\.build-cache\tools\w64devkit-2.10.1\w64devkit\bin\gcc.exe'
$env:CGO_ENABLED = '1'
$env:PATH = (Join-Path $env:GOROOT 'bin') + ';' + (Split-Path $env:CC) + ';' + $env:PATH
$env:GOMAXPROCS = '2'
go test -race -p=1 -parallel=1 ./internal/memory ./internal/budget ./internal/engine/eino ./internal/launch ./internal/acp/agent ./internal/configschema ./internal/tools ./internal/learning ./harness -timeout=1800s -json -count=1
go test -race -p=1 . -run 'Memory|Personal|SkillLearning.*(External|Provenance|Memory)|ExternalContent' -timeout=1800s -json -count=1
go test -race -p=1 ./internal/runtime -run 'Memory|Execution|Continuation|Background|RunQueue|Budget|Session.*(Delete|Archive|Retention|Cleanup)|SkillLearningAutoPatch' -timeout=1800s -json -count=1
~~~

不要把这些重型测试与 Rust 发布链接、时限敏感的桌面冒烟同时运行。原始并行检查中长任务用例触发了夹具的 60 秒时限，降低并行度后单独复测通过；保留两次记录，不能把原始整包退出码改成成功。以上建议顺序与并行度不改变测试内部断言；本轮实际命令和结果以发布验收记录为准。

真实模型验收是显式 opt-in，默认测试跳过，不读取安装配置或发出真实请求。启用后只使用所选模型路线和网络配置，每例使用全新临时 workspace/profile/数据库。当前默认路线为 `gpt-6.1-sol` / `responses`；费用受每个 run 4 次模型调用、4 次工具调用、40000 token 总额与单次 1024 输出 token 上限约束，地点生命周期最多包含 3 个 run。不要把账本 token 数当作货币费用。

~~~powershell
$env:DEERFLOW_MEMORY_ACCEPTANCE_CONFIG = 'D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-20261010-103944\user-data\config\config.yaml'
$env:DEERFLOW_MEMORY_ACCEPTANCE_REPORT = Join-Path $env:TEMP 'memory-live-report.json'
go test ./internal/launch -run '^TestMemoryLiveReleaseAcceptance$' -timeout=1200s -json -count=1
Remove-Item Env:DEERFLOW_MEMORY_ACCEPTANCE_CONFIG
Remove-Item Env:DEERFLOW_MEMORY_ACCEPTANCE_REPORT
~~~

报告只保存合成用例名、状态、候选数量、工具回执/固定校验原因、参数形状、调用/token/hold 计数，不保存密钥、安装记忆、provider 原始响应或记忆正文。hold/usage 未决时测试停止后续调用，先核账，不为获取绿灯自动重试。自动正样本返回空候选、显式路径未写入时应保持失败；文件攻击用例必须有成功读取含攻击内容的持久回执，不能只观察模型尝试调用工具。修复后 16 项验收已通过，自动同值步骤还断言候选确实抵达宿主并取得 same_deleted_value；恢复后校验同一事实 ID、revision 增长及 Close/Open 持久性。阴性结果与阳性结果必须共同评估。

当前安装没有启用 embedding。真实 embedding 在临时环境使用固定模型验证，不配置到安装。可从仓库根目录准备 Python 3.12 环境（本机 Python 3.15 无可用 onnxruntime wheel）：

~~~powershell
$acceptanceDir = Join-Path $env:TEMP 'deerflow-memory-embedding-recheck'
uv venv --python 3.12 (Join-Path $acceptanceDir 'venv')
$acceptancePython = Join-Path $acceptanceDir 'venv\Scripts\python.exe'
uv pip install --python $acceptancePython fastembed==0.7.4
$env:MEMORY_MODEL_DIRECTORY = Join-Path $acceptanceDir 'model'
& $acceptancePython deploy/docker/single-user-memory-20261027/download_model.py
uv pip freeze --python $acceptancePython > (Join-Path $acceptanceDir 'requirements.txt')
$adapter = Get-Content deploy/docker/single-user-memory-20261027/embedding_server.py -Raw
$adapter = $adapter.Replace("specific_model_path='/opt/deerflow-memory/model'", "specific_model_path=os.environ['MEMORY_MODEL_DIRECTORY']")
$adapter = $adapter.Replace("HTTPServer(('0.0.0.0', 8080)", "HTTPServer(('127.0.0.1', 18080)")
('import os' + "`n" + $adapter) | Set-Content (Join-Path $acceptanceDir 'embedding_local.py') -Encoding utf8
$env:HF_HUB_OFFLINE = '1'
& $acceptancePython (Join-Path $acceptanceDir 'embedding_local.py')
~~~

下载器验证固定 revision `46fbe35fd4374a00fee7de77dfddaeb6dd6a2c59` 的全部文件摘要。上述服务仅监听 loopback；如端口占用，服务和测试一致改用空闲端口。另一个终端在 `go-harness` 目录设置 `MEMORY_EMBEDDING_TEST_URL=http://127.0.0.1:18080`，运行 `go test ./internal/memory -run '^TestPersonalMemoryChineseEmbeddingAcceptance$' -timeout=300s -json -count=1`。完成后 Ctrl+C 关闭自己启动的临时服务，并删除本终端该环境变量。验收须有 20 个已落库的 512 维有限非零向量、100 次成功 query embedding 和 Recall@5≥0.95；词法回退不算真实 embedding 成功。记录 p95，但这 20 条语料不能证明 10000 向量规模的性能。

桌面包验收从仓库根目录、沿用固定 Go 工具链执行，输出目录须为新的路径：

~~~powershell
cargo test --release --locked --manifest-path bridge/Cargo.toml --config scripts/cargo-windows-static-crt.toml
./scripts/test-desktop-source-identity.ps1
./scripts/build-windows.ps1 -Package Desktop -Configuration Release -Development -SkipTests -DesktopTargetDirectory desktop-app\target -OutputDirectory dist\desktop\memory-local-recheck
~~~

开发包命令仍重新构建桌面、Waku daemon、Rust Bridge 和 Go 配置/控制/技能工具，并运行两种协议冒烟及包内 Bridge 集成检查。`-SkipTests` 跳过的是广泛回归；不能据此把 `BUILD_PACKAGE.json` 的 `regression_tests=skipped_development` 改成 passed。冒烟使用合成配置、假模型和复制的临时包，不操作当前安装，也没有视觉/UI 验收。

正式交付须在审阅后的干净提交上重新运行完整构建，去掉 `-Development -SkipTests`，执行脚本规定的全量回归并产生源码/二进制 ZIP 及 hash manifest。当前工作区包含既有未提交修改，不能自动提交所有改动或绕过 clean-source 检查来声称正式包通过。开发包冒烟、正式交付包和目标安装升级冒烟分别记录。

本次实际使用独立 worktree `C:\df-builds\memory-20261117`，将当前源码按文件摘要固化为本地 `codex/memory-release-20261117` 分支的 `86e83d6bf4e5260935cdc1b361efc0325838ce60`，不改变原工作区 HEAD 或推送。正式构建未跳过测试，源码/二进制 ZIP 和安装文件均已校验。验收报告是构建后的独立证据，不修改已签定摘要的交付 ZIP。

## 上线顺序

1. 重新确认运行程序、有效配置和 data-dir。阶段 0 的本机有效位置是 `dist/desktop/DeerFlow-Desktop-Go-20261010-103944/user-data/config/go-harness-state`；以当次启动的实际参数为准，不根据目录默认值猜测。勿把其他便携安装的 profile 或数据库混入。
2. 停止新任务准入，等待已有调用完成或有序取消，关闭前台执行、自动提取、索引及所有持库进程。已分派但无完整结果/usage 的调用保留 unknown 和 hold；只有从未分派的预留可释放。`daily_attempts: 0` 只暂停新调用，已保存结果仍可能发布，不能替代停止程序。
3. 用 SQLite 一致性备份保存完整数据库，并保存配置、旧程序和构建标识。单独复制活动的主 db 文件可能遗漏 WAL；要使用 backup API，或在所有持库者关闭且 checkpoint 成功后备份。校验备份 `integrity_check` 与摘要，保留只读原始备份，之后的演练只修改其复制品。
4. 先在副本执行上述验收，再安装经过验证的交付包。首次打开由程序持独占数据目录锁执行增量迁移；迁移 journal 与该次增量 DDL 同事务，失败时保持停止准入，保留错误并修复原因后重试。禁止手工删 journal、修改 checksum 或给旧 job 填当前快照。
5. 启动时先恢复记忆调用，再恢复通用预算中断。完整付费响应只结算/重放，不重新请求模型；未验证的旧 queued/running/result job 标记 discarded/legacy_unverifiable，并记录 legacy_usage_unavailable。已完成事实和回执保留；unknown 不随队列处置释放。
6. 检查 schema=5、profile、历史 ID/revision/回执摘要、队列处置、预算 hold 和新输入清单。在允许真实模型验收的预算内执行“新声明→保存→删除→新不同值”的单次验证，同旧值应要求显式恢复。确认 SDK/ACP Flush 状态、词法可读与新 revision 索引；再恢复常规任务准入。

## 监控与排错

- Flush 的 pending/blocked/failed 不应报成完成；requires_explicit、quota_skip、no_facts 属于终态 skipped，不表示保存了事实。混合批次的逐项拒绝见 memory_extraction_items，输入整体 completed 也不是新增事实数。
- 查看 superseded、rejected（按固定原因码）、requires_explicit、quota_skip、discarded、blocked/budget_unknown、最老 queued 时间、available_at 与索引 terminal。索引诊断使用 SDK MemoryIndexFailures；原文、模型响应和凭据不应写入运行日志。
- 源事件缺失或旧清单没有策略时要求新输入，不补签来源。没有结构化值身份时要求显式请求，不根据旧正文猜测身份。
- unknown 需要有证据的账本核对，不能清空 hold 让 worker 重试。索引 terminal 按 revision/model 隔离，新的事实版本有新机会；当前没有人工清零 terminal 的专用 UI/API。

## 回退

先停止准入与所有持库进程，备份**最新**升级后数据库。只可回退到已验证兼容 schema v5、删除屏障、来源规则、预算账本和当前固定执行契约的程序。schema 相同不代表旧包可以恢复 memory-tools-v5 / guidance=1 的新执行；旧版本对未知契约的拒绝必须保留。遇到旧读库程序明确拒绝新版本，应保持拒绝，不绕过 journal 检查。

若原安装版本不能理解新协议，保持服务停止或使用兼容的新版本关闭模型记忆访问，待修复后恢复。不得直接运行旧程序写新库，也不得把升级前备份覆盖已有新事实的数据库。旧备份只用于隔离诊断和有审阅的数据恢复；任何数据恢复都必须保留升级后的用户事实、操作回执、删除历史和未知预算记录。自动恢复旧备份不属于安全回退。
