# 运行 token 预算与模型窗口压缩调整

默认关闭累计 token 硬上限。原默认值 200,000 会把每次模型调用重传的历史、工具结果和工具定义都累计计算，导致正常任务提前停止。保留模型调用次数、工具调用次数、单次输出和执行时间限制。

成本预算可通过 `--max-tokens`、SDK `Config.Budget.MaxTokens` 或宿主 YAML `local_acp.max_run_tokens` 显式开启，零值关闭。CLI 优先于 YAML。显式预算剩余不足四分之一时，在模型请求中加入收尾提示，该提示也计入预算，不改写持久化用户输入。

桌面、Go 可执行程序和随包配置默认启用自动压缩。每个代理根据实际选择的模型及角色输出设置计算阈值。

```text
available = max(1, context_window - output_cap - max(16, context_window / 50))
trigger = max(1, available * 3 / 4)
```

例如，输出上限 4,096 时，32,768 窗口在 21,012 输入 token 左右压缩，128,000 窗口在 91,008 左右压缩，1,048,576 窗口在 767,631 左右压缩。未配置窗口时才使用 32,000 兜底值。显式 token 阈值可以提前压缩，并会受较小窗口约束。默认不开启独立的消息数量触发，避免大窗口模型在上下文很小时过早压缩。

压缩保留原始用户请求和至少八条近期消息，并向前扩展至完整工具调用组。首个长任务中的旧工具往返也可归入摘要，多次压缩仍保留原请求。没有可缩减的历史时跳过摘要，交由模型窗口检查处理。原始工具结果继续保存在回执和私有快照中，模型侧保留有界预览，需要细节时通过 `read_tool_output` 读取。

已有中断任务继续使用原始不可变预算和执行协议；新请求使用新策略。SDK 嵌入方通过 `Config.Compaction.Enabled` 显式启用压缩。

## 验证

以下模块测试通过：

```powershell
go test -p 2 ./internal/launch ./internal/engine/eino ./internal/budget ./internal/runtime ./internal/configschema ./internal/desktopconfig ./internal/acp/agent -timeout 5m
go test -p 2 . -run '^TestSDK(DurableBudgetActualToolAndRestart|NotificationContinuationUsesOriginalBudgetAndIndependentAck|NotificationContinuationExhaustedOriginalBudgetRollsBackAdmission)$' -timeout 2m -count=1
```

新增覆盖默认累计消耗超过 200,000、显式预算继续生效、预算提醒不改写输入、单任务重复压缩、并行工具组完整性、不同模型窗口的动态阈值、大窗口不使用小窗口兜底阈值、较大新请求不被无效摘要拦截，以及 YAML/CLI 优先级。五个 Go 可执行程序构建成功，实际用户配置经原生配置工具验证。

全量 `go test -p 2 ./... -timeout 5m` 尚未通过。SDK 根包仍有以下失败，单独运行可复现：

- `TestSDKNotificationContinuationSourceTamperBlocksResume` 的错误类型断言失败，模型调用数保持不变。
- `TestDesktopMemoryPolicyScopesInjectionAndTool` 的文本断言匹配到了记忆说明中的 `workspace` 字样。
- `TestSDKCommandPermissionPrecedesProcessAndReceipt/nonzero` 仍要求命令失败产生顶层运行错误。
- 全量 SDK 根包测试超过五分钟时限，在技能行为测试阶段终止。

本次运行链路测试中，另同步修正三处已与当前实现不一致的测试前提：合法的 `auto_patch` 枚举、记忆配置字段数量，以及容纳代理指令与工具 schema 所需的测试窗口大小。

## 当前运行包

当前桌面使用 `DeerFlow-Desktop-Go-20261004-GlobalMemory`。已更新该包的五个 Go 程序、默认配置及用户配置，并重启 Go daemon，桌面进程保持运行。原配置、程序、离线状态与实际运行源码保存在该包 `user-data/backups/token-policy-20261005-192330`。升级说明和程序哈希见运行包根目录 `TOKEN_POLICY_UPDATE.json`，该更新明确标记为本地工作树构建。
