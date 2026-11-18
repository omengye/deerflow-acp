# 审查修复与桌面发布报告

日期：2026-11-17。范围：B1–B6（含 B6a/B6b）及 Terra 饮品证据缺陷。本轮经用户授权修改代码、测试、文档并更新本机桌面安装。本轮真实模型测试均使用 `gpt-6.1-terra`、临时合成 profile/workspace/database；仅从安装配置读取既有模型路线，测试不改写安装配置或用户记忆。

当前状态：全部修复、回归和本机发布验收完成，桌面程序已更新。可机读结果见 `docs/reports/memory-review-fix-20261117.json`；历史失败和主动中止的构建记录保持原样。

## 修复与契约

- B1：在预算终态归一和资源清理之后才判定是否未调用可选 PostRun；业务终态事务仅将无 job、无 audit/result/staging/显式操作的 pending 转为 `postrun_skipped`。failed/cancelled 的未处理输入为 discarded。waiting_input、不明预算及已开始提取不被标作成功。启动修复只接受 completed execution/run、无预算活动或未知支出且停止原因/原始事件可证明的旧数据；不缩小 Flush 历史集合，不修改 schema。
- B2：有限遗忘语法预编译，支持“请忘记我喜欢茶”“Forget that I like tea”“忘掉我的城市”，目标仍须匹配宿主解析的完整事实、属性或 ID。
- B3：裸“我在 / I am in”的地点名采用有限集，保留北京、上海等既有能力，并按 NFKC 判断兼容字符；value 仍是用户原文。其他地名需“住在 / 我的城市是 / I live in”明确类型声明。不是通用地理实体识别，也不以活动黑名单冒充此能力。
- B4：消除独立任务文字、英文子串和词内撇号导致的误拒；保留整段引用、第三方、条件/否定、权限与凭据检查。`Check if the build passes` 等有限的独立尾句不会把前面的事实变成条件声明；未支持的复杂语句仍保守拒绝。
- B5：保留 external_context 的风险信息及全部历史来源值；纠正“必定读过第三方内容、每次必须确认”的过强提示。用户证据与上下文暴露是不同维度；所有记忆仍不可信且不能授权工具。新指引 `memory-tools-v5`、`memoryGuidanceVersion=1`，固定 V3/V4/旧 guidance 保留，参数 schema V3、write policy v2、extraction schema 2、DB schema 5 不变。
- B6a：来源/范围的三处分支使用可选读错误分级和独立审计；不是用只认 OptionalWriteFailure 的写错误函数吞 raw error。
- B6b：校验 payload 与选中行身份，基于行 ID 和账本 member 结算；不可恢复来源以 source_integrity 处置。已付费响应保留，unknown 保持 blocked 和 hold；真实 SQL 故障返回错误且不删任务。
- Terra：宿主只从原文中恢复紧邻且完整的记住命令前缀，不能由另一句命令或模型补写授权；补充全跨度指引与可执行示例。

## 验收记录

首轮新增 validator 测试按预期复现旧缺陷。受影响模块首轮扩大回归暴露了全角 London 正例被有限地点集误拒；已修正 NFKC 名单判断，保留原值，并通过同值删除屏障回归。发布前补齐短 token 赋值的源级拒绝；坏数据矩阵还复现了 malformed manifest JSON 在旧清理 SQL 中阻塞的问题，现让它进入 source_integrity 结算/处置。已补事件、manifest、payload 的非法 JSON 以及 paid/unknown 与错误 payload 身份组合测试。终态覆盖补查还复现 refusal/content_filter 留 pending，已改为根据实际未调用钩子的证据判定新输入，历史修复补对应停止原因。原失败和三次主动停止构建的日志保留，最终交付以构建元数据记录的干净快照为准。

已完成的专项结果如下；不同批次存在重叠，不相加作为独立用例总数。原始记录位于 `C:\df-builds\memory-review-fix-20261117-evidence`。

| 验收 | 通过 / 失败 / 跳过 | 证据 |
|---|---|---|
| memory 整包（凭据与坏 JSON 修正后） | 279 / 0 / 3 | `memory-package-v2-final.jsonl`；缺失可选环境的用例由单独验收记录解释 |
| 最终终态及保护回归（SDK、memory、runtime、Eino） | 207 / 0 / 0 | `terminal-complete-final.jsonl` |
| 相关并发/恢复 race | 203 / 0 / 0 | `race.jsonl` |
| 审查修复 race | 119 / 0 / 0 | `review-race-final.jsonl` |
| 坏队列最终 race | 25 / 0 / 0 | `queue-race-final.jsonl` |
| 最终候选终态 race | 67 / 0 / 0 | `terminal-race-delivery.jsonl` |
| 安装库一致性副本迁移/重复打开/旧 schema 4 拒绝 | 3 / 0 / 0 | `preflight-migration.jsonl`；升级前又对排空后的新副本完成 2 项复演，见 `drained-migration-rehearsal.jsonl` |
| 最终候选真实模型质量/对抗/完整生命周期 | 16 / 0 / 0 | `terra-delivery-full.json`：43 次调用、61,524 tokens，held=0、unsettled=0 |

