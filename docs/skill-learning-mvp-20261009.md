# 技能自进化 MVP 交付说明

交付日期为 2026-10-09。按最新用户要求，本次收口到技能自进化 MVP，不继续重复测试、修复低收益边界或扩展桌面与真实模型评测范围。

## 1. 交付结论

自动主链已经接通并有定向运行证据。

```text
普通真人多轮任务
  -> 持久化终态 outbox
  -> 空闲后自动提取有界脱敏经验
  -> 语义归并、识别后续纠正
  -> 匹配当前授权技能
  -> 自动生成 instruction-only SKILL.md 候选
  -> 人工批准 / 满足独立授权与评测的受限自动发布
  -> 新任务读取新版，保留版本记录、观察与回滚
```

用户不需要手工编写经验、整理技能、选择目标、拆分 episode 或调用生成接口。人工模式的人工操作仅为审核系统已生成的提案。初始配置与共享授权属于使用授权，不属于手工进化。

本次交付支持已有技能改进，并可在另行授权后自动生成新技能待审提案。自动发布限于已有启用 managed 技能的小型、验证改善的指令补丁；新技能、首次接管导入技能、脚本/支持文件和其他高风险修改仍需人工批准。它不代表模型权重训练，也不是无边界自治发布。

## 2. MVP 验收依据

| 要求 | 当前实现与证据 | 结果 |
| --- | --- | --- |
| 普通多轮会话自动总结 | runtime 在真实 accepted input 与终态事务捕获；`learningStore.Start` 自动生产，复用共享维护 dispatcher | 已完成 |
| 不需要手工进化 | SDK 原生链只提交普通真人输入，无手工经验、目标裁决、episode 或生成操作，自动到 proposal | 已完成 |
| 早期未验证、后续纠正 | 新 resolution policy 保留原经验与来源，真实较晚执行覆盖原引用后自动解除阻塞，不增加独立票 | 已完成 |
| 人工审核 | 单任务自动待审；认证 MANAGE 获取 diff 并 approve；启用 auto_patch 也不能自动采用 review draft | 已完成 |
| 自动审核/采用 | 两个真实独立授权任务、启用 managed head、可信 baseline/candidate 改善与零回归、单独发布 grant 后自动发布 | 已完成，受限范围 |
| 新能力候选 | 完整授权目录判断缺能力、固定非内置来源和可读名称，只生成 SKILL.md，始终人工采用 | 已完成，独立开关 |
| 发布后使用 | 关闭学习后重启，真实新 Eino run 读取已采用版本；原导入文件不覆盖，维护阶段不重跑 | 已完成 |
| 基础安全与成本 | 默认关闭、学习/生成共享预算与有限重试、脱敏、生成凭据阻断、贡献/共享撤销及完整发布守卫 | 已完成，非绝对安全承诺 |
| 可追溯与恢复 | immutable revision/job digest，来源检查、已保存 checkpoint 复用、原观察/回滚能力 | 已完成 |
| 测试范围不扩大 | 仅 self-evolution / 直接受影响 regression / Eino 本机 loopback / 正式设置链，无全包、race 或外部付费模型 | 已遵守；现停止新增测试 |

最近一次核心 SDK 运行中，原多轮链与新增“第一轮未验证、第二轮纠正”链均通过。新增两分支分别验证人工批准与已有技能受限自动采用，保留 2/4 个原经验、1/2 条 resolution link、1/2 个独立观察；12/17 条维护调用全部结算，无伪造 operator feedback。正式设置保存与启动的已有技能、新能力两个顶层用例最终完整精确选择器也通过。

具体测试名称、运行命令、早期失败及修复记录见 [实施记录](skill-learning-status-20261008.md)。这些是确定性接线与权限/事务证据，不是模型语义质量验收。

## 3. 启用方式

配置默认保持关闭，本次未替用户开启付费能力，也未修改用户有效设置、个人数据库或已采用技能。以下是在现有配置中的合并片段，不是完整配置替换；模型引用、技能来源和预算仍使用当前项目已授权配置。保存后重启宿主使 startup-only 配置生效。

### 推荐的人工审核模式

```yaml
skill_evolution:
  enabled: true
  mode: review
  generation:
    enabled: true
  learning:
    enabled: true
    consolidation_enabled: true
    matching_enabled: true
  discovery:
    experience_patterns_enabled: true
    experience_review_drafts_enabled: true
```

保留原正数 `skill_evolution.budget`、learning 子预算和具体模型配置。`null` / 空模型名称继承配置的默认模型。generation 必须有已授权、可写的 skill source，不能只打开开关却没有合法目标来源。默认空闲窗口为 300 秒；主对话不等待学习。全局 learning opt-in 后普通前台会话默认贡献，可通过逐会话 control 关闭，敏感会话应关闭贡献。

