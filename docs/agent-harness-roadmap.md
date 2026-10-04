> 历史记录：本文描述旧 Python 实现或当时的桌面设计，命令和能力声明不作为当前分支的使用说明。原文与源码参见[固定历史提交](https://github.com/omengye/deerflow-acp/tree/9c29fcaac991f2d9f1e7af68250b12f6b3ec07d9)。当前 Eino Go 入口与验收范围见[项目说明](../README.md)。

# DeerFlow API — Agent Harness 能力演进蓝图

> **生成日期**: 2026-05-07
>
> **分析依据**: 3 份深度研究报告（行业标准 + 官方文档）+ 完整代码库审计
>
> **状态**: Draft — 待评审

---

## 一、现状总览

DeerFlow-API 是基于 LangChain + LangGraph 构建的**生产级 agent harness**，通过 FastAPI + SSE 将 Agent 能力暴露为 REST API。

### 已有能力矩阵 (24 项)

| 维度 | 能力 | 实现状态 |
|------|------|---------|
| **核心框架** | LangGraph state machine, 多模型支持, SSE 流式 | ✅ 完整 |
| **工具生态** | 动态注册, MCP 集成, 延迟加载, ACP 调用 | ✅ 完整 |
| **子 Agent** | task tool, general/bash 类型, 并发执行, 取消 | ✅ 完整 |
| **持久化** | SQLite checkpointer, 线程管理, 状态快照 | ✅ 完整 |
| **记忆系统** | 事实存储, 注入式记忆, 摘要钩子, 防抖更新 | ✅ 完整 |
| **技能系统** | 加载/安装/验证/安全扫描/.skill 档案 | ✅ 领先 |
| **安全治理** | Sandbox provider, Guardrail middleware, 循环检测 | ✅ 完整 |
| **可观测性** | LangSmith, Langfuse, token tracking | ✅ 完整 |
| **中间件** | 声明式特性标志, `@Next/@Prev` 定位, 18+ 中间件 | ✅ 创新 |
| **文件处理** | 上传/下载, PDF/Excel/PPT→Markdown 转换 | ✅ 完整 |

### 行业领先领域

- **Skill 生命周期**: 比 Anthropic Skills 更完善（安装/验证/安全扫描/热重载）
- **声明式架构**: `RuntimeFeatures` + `@Next/@Prev` 优于硬编码模式
- **循环防护**: 双重保护 (3x 警告 + 5x 停止) + 频率限制，行业仅单重
- **多模型回退**: LLM 错误自动 fallback，仅少数框架支持
- **工具去重**: 自动处理重复工具名冲突 (issue #1803)

---

## 二、能力差距矩阵

### 🔴 P0 — 关键缺失（影响生产可靠性）

| # | 能力 | 行业标准 | 当前状态 | 差距描述 | 建议方案 |
|---|------|---------|---------|---------|---------|
| 1 | **Human-in-the-Loop (完整)** | 5 种 HITL 模式 (checkpoint 中断/工具审批/escalation/structured feedback/debate) | 仅有 `ask_clarification` | 缺少 checkpoint 级别中断、工具执行前审批、escalation 路由 | 利用 LangGraph `interrupt()` 实现审批 gate；在 `GuardrailMiddleware` 中集成 HITL |
| 2 | **Evaluation 框架** | OTel-based eval, pairwise LLM-as-judge, golden dataset, CI/CD gating | 仅基础 pytest | 无 golden dataset 评估、无回归检测、无 LLM-as-judge、无质量门禁 | 集成 `agentevals` 或 LangSmith evaluators；添加 pairwise 对比评估 |
| 3 | **Planner/Generator/Evaluator 分离** | 三 agent 模式消除自评分偏见 (Anthropic/Claude Code 推荐) | 单 agent 自评 | subagent 同时承担规划和执行，无独立评估环节，产生系统性乐观偏差 | 新增 `evaluator` 子 agent 类型；实现 `Planner→Generator→Evaluator` 管道 |
| 4 | **Context Compaction Pipeline** | 7+ 恢复路径, circuit breaker, 自动上下文重组 | 基础摘要钩子 | 无 token budgeting、无上下文溢出恢复、无分层压缩策略 | 实现 recovery ladder（按成本排序的 7+ 恢复路径）；添加 circuit breaker 防无限循环 |

### 🟡 P1 — 重要增强（提升竞争力）

| # | 能力 | 行业标准 | 当前状态 | 差距描述 | 建议方案 |
|---|------|---------|---------|---------|---------|
| 5 | **多 Agent 编排模式扩展** | 7 种模式 (Handoff/Swarm/Crew/Supervisor/Graph/Subgraph/Conversation) | 仅 task 工具 (2 种) | 缺少 Handoff-first 路由、Swarm 动态交接、Supervisor 监督模式 | 实现 `handoff` 工具（参考 OpenAI SDK agents-as-tools）；添加 Supervisor 路由中间件 |
| 6 | **治理与策略引擎** | RBAC, token budgets, model allowlists, 审计日志 (Orloj YAML 模式) | 基础 guardrail | 无 per-agent 策略、无 token 预算控制、无成本上限、无审计日志 | 实现 declarative policy engine；支持 YAML 策略定义 |
| 7 | **OpenTelemetry 集成** | CNCF 标准, GenAI semantic conventions (LLM/Tool/Retrieval/Agent spans) | LangSmith/Langfuse 私有协议 | 无法接入 Datadog/Grafana/Jaeger 等通用 APM | 添加 OTel SDK 集成，导出 GenAI spans |
| 8 | **Cost Attribution** | per-agent/workflow/tool 成本分解 | 仅 token 追踪 | 无法按线程/agent/工具维度统计成本 | 扩展 `token_usage` middleware，添加 cost 维度标注 |
| 9 | **A2A Protocol 支持** | Google-led 多框架互通标准 (Agent Card/Task/Message) | ❌ 无 | 无法与其他框架的 agent 直接通信 | 评估集成 Google A2A SDK，实现 agent card 注册 |
| 10 | **Durable Execution 增强** | 工作流级持久化 (Agentspan/Orloj), 自动重试, 死信队列 | 后台任务管理 | 任务失败无自动重试、无死信处理、无 idempotency 保证 | 集成 `Restate` 或实现 retry/dead-letter 机制 |
| 11 | **Agentic RAG** | Corrective/Self/Adaptive RAG (多跳准确率 42%→94.5%) | ❌ 无 | 无检索质量评估、无 query 重写、无自适应路由 | 实现 Corrective RAG 管道（retrieve→grade→rewrite） |

### 🟢 P2 — 长期演进（锦上添花）

| # | 能力 | 描述 |
|---|------|------|
| 12 | **实时语音 Agent** | OpenAI realtime agent, Gemini Live API 集成 |
| 13 | **MCP Elicitation** | MCP 2025-11 规范的用户交互原语（表单/确认） |
| 14 | **MCP Sampling** | LLM callback 能力，让 MCP server 主动调用 LLM |
| 15 | **Prompt 版本管理** | prompt versioning, A/B 测试, 回滚机制 |
| 16 | **知识图谱集成** | GraphRAG, Neo4j 集成，结构化知识存储 |
| 17 | **Web 浏览能力** | Playwright/browser automation 集成 |
| 18 | **Debate 协议** | 多 agent 辩论达成共识，投票/quorum 机制 |
| 19 | **Enterprise SSO** | OAuth 2.1 / SAML 企业级认证 |
| 20 | **MCP Apps** | MCP 2026-01 新增的交互式 UI 组件返回能力 |

---

## 三、实施路线图

### Phase 1 — P0 关键能力（1-2 周）

> **目标**: 补齐生产可靠性短板，达到 OpenAI Agents SDK / Google ADK 同等水平

#### 1.1 Human-in-the-Loop 完整实现

```
优先级: 🔴 最高
工作量: 3-4 天
依赖: LangGraph interrupt() API

实施步骤:
  1. 在 GuardrailMiddleware 中实现工具审批 gate
     - deny 时不直接返回错误，而是 interrupt() 等待人工审批
     - 支持 Command(resume=human_decision) 继续执行
  2. 扩展 ask_clarification 为 HITL 入口
     - 支持 checkpoint 级别中断（pause at specific node）
     - 添加 escalation 路由（agent 识别不确定性时自动升级）
  3. 实现 structured feedback
     - 用户可纠正中间输出
     - 提供指导性反馈而非简单问答
  4. API 端点扩展
     - POST /api/threads/{id}/approve   — 审批待执行工具
     - POST /api/threads/{id}/resume    — 携带用户输入恢复
     - GET  /api/threads/{id}/pending   — 查询待审批项

验收标准:
  ✅ 工具执行前可暂停等待审批
  ✅ 审批后可继续执行，携带人工决策
  ✅ agent 可主动 escalation 到人类
  ✅ 前端可展示待审批项并交互
```

#### 1.2 Evaluation 框架

```
优先级: 🔴 最高
工作量: 3-4 天
依赖: 无

实施步骤:
  1. 集成 LLM-as-judge evaluator
     - 实现 pairwise 对比评估（比单评分更可靠）
     - 支持 faithfulness checking（输出与检索内容的一致性）
  2. 建立 golden dataset
     - 定义测试用例集（输入 + 期望输出/评分标准）
     - 支持 offline evaluation（不重新执行，基于 trace）
  3. CI/CD gating
     - 评估分数低于阈值时阻塞部署
     - 回归检测（snapshot diff）
  4. 评估 API
     - POST /api/evaluate/batch  — 批量评估
     - GET  /api/evaluate/report — 获取评估报告

验收标准:
  ✅ 支持 pairwise LLM-as-judge
  ✅ golden dataset 评估流水线
  ✅ CI/CD 集成分数门禁
```

#### 1.3 Planner/Generator/Evaluator 分离

```
优先级: 🔴 高
工作量: 2-3 天
依赖: 子 agent 基础设施

实施步骤:
  1. 新增 evaluator 子 agent 类型
     - 系统 prompt 专注于评估而非执行
     - 接收 planner 的规范和 generator 的产出
     - 输出结构化评估报告（pass/fail + 理由）
  2. 实现三阶段管道
     Planner → Generator → Evaluator
     - Planner: 将用户请求分解为详细规格
     - Generator: 按规格执行
     - Evaluator: 验证结果是否符合规格
  3. 失败回退
     - Evaluator 不通过时，反馈给 Generator 重新执行
     - 最多重试 N 次后 escalation

验收标准:
  ✅ evaluator 子 agent 可独立运行
  ✅ 三阶段管道可配置启用
  ✅ 自评分偏见消除（对比实验验证）
```

#### 1.4 Context Compaction Pipeline

```
优先级: 🔴 高
工作量: 3-4 天
依赖: 记忆系统

实施步骤:
  1. 实现 recovery ladder（7+ 恢复路径，按成本排序）
     R0: 移除最旧的非关键消息
     R1: 压缩中间 tool 结果（保留摘要）
     R2: 合并连续同类消息
     R3: 文件系统化（大观察写入文件，仅保留引用）
     R4: LLM 摘要（对旧对话进行语义压缩）
     R5: 移除整个对话段（保留关键事实到 memory）
     R6: 重置上下文（从 memory 重建）
  2. 添加 circuit breaker
     - 监控 token 使用率
     - 超过 80% 阈值时主动触发 compaction
     - 超过 95% 时强制 R6
  3. Token budgeting
     - per-agent token 预算
     - 实时跟踪和预警

验收标准:
  ✅ 上下文溢出时自动恢复
  ✅ circuit breaker 防止无限循环
  ✅ token 使用率可监控
```

---

### Phase 2 — P1 竞争力提升（2-4 周）

#### 2.1 多 Agent 编排模式扩展

```
优先级: 🟡 高
工作量: 1 周
依赖: Phase 1.3 (evaluator)

新增模式:
  ├── Handoff: agents-as-tools（参考 OpenAI SDK）
  │   └── 专用 agent 处理完后自动交还控制权
  ├── Supervisor: 监督者路由
  │   └── 根据任务类型动态分配子 agent
  └── Swarm: 动态交接
      └── agent 间基于上下文自动交接

API 扩展:
  POST /api/agents/{id}/handoff — 手动交接
  GET  /api/agents/available    — 查询可用 agent 列表
```

#### 2.2 治理与策略引擎

```
优先级: 🟡 高
工作量: 1 周

策略定义 (YAML):
  policies:
    - name: "conservative"
      max_tokens_per_turn: 10000
      allowed_models: ["claude-sonnet", "gpt-4o"]
      tool_allowlist: ["read_file", "search"]
      require_approval: ["write_file", "bash"]
      max_cost_per_thread: 5.00  # USD

执行层:
  PolicyMiddleware 拦截工具调用
  检查 token budget, model allowlist, tool permissions
  违规时 deny + 审计日志
```

#### 2.3 OpenTelemetry 集成

```
优先级: 🟡 中
工作量: 3-5 天

集成:
  ├── OTel SDK 安装
  ├── GenAI semantic conventions
  │   ├── gen_ai.client.request
  │   ├── gen_ai.client.response
  │   ├── gen_ai.tool.call
  │   └── gen_ai.agent.execution
  ├── Exporters
  │   ├── OTLP (Datadog/Grafana/Jaeger)
  │   └── 保留 LangSmith/Langfuse 作为可选
  └── 结构化日志增强
      ├── Operational: 系统事件
      ├── Cognitive: agent 决策
      └── Contextual: 上下文变更
```

#### 2.4 Cost Attribution

```
优先级: 🟡 中
工作量: 2-3 天

扩展 token_usage middleware:
  ├── 按 thread 维度统计
  ├── 按 agent 维度统计 (parent/subagent/evaluator)
  ├── 按工具维度统计
  └── 成本换算 (model × token count × price)

API:
  GET /api/threads/{id}/usage  — 线程用量
  GET /api/usage/summary       — 全局汇总
```

#### 2.5 Agentic RAG

```
优先级: 🟡 中
工作量: 1 周

实现 Corrective RAG:
  Retrieve → Grade → Sufficient?
    ├─ Yes → Generate answer
    └─ No → Rewrite query → Retrieve again

组件:
  ├── Retriever: 多源检索 (web + local + MCP)
  ├── Grader: LLM 评估检索质量 (relevant/irrelevant)
  ├── Query Rewriter: 基于失败原因重写查询
  └── Router: 根据查询类型选择检索策略

集成点:
  作为可选 skill 或 MCP server 接入
```

---

### Phase 3 — P2 长期演进（1-2 月）

| 优先级 | 能力 | 预估工作量 | 依赖 |
|--------|------|-----------|------|
| 3.1 | A2A Protocol 支持 | 1-2 周 | Phase 2.1 |
| 3.2 | Durable Execution 增强 | 1 周 | Phase 1.4 |
| 3.3 | MCP Elicitation/Sampling | 3-5 天 | MCP 协议更新 |
| 3.4 | Prompt 版本管理 | 1 周 | 无 |
| 3.5 | Web 浏览 (Playwright) | 1 周 | sandbox 增强 |
| 3.6 | 实时语音 Agent | 2 周 | 模型支持 |
| 3.7 | Debate 协议 | 1-2 周 | Phase 2.1 |
| 3.8 | Enterprise SSO | 1 周 | 无 |
| 3.9 | MCP Apps | 3-5 天 | MCP 协议更新 |

---

## 四、架构设计图

### 当前架构

```
┌─────────────────────────────────────────────────────────┐
│                      FastAPI (app/)                      │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌───────────┐  │
│  │  chat    │ │ threads  │ │  skills  │ │    mcp    │  │
│  │  router  │ │  router  │ │  router  │ │   router  │  │
│  └────┬─────┘ └────┬─────┘ └────┬─────┘ └─────┬─────┘  │
│       └─────────────┴────────────┴─────────────┘        │
│                         │                               │
│                  DeerFlowClient                         │
└─────────────────────────┬───────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────┐
│              Agent Harness (deerflow/)                    │
│                                                         │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────────┐  │
│  │ Lead Agent  │  │ Subagents    │  │   Memory      │  │
│  │ (LangGraph) │──│ (task tool)  │  │  (facts)      │  │
│  └──────┬──────┘  └──────┬───────┘  └───────┬───────┘  │
│         │                │                   │         │
│  ┌──────▼────────────────▼───────────────────▼──────┐  │
│  │              Middleware Chain                     │  │
│  │  Guardrail → LoopDetect → Summarize → ToolError  │  │
│  └──────────────────────┬───────────────────────────┘  │
│                         │                               │
│  ┌──────────────────────▼───────────────────────────┐  │
│  │              Tools Layer                          │  │
│  │  Builtins │ MCP │ Skills │ Sandbox │ ACP │ Search│  │
│  └──────────────────────────────────────────────────┘  │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐                    │
│  │   Tracing    │  │  Checkpoint  │                    │
│  │ LangSmith+   │  │   (SQLite)   │                    │
│  │  Langfuse    │  │              │                    │
│  └──────────────┘  └──────────────┘                    │
└─────────────────────────────────────────────────────────┘
```

### Phase 1 完成后的目标架构

```
┌─────────────────────────────────────────────────────────┐
│                      FastAPI (app/)                      │
│  ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌──────┐ │
│  │ chat │ │threads│ │skills│ │ mcp  │ │ eval │ │ HITL │ │
│  └──┬───┘ └──┬───┘ └──┬───┘ └──┬───┘ └──┬───┘ └──┬───┘ │
│     └─────────┴────────┴────────┴────────┴────────┘     │
└─────────────────────────┬───────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────┐
│              Agent Harness (deerflow/)                    │
│                                                         │
│  ┌─────────────┐  ┌──────────────┐  ┌───────────────┐  │
│  │ Lead Agent  │  │ Subagents    │  │   Memory      │  │
│  │ (LangGraph) │──│ Planner      │  │  (facts)      │  │
│  │  + HITL     │  │ Generator    │  │               │  │
│  │  + Evals    │  │ Evaluator ◄──┼──┘               │  │
│  └──────┬──────┘  └──────┬───────┘  └───────┬───────┘  │
│         │                │                   │         │
│  ┌──────▼────────────────▼───────────────────▼──────┐  │
│  │           Enhanced Middleware Chain               │  │
│  │  Guardrail/HITL → LoopDetect → Compaction →      │  │
│  │  Summarize → ToolError → CircuitBreaker          │  │
│  └──────────────────────┬───────────────────────────┘  │
│                         │                               │
│  ┌──────────────────────▼───────────────────────────┐  │
│  │              Tools Layer                          │  │
│  │  Builtins │ MCP │ Skills │ Sandbox │ ACP │ Search│  │
│  └───────────────────────────────────────────────────┘  │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │   Tracing    │  │  Checkpoint  │  │  Evaluation  │  │
│  │ LangSmith+   │  │   (SQLite)   │  │ LLM-as-Judge │  │
│  │  Langfuse    │  │   + HITL     │  │ + Golden Set │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
└─────────────────────────────────────────────────────────┘
```

### Phase 2 完成后的目标架构（新增模块）

```
                    ┌──────────────┐
                    │   Policy     │
                    │   Engine     │
                    │  (YAML/RBAC) │
                    └──────┬───────┘
                           │
┌──────────────┐  ┌────────▼────────┐  ┌──────────────┐
│   OTel       │  │ Cost Attribution│  │  Agentic     │
│  Exporters   │  │ (thread/agent/  │  │    RAG       │
│ (Datadog/    │  │  tool level)    │  │ (retrieve →  │
│  Grafana)    │  │                 │  │  grade →     │
└──────────────┘  └─────────────────┘  │  rewrite)    │
                                        └──────────────┘
                                              │
┌──────────────┐  ┌──────────────┐  ┌────────▼──────┐
│  Multi-Agent │  │  Supervisor  │  │  Handoff &    │
│  Patterns    │──│   Router     │──│    Swarm      │
│ (Handoff/    │  │              │  │               │
│  Supervisor  │  │              │  │               │
│  Swarm)      │  │              │  │               │
└──────────────┘  └──────────────┘  └───────────────┘
```

---

## 五、技术选型建议

| 需求 | 推荐方案 | 备选方案 | 理由 |
|------|---------|---------|------|
| HITL 实现 | LangGraph `interrupt()` | 自定义 pause/resume | 原生支持，与 checkpointer 无缝集成 |
| Evaluation | LangSmith evaluators | agentevals (OTel-based) | LangSmith 已有集成，迁移成本低 |
| Policy Engine | 自定义 YAML 驱动 | Orloj (重量级) | 轻量级，与现有 guardrail 兼容 |
| OTel 集成 | opentelemetry-sdk + opentelemetry-instrumentation-langchain | 仅 LangSmith | CNCF 标准，供应商无关 |
| Agentic RAG | LangChain CorrectiveRAGChain | 自研 | 成熟实现，与现有 tools 层集成 |
| A2A Protocol | google-adk a2a-python-sdk | 自研实现 | Google 官方维护，标准制定者 |
| Durable Execution | Restate Python SDK | 自研 retry 机制 | 生产验证，自动重试 + 死信 |

---

## 六、风险与约束

### 技术风险

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| LangGraph `interrupt()` 与现有 SSE 流式冲突 | HITL 中断可能导致流式连接挂起 | 测试验证，必要时实现 SSE heartbeat |
| LLM-as-judge 评估成本高 | 每次评估消耗额外 token | 采样评估 + 缓存结果 |
| A2A Protocol 标准仍在演进 | 实现可能因标准变化需重构 | 先封装抽象层，延迟具体实现 |
| OTel 集成与现有 LangSmith/Langfuse 共存 | 重复数据导出增加开销 | 可配置开关，按需启用 |

### 外部约束

| 约束 | 影响 | 应对 |
|------|------|------|
| 依赖 MCP 协议版本 | Elicitation/Sampling 需 MCP server 端支持 | 跟随 MCP 规范更新节奏 |
| 模型 API 限制 | Context compaction 依赖模型 summarization 能力 | 多模型 fallback 策略 |
| 团队规模 | P0+P1 需 6-10 周，可能需分阶段交付 | 优先 P0 中影响最大的 2 项 |

---

## 七、成功度量 (KPI)

| 指标 | 当前值 | Phase 1 目标 | Phase 2 目标 |
|------|--------|-------------|-------------|
| HITL 覆盖率 | 1/5 模式 | 4/5 模式 | 5/5 模式 |
| 评估覆盖率 | 0% | 核心场景 60% | 全场景 90% |
| 自评分偏差 | 存在 | 消除 (PGE 分离) | — |
| 上下文溢出恢复 | 无 | 自动 (7 路径) | — |
| OTel 集成 | ❌ | — | ✅ 完整 |
| 多 Agent 模式 | 2 种 | — | 5+ 种 |
| 成本可追溯 | 仅 token | — | per-thread/agent/tool |
| Agentic RAG 准确率 | N/A | — | 多跳 >80% |

---

## 八、决策记录

| 日期 | 决策 | 理由 | 状态 |
|------|------|------|------|
| 2026-05-07 | 选择 LangGraph `interrupt()` 实现 HITL | 原生支持，最小侵入 | 待定 |
| 2026-05-07 | Evaluation 优先 LLM-as-judge pairwise | 比单评分可靠，行业共识 | 待定 |
| 2026-05-07 | 三阶段管道 (Planner/Generator/Evaluator) | 消除自评分偏见 | 待定 |
| 2026-05-07 | OTel 与 LangSmith/Langfuse 共存 | 渐进迁移，不中断现有流程 | 待定 |
| 2026-05-07 | A2A 延迟到 Phase 3 | 标准仍在演进，非核心需求 | 待定 |

---

## 附录 A：关键参考文档

| 来源 | 链接 | 用途 |
|------|------|------|
| LangGraph HITL | https://docs.langchain.com/oss/python/langgraph/human-in-the-loop | P0.1 实现参考 |
| LangSmith Evaluators | https://docs.smith.langchain.com/evaluation | P0.2 实现参考 |
| Anthropic Agent Best Practices | https://www.anthropic.com/engineering/writing-tools-for-agents | P0.3 设计参考 |
| OpenAI Agents SDK | https://developers.openai.com/api/docs/guides/agents | P1.1 模式参考 |
| Google ADK | https://cloud.google.com/agent-builder/agent-development-kit/overview | P1.5 A2A 参考 |
| Orloj Governance | https://github.com/orloj/orloj | P1.2 策略引擎参考 |
| OpenTelemetry GenAI | https://opentelemetry.io/docs/concepts/signals/ | P1.3 规范参考 |
| IMPACT Framework | https://www.morphllm.com/agent-engineering | 整体架构参考 |

---

## 附录 B：术语表

| 术语 | 含义 |
|------|------|
| HITL | Human-in-the-Loop，人工介入机制 |
| A2A | Agent-to-Agent Protocol，Google 主导的多 agent 通信标准 |
| MCP | Model Context Protocol，Anthropic 主导的工具集成标准 |
| OTel | OpenTelemetry，CNCF 可观测性标准 |
| PGE | Planner/Generator/Evaluator 三阶段管道 |
| RAG | Retrieval Augmented Generation |
| RBAC | Role-Based Access Control |
| Circuit Breaker | 熔断器模式，防止系统过载 |