最终候选为独立干净快照 `f68caaf8b86748d49ca51e7d4b3fb04363d19aa9`。3,441 个冻结文件已与原工作区核对；非文档源码一致，主工作区 HEAD 保持 `68753f15e51ef285260bca52388c44c786ff0f41`，没有回退或覆盖原有未提交修改。快照中不包含 `.deerflow/` 本机状态。

## 最终发布与安装结果

| 项目 | 实际结果 |
|---|---|
| 完整 Go 发布回归 | 57 包，101 批；叶子 4209 通过、37 跳过、0 失败；顶层 1973 通过、32 跳过 |
| 正式构建与兼容验证 | Release、clean-rebuilt；Rust Bridge 21 项、源码身份 28 项、双模式协议冒烟、实际包内 Bridge 6 项均通过 |
| 交付完整性 | 98 个包文件 SHA-256 一致；源码与二进制 ZIP 均完成 SHA-256、CRC 及源码提交/包内容核对 |
| 升级前验收 | 停止新任务、等待在途工作完成，取得排空后的新 SQLite 一致性副本，重复打开/迁移与旧 schema 4 拒绝测试通过 |
| 本机更新 | `deerflow-skills-20261117-144122-823`；98 个已安装文件与交付包一致；桌面窗口响应，单实例 desktop/Waku/daemon，正常恢复任务接收 |
| 配置与数据 | Terra 配置文件摘要及有效设置不变，数据目录绑定不变；schema=5、integrity=ok；历史 profile/scopes/facts/revisions/slots/operations 摘要全部保持 |
| 安装真实模型生命周期 | 实际安装 Bridge + daemon + Terra，7 个模型驱动步骤全部通过；16 次调用、27,858 tokens；held=0、unsettled=0；隔离 daemon 已停止，临时凭据已移除 |

37 个叶子跳过保留原状态：其中 6 个需包内二进制的 Bridge 用例在构建后单独通过，真实记忆 opt-in 用例另以 Terra 完整 16 项通过。其余涉及 Windows 符号链接权限、未启用的 command provider/Poppler/WSL2/Docker/Python Manager/OCI/真实 Web/OpenCLI 等环境，以及未启用的真实 embedding；不把这些默认跳过算作通过，完整清单见机器记录。

安装生命周期覆盖：自动保存“请记住：我喜欢茶”并验证候选及落库 → 自然语言“请忘记我喜欢茶”删除 → 北京自动保存 → 删除后同值候选被拒绝 → 上海新值沿原 ID 恢复 → 完整重启 → 显式项目路径只写 workspace → 显式同值恢复 → 再次完整重启保持。验证数据库/回执/搜索/版本与账本，不以模型口头回复作为成功依据。

原安装已有的 failed job 保留，不将其清空或当作本轮新增失败；最终无 queued/running 记忆任务，账本无残留 hold 或未知模型调用。旧备份保留，新完整备份位于 `C:\Users\omeng\DeerFlowBackups\memory-review-fix-20261117`。备份用于恢复/取证，不允许直接用旧数据库覆盖升级后的用户事实，旧二进制也未获得恢复 V5 执行的兼容承诺。

交付目录：`D:\Tools\deerflow-api\dist\desktop\memory-review-fix-20261117`。实际更新目标：`D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-20261010-103944`。源码 ZIP 保留构建时文档状态；最终报告、修复计划、复核发现与日志另随 `acceptance` 交付。用户数据库副本和凭据配置不进入交付包。所有实际结果见该目录的校验清单及机器记录。


## 执行入口与范围

本机 Windows x64 使用 Go 1.26.8；正式构建不使用 `Development` 或 `SkipTests`。相关脚本及脱敏输出一并归档，数据库备份和临时凭据不进入交付证据目录。

```powershell
# 在独立快照执行正式构建，内部包含 Go 全量回归、Rust、源码身份、双模式协议及包内 Bridge 验收。
& C:\df-builds\memory-review-fix-20261117\scripts\build-windows.ps1 `
  -Package Desktop -Configuration Release `
  -DesktopTargetDirectory D:\Tools\deerflow-api\desktop-app\target `
  -OutputDirectory dist\desktop\memory-review-fix-20261117

# 在快照的 go-harness 下，设置报告所列 Go 环境；race 另启用 CGO 和本机 GCC。
go test -race . ./internal/memory ./internal/runtime -run '^TestMemoryReviewB1|^TestMemoryStage4' -count=1 -json

# opt-in 配置只提供模型路线；测试自行创建合成状态并输出结构化验收报告。
go test ./internal/launch -run '^TestMemoryLiveReleaseAcceptance$' -count=1 -timeout 35m -json
```

最终修复没有改变 embedding/索引实现；本次安装未启用真实 embedding 服务，保留前次固定模型验收证据，不将默认跳过视为本轮通过。有限自然语言规则、非本机部署、大规模检索性能和物理擦除等范围仍以修复计划第 8 节为准；这些不是本轮 B1–B6 修复的未完成项。
