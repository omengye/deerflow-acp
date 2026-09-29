# 后台通知进入父模型的实施设计

状态：待实施。基于已接入的 native background host、共享预算和前台 durable HITL。

## 目标与入口

父 prompt 经常在后台任务完成前已经结束。每条通知需要独立的 continuation
run/input，复用前台执行、TurnLoop、HITL 与恢复路径，并继续使用该任务原预算。

先提供显式 SDK `ProcessBackgroundNotification(ctx, sessionID, notificationID,
emit, approve)` 和协商扩展 `_deerflow/notifications/process`。ACP 只接受
`sessionId`、`notificationId`，进入与 prompt、execution resume 相同的同步准入流程。
后续自动调度调用同一个准入接口。

处理通知不代表批准工具。没有当前会话持有人时返回未附着；不能将历史
`SubmittedBy` 当作当前连接身份。已有前台 run 或 waiting execution 时不准入，
不向父 native history 追加内容，也不消费通知。

## 持久身份与事务

新增不可变来源表 `harness_continuation_sources`，至少保存：

- 新 `run_id`、新 `input_id`，分别唯一。
- 唯一 `notification_id`，固定父 session 与 task。
- `origin_run_id`、`origin_input_id`、原 `root_budget_id`。
- 通知快照、权威 task 结果快照、配置/扩展版本及 `source_sha`。

原 run/input 不改写，不重用其一对一身份。新增可信 input source 标记，并将
`SourceSHA` 固定到 execution、TurnLoop item 和 execution manifest；每次首次执行、
checkpoint 恢复与公开 resume 都重新验证。来源缺失或摘要不匹配时拒绝执行，
不能退回普通 prompt 路径。

私有 `BeginContinuationTx` 在单个事务内：

1. 验证 inbox、原 task Binding、父 session、origin run/input 和当前配置。
2. 若通知已绑定 continuation，返回其原 execution；不再次运行模型。
3. 固定通知和权威任务结果快照，计算来源摘要。
4. 插入新 run/input/source、预算成员和 attempt、现有 execution 状态。
5. 追加 `continuation_started` 领域事件。

使用 `BindMemberTx` 接入原 root，**不调用 `CreateRootTx`**。原预算耗尽、unknown
或未确认的副作用仍阻止准入。不同预算 root 的通知不合并，也不放入新用户 prompt
来获得新额度。不能直接调用现有 `Service.Run`，其默认行为会创建新 root 和
`user_message`。

## 模型输入和权限

runtime 通过可信 source hook 提供通知内容，engine 投影为标明来源的消息。
provider 需要时可采用 user role，但领域历史必须保留通知来源。不得重发原 prompt，
也不得伪造原 `background_agent` 的第二条 tool result。

沿用来源任务的配置和扩展约束；配置已变化时先报告冲突并保留通知，不静默采用
更宽权限。旧 `allow_once` 不转移，新工具仍创建新回执、intent 与 grant。

显式 process 没有 PermissionHandler 且策略为 ask 时，保留 durable waiting。
当前前台默认 nil handler 会 reject_once，因此 process 必须安装返回
`PermissionCancelled` 的缺省 handler，让已提交的 waiting checkpoint 留待
`executions/resume`。已配置 allow/reject/read_only 策略继续先行决策。

## 三种确认状态

| 状态 | 含义 | 不应触发的行为 |
| --- | --- | --- |
| native outbox ACK | 通知已可靠写入 inbox | 不启动父模型 |
| UI ACK | 用户将通知标记为已读 | 不授权工具，不阻止按 ID 处理通知 |
| model delivery | 通知已原子绑定 durable continuation | 不修改 UI `handled` |

通知 model delivery 通过 source/execution 关联查询。恢复使用已经固定的快照，
不依赖通知是否已在 UI 确认。旧 waiting_input 通知需要同时记录通知版本和当前
task 版本，不能将过期通知当成当前审批请求。

重启后，未准入通知可以处理；waiting continuation 使用现有审批恢复；已完成的
重复 process 返回原 execution；结果不确定的执行走既有对账，不能重建 run 绕过。

## 文件分工与验收

| 工作 | 主要文件 |
| --- | --- |
| 来源表、准入事务、幂等与快照 | 新 `internal/runtime/continuations.go`；background inbox 的事务读取辅助 |
| 来源与 manifest/resume 校验 | runtime executions、execution_service |
| 模型输入投影 | interaction、engine turn_loop/engine |
| SDK、ACP 入口与同步准入 | 新 host/SDK continuation 文件；harness 可选接口；ACP admit/dispatch |

必须覆盖父 prompt 已结束、与新用户 prompt 的并发准入、原预算计费、跨 root 拒绝、
重复 process、UI ACK 独立、来源篡改、进程重启、断连后的审批恢复，以及模型/工具
故障后不会重新创建 root。使用本地模型 fixture；真实编辑器验收另行记录。