启用后，一个有完整本地观察证据的普通多轮任务即可自动生成待审提案。从原提案界面或认证 MANAGE 的 `proposal.list` / `proposal.get` 查看 diff 和版本，使用 `proposal.approve` / `proposal.reject` 审核，不需要手工生成。

### 已有技能受限自动模式

```yaml
skill_evolution:
  mode: auto_patch
  learning:
    cross_session_enabled: true
  discovery:
    experience_review_drafts_enabled: false
  auto_patch:
    experience_patterns_enabled: true
  behavior:
    enabled: true
    suite_path: path/to/your-trusted-skill-suite.json
```

此片段叠加在前述基础模式上；suite_path 必须替换为真实、与目标匹配的宿主可信套件，不是可直接使用的占位文件。正文修改需要 targeted baseline 失败到 candidate 通过及 regression 零回归；description 修改另外需要正负 metadata-only trigger suite。导入技能需要先合法人工采用成为 managed head，不能借自动模式越过首次接管。

每个跨会话来源还需明确 `workspace_shared=true`；只共享目录不等于共享对话授权。不同初始真人任务可自动形成独立会话来源，不需人为切 episode。默认独立门槛为 2。若保留单任务草案开关，草案始终人工，且该草案已消费证据不会因后来任务自动升级或退消费；推荐 MVP 在两种模式中择一，不混淆预期。

### 新技能待审

另行开启 `discovery.experience_new_skills_enabled` 并设置真实的、generation-authorized 非内置 `new_skill_source_id`。新能力发现自动命名、生成和审查，人工只批准。即使其他已有技能使用自动模式，新技能也不会自动发布。

## 4. 后续事项与已知限制

这些项目保留在后续 backlog，不作为当前 MVP 收口阻塞，也不在本次继续循环测试修复。

| 项目 | 当前边界 / 后续事项 |
| --- | --- |
| 真实模型质量 | 未用代表性标注任务验证纠正识别、归并和匹配准确率。固定 loopback 不能证明模型会总能生成正确技能；先试点人工审核 |
| 可信套件代表性 | 自动发布只证明当前宿主套件；未做真实留出样本或质量指标验收。不要将局部改善宣传为普遍泛化 |
| 桌面完整验收 | 设置、状态、diff、来源和审核控制已接源码与隔离逻辑检查；未做桌面构建、视觉与真实 daemon 往返验收 |
| 任务边界 / 复制 | 不自动语义拆分长会话。精确初始输入与显式 lineage 保守去重；未声明的改写复制不保证识别，同源复制不会增加独立票 |
| 长历史与证据不足 | 每批最多 8 真人 turn、总计 8 delegated/continuation run。原输入不在后续固定引用中、截断或无真实本地验证时继续 unresolved，不扩大上下文或补造证明 |
| 临时失败与恢复 | readiness 等待与原 job 最多 3 attempts / 原 deadline。未提交输出可能再次计费；超时、预算耗尽、永久来源变化不自动无限重做 |
| 已生成候选 | head/revision/source 改变不自动 rebase。新技能 lifetime 预留不退款；人工重试是已有可选管理能力，不是正常进化前提 |
| 隐私与凭据 | 有界脱敏和模式扫描不是绝对防泄漏；高敏感会话应不贡献。网页/MCP/未知工具仅 hints，不作为写入指令授权 |
| 发布后观察 | 不把自动推断当质量票。无确认反馈时保持 insufficient_evidence；来源撤销保留已采用资产并标记 withdrawn，不制造失败或擅自删技能 |
| 低收益边界 | 直接 Registry enable/disable 与 publication history 不一致时会保守拒绝自动观察；暂不放宽。严格定时 fixture 在高 I/O 并行负载下曾有一次两轮 view 失配，单独分支及随后完整选择器通过，未放宽生产规则 |

以后需要桌面发布或真实模型质量评估时，应另定预算、样本与验收范围，不把它们混入本次 MVP。

## 5. 相关落点

- [设计与三家参考](skill-learning-design-20261008.md)
- [详细实施及测试记录](skill-learning-status-20261008.md)
- [配置示例](../config.example.yaml)
- 自动学习主逻辑在 `go-harness/internal/learning`，startup wiring 在 `go-harness/deerflow.go`。
- 候选发现、生成、审核与发布复用 `go-harness/internal/skills/evolution`，模型适配复用 `go-harness/internal/engine/eino`。

MVP 交付口径为“普通多轮会话自动沉淀与生成技能候选，人工只审核；独立授权及可信评测通过后，可受限自动采用已有技能补丁”。剩余质量、桌面和边界项已经明确列出，不宣称全面自治进化。
