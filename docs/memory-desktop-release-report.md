# 记忆修复桌面交付与本机升级报告

日期：2026-11-17。范围：当前 Windows 本机便携安装；承接[真实模型修复报告](memory-model-fix-report.md)。本轮用户已授权重建交付包并更新桌面程序。

**正式交付包、完整发布回归、双模式协议冒烟、包内 Bridge 验收和本机升级已完成；安装程序的完整记忆生命周期已使用用户随后授权的 `gpt-6.1-terra` 通过。** 原默认 `gpt-6.1-sol` 本轮因 `credit_balance_exhausted` 无法复验。追加的 Terra 质量/对抗测试为 **15/16 通过**，饮品偏好证据不完整的失败定向复测仍存在；追加审查还确认 **B1 的 P1 Flush 缺陷未修**，见[复核报告](memory-review-recheck-report.md)。不能将当前包称为所有修复与质量门槛均已关闭。升级时保留原配置；收尾发现 default_model 已另行变为 Terra，当前服务已应用，本轮没有回写该配置。

## 1. 源码与交付物

原工作区基线为 `68753f15e51ef285260bca52388c44c786ff0f41`，包含已有未提交修改。为保留原工作区，使用独立 worktree `C:\df-builds\memory-20261117`，在本地分支 `codex/memory-release-20261117` 固化精确构建快照：

`86e83d6bf4e5260935cdc1b361efc0325838ce60`

快照包含当前构建输入及已有改动，不仅包含记忆修复；3427 个源文件按 SHA256 核对，排除 `.deerflow/` 本地状态、被 Git 忽略的安装数据和构建产物。原工作区 HEAD 未改变，没有推送。模型修复的三个生产文件及三个测试文件与前次报告中的摘要逐一一致。发布后记录写入当前工作区和独立验收目录，不回写已校验的源码 ZIP。

从干净快照运行正式入口，**没有使用 `-Development` 或 `-SkipTests`**：

```powershell
./scripts/build-windows.ps1 -Package Desktop -Configuration Release `
  -DesktopTargetDirectory D:\Tools\deerflow-api\desktop-app\target `
  -OutputDirectory dist\desktop\memory-release-20261117
```

工具链为 Go 1.26.8、Cargo/Rust 1.99.1。正式产物已复制并逐文件核对到 `D:\Tools\deerflow-api\dist\desktop\memory-release-20261117`。

| 交付物 | SHA256 |
|---|---|
| `DeerFlow-Desktop-Go-source.zip` | `e6e4a63989eafadf52a4ed108b43bcac2d6805e5ad9b4e150b547bcda051db1f` |
| `DeerFlow-Desktop-Go-windows-x64.zip` | `f2f06007c6f4d7413e78d465d3de4dfc460ca2c11a81bda6ac51e3de88ecb652` |
| `DeerFlow-Desktop-Go/PACKAGE_FILES.sha256` | `502820de13372ad6b48272d8b12c1da5de9bef3bb060d027a6ceca742de0f5ff` |

包中 `BUILD_SOURCE.json` 为 `clean-rebuilt`，提交号与源码 ZIP 一致；`BUILD_PACKAGE.json` 记录 `regression_tests=passed`、`desktop_protocol_smoke=passed_both_modes`、`packaged_bridge_tests=passed`。98 个包文件通过摘要核对；交付目录复制验证共 103 个文件（包括 ZIP 与校验文件）。

## 2. 发布回归与协议验收

| 项目 | 本轮结果 |
|---|---|
| Go 全量 `./...` 发布回归 | 57 个包、101 批；1993 个顶层用例均有记录，1961 通过、32 跳过；按叶子计 **4076 通过、37 跳过、0 失败** |
| Rust Bridge | **21 通过、0 失败**（19 个单元用例、2 个启动集成用例） |
| 源码身份夹具 | **28 项通过** |
| Go 分批测试与构建入口夹具 | 两套脚本均通过 |
| 桌面更新脚本夹具 | 通过；最初外层包装误读预期拒绝测试留下的原生命令退出码，单独记录并纠正包装判断，没有修改产品或降低断言 |
| 标准桌面协议冒烟 | 通过，清理错误为 0 |
| `--legacy-empty-mcp-policy` 冒烟 | 通过，清理错误为 0 |
| 实际包内 Bridge/daemon/stdio 集成 | `^TestExistingRustBridge` 的 6 个顶层用例通过，包耗时 15.203 秒 |
| 最新目标数据库副本迁移复演 | 停止新任务准入后重新备份，再运行 `^TestMemoryStage5InstalledCopy`；SDK 与 memory 两个用例通过 |

