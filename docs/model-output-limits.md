# 模型单次输出额度

上下文窗口、单次输出额度和任务总预算是三个不同的限制。部分推理模型将推理 tokens 计入单次输出；因此即使上下文还很充足，也可能在生成正文前耗尽输出额度。

每个 `models` 条目支持：

- `max_output_tokens`：固定的请求额度，`0` 或未配置时继承宿主限制。
- `max_supported_output_tokens`：当前模型及接口声明的输出上限，`0` 或未配置表示未知。应依据实际接口的文档或元数据填写，不能用上下文窗口替代。

`local_acp.max_output_tokens` 仍是所有会话模型的宿主硬限额，默认 `4096`；`0` 关闭这一层限制。显式模型请求额度超过已声明的服务上限或非零宿主硬限额时，保存和启动均报错。继承的额度会受已声明服务上限约束。

示例仅适用于已核实相应服务能力的模型：

```yaml
local_acp:
  max_output_tokens: 16384
models:
  - name: reasoning-model
    provider: openai
    model: provider/model-id
    max_output_tokens: 16384
    max_supported_output_tokens: 64000
  - name: other-model
    provider: openai
    model: provider/other-id
    max_output_tokens: 4096
    max_supported_output_tokens: 0
```

实际请求还会受子代理额度、调用方更小的额度、上下文剩余空间和任务剩余预算约束。主代理、子代理、上下文压缩及会话后处理共用这一约束；预算预留记录与发往服务的额度一致。任务恢复保留已保存的预算，修改配置不会扩大旧任务预算。独立的技能维护及连接探测继续使用各自的请求预算。

在桌面设置的“模型”中填写这两个字段，保存并重启后生效。服务上限只是能力声明，不会主动提高请求额度；未知上限不会被猜测。服务仍可能施加账户、网关或版本相关的更低限制。

当服务返回 `length` / `max_tokens`：

- 没有正文、也没有工具调用：立即停止同条件重试，并显示服务报告的输出及推理用量（如有）。
- 已有正文：保留部分结果并进行最多四次续写；重复相同内容时提前停止。
- 截断的工具调用：整组拒绝执行，只允许有限次数地重新生成完整调用；不会重放此前完成的工具。

运行中不自动增加额度，不自动降低推理强度。输出受限通过 ACP 的 `limit: output_tokens` 标识，任务总预算通过 `limit: tokens` 标识；真正的上下文超限沿用 `context_limit` 错误。未知 `max_tokens` 停止原因仅显示 Token 限制，不能推断为上下文耗尽。