两种冒烟均使用包内实际程序、Waku WebSocket 和 ACP 协议以及隔离的合成配置/假模型，覆盖启动故障修复、会话与历史恢复、流式回复、取消、工具审批、MCP 策略、配置应用、删除及重启保持等。没有视觉/UI 操作验收。与上一轮开发包不同，本轮正式构建的两次协议冒烟均首次通过。

37 个叶子跳过逐项保存在[机器记录](reports/memory-desktop-release-20261117.json)：包含 Windows 符号链接权限、未启用的 Python/WSL/Docker/OCI/外部服务，以及真实模型、embedding 和包内 Bridge 的 opt-in。其中包内 Bridge 在构建后单独执行并通过；真实模型的追加结果见第 4 节，不计入本轮默认全量回归的通过数。此前真实 embedding、受影响包 race 及模型指引恢复 race 的证据沿用原报告，本轮未重复这些集合，也没有把普通全量回归称作 race。

## 3. 本机程序升级与数据核查

- 更新位置：`D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-20261010-103944`。
- 完整旧程序/用户数据备份：`C:\Users\omeng\DeerFlowBackups\memory-release-20261117`；885 个用户数据文件经摘要验证。
- 更新后 build ID：`deerflow-skills-20261117-110010-950`；9 个 EXE 已替换，98 个安装文件与验收包摘要一致。
- 升级后已验证桌面、Waku daemon、Go daemon 均从目标目录运行，窗口存在且进程响应正常，任务准入恢复。既有微信通道更新到包内版本，配置、认证与绑定保留，没有发送消息。收尾时桌面和 Waku 已不在运行，目标 Go daemon 仍正常；保留这一当前状态，没有再次拉起窗口。

先停止新任务准入并等待活跃执行结束。首轮等待发现一条旧 `queued` 任务已没有来源 run，无法靠等待完成；核对来源和账本后，将其保留在备份并交由已验证的迁移处理，没有人工删除或补签来源。停止准入后的最新 SQLite backup API 副本通过迁移复验，再关闭目标持库进程、备份全部用户数据并替换程序。升级脚本重启后自动恢复准入；随后完成只读迁移核查。

| 核查 | 结果 |
|---|---|
| 有效配置与绑定 | 升级后首次核查 SHA256 保持 `4b4e095f894a2fd4cf116844c4e617450ad8f4033f97e06f6864549b868243a4`；收尾为 `2f5c8906a84016fb71b181d0df062f806ead70b6e00e875cc1d526650fe04e62`，文件与有效 revision 一致。唯一语义差异是 default_model 从 sol 变为 Terra（文件写入时间 11:42:41），其他配置字段和 data-dir 保留 |
| 数据库完整性 | `integrity_check=ok`；旧库无迁移 journal，更新后记录 1–5，schema=5 |
| 历史数据 | profile、4 个 scope、13 个事实、16 个 revision、13 个 slot、16 个操作回执的既有字段摘要全部一致 |
| 旧任务 | 179 completed、1 个既有 failed 保留；1 条孤立 queued 转为 discarded，处置原因为 `legacy_unverifiable` |
| 运行与预算 | 活跃执行、排队执行均为 0；没有 queued/running 记忆任务；hold=0、未结算模型调用状态为空 |

关闭后的原始数据库备份 SHA256 为 `277af2b5f0f54e5e5a35e183bd115ef5300d6eaa7ef427d4aab963aac0f60cb5`。完整备份含私有配置与历史，保留在本机备份目录，不放入交付 ZIP。禁止将旧程序直接用于已升级数据库，也不能用升级前数据库覆盖后续新增事实；回退规则见[运行手册](memory-upgrade-runbook.md)。

## 4. 安装后真实模型验收与替代路线

尝试使用**目标安装的 Rust Bridge 和 Go daemon**，读取当前模型/网络路线，在独立临时 profile、workspace、数据库中执行自动保存→删除→同值拒绝→新值恢复、显式路径与显式同值恢复、Bridge/daemon 重启保持。没有向实际用户 profile 写入合成事实。

两次尝试均在第一个前台请求阶段停止：每轮 4 次模型调用上限触发，前台 `max_turn_requests`；持久模型 timing 全为 failed，usage 为保守估算，没有 worker 调用或候选。两轮共 **8 次账本调用、20720 个估算 token、hold=0、未结算调用=0**。这些不是提供方实际 token 用量，也不是记忆返回空候选的复发证据。

额外做了 1 次直接传输诊断，输出上限 512 token。服务返回 HTTP 200 后在 SSE 中给出 `response.failed` / **`credit_balance_exhausted`**，usage 缺失。该诊断不在 harness 账本内，单独计数，实际消耗未知；不能算作成功模型调用。临时配置中的凭据均已清理，隔离 daemon 已停止。

用户先回复“保留为待验收，先汇报已完成结果”，随后明确授权“**可以使用模型 gpt-6.1-terra 进行测试**”。本轮测试只在隔离 profile 选择现有 Terra 路线，没有改写安装配置；实际安装 default_model 的另行变化按第 3 节记录，不覆盖当前设置，也不推断修改来源。

Terra 首轮已通过自动保存、删除后同值候选拒绝和新值重新学习，但测试客户端默认取消工具审批，显式路径停在 waiting_input；这不是记忆校验失败或 B1。修正测试客户端，将受限于三个记忆工具的隔离会话设为允许执行后，使用全新数据目录复跑：

| 安装 Bridge/daemon 生命周期 | 结果 |
|---|---|
| 自动保存北京 | 实际产生并提交 1 个候选 |
| 删除后再次声明北京 | 候选实际到达，same_deleted_value 拒绝；列表/搜索无已删事实 |
| 新声明上海 | 恢复同一事实 ID，revision 为初始值+2 |
| 显式项目路径 | remember_memory 回执成功，路径仅写 workspace，global 保持原事实 |
| 新显式请求恢复相同上海值 | 同一事实 ID，revision 再增长 2 |
| 完整 Bridge/daemon 重启 | 自动与显式结果、ID/revision/content 均保持；schema=5、integrity_check=ok |

完整轮 **12 次模型调用、16912 token、3 次已结算 worker 调用**，hold=0、未结算调用=0。前一轮夹具审批中断另记 **8 次、9983 token**。两轮原始记录均保留，所有临时配置凭据已清理。

为核对替代路线，另对同一生产源码运行完整 16 项质量、对抗及重新学习验收。测试 overlay 仅将所选模型改成 Terra，直接只读原安装配置，没有生成额外凭据配置文件或修改生产源码。结果 **15 通过、1 失败、0 跳过**：姓名、地点、语言、数据库、所有攻击/引用/权限/凭据拒绝、外部文件与真实声明混合、显式路径和两种重新学习均通过；`generic_drink` 返回 1 个候选，但被 `incomplete_declaration` 拒绝，可见事实数 0。该项单独复测同样失败，故不能声称 Terra 的完整质量验收通过，也不是原来的“模型返回零候选”。本轮未放松完整声明验证来绕过它。

完整质量轮 **43 次模型调用、52477 token**；饮品定向复测 **2 次、2187 token**；均 hold=0、未结算调用=0。以上 Terra 检查合计 65 次、81559 token，与原默认路线的估算费用、无账本传输诊断及上轮历史验收分开统计。当前安装未配置 embedding，本轮未更改该设置。

待办变为：原默认模型额度恢复后的同路线复验、Terra 饮品偏好的完整证据质量改进，以及追加审查报告中尚未修复的问题（优先 B1）。替代路线的生命周期通过不能抵消这些未通过/未修项目。

## 5. 证据位置

- 汇总：[机器可读发布与升级记录](reports/memory-desktop-release-20261117.json)。
- 构建、双模式冒烟、迁移、更新及两轮模型失败日志：`D:\Tools\deerflow-api\dist\desktop\memory-release-20261117\acceptance`。
- 全量 Go 分批清单与原始 JSON 日志保留在独立构建目录的 `.build-cache/go-tests`，具体路径与摘要在机器记录中。
- 本轮未修改生产代码；原有未提交工作保留。正式包包含模型指引修正，安装生命周期由 Terra 验证通过；原默认路线不可用、Terra 一项质量失败及追加审查缺陷单独跟踪。
