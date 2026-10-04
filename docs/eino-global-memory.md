# Eino Harness 全局记忆

实施日期：2026-10-04。此分支使用 Go 与 Eino，无 Python 运行依赖。

## 读取和写入范围

| 范围 | 共享边界 | 自动提取 |
| --- | --- | --- |
| global | 同一份 Waku user-data，跨项目、会话、模型 | 不自动写入；显式创建、导入或提升 |
| workspace | 当前项目目录下的会话 | memory_scope 为 workspace/global 时写入 |
| session | 当前会话 | memory_scope 为 session 时写入 |
| user（兼容旧接口） | 旧宿主用户身份＋项目 | 保留原有 SDK 行为，不等同于 global |

默认读取全局＋项目。当前用户要求优先于记忆；描述性事实冲突时依次优先使用会话、项目、全局。记忆不能授予权限或修改工具策略。

全局 preference/profile 可以在问题关键词未命中时进入有界自动注入；显式 search_memory 仍要求查询匹配。现有自动注入 4096 字节预算、每范围最多 6 个检索结果及管理列表 1000 行/3 MiB 上限保留。工具模式只在显式检索时读取。已固定的执行快照不会因管理员修改记忆而在运行中变化。

## 配置

```yaml
local_acp:
  memory_scope: workspace
memory:
  enabled: true
  include_global: true
  mode: middleware
  injection_enabled: true
  retrieval_enabled: true
```

include_global 默认 true；关闭后模型只读取选定的本地范围，管理员仍可管理全局。memory_scope 的 global 值表示读取全局＋项目、自动提取写项目；启用记忆时与 include_global:false 冲突会明确拒绝。memory.enabled:false 停止模型访问及提取，仍保留事实。自动提取不允许模型选择 global。

全局身份保存在 harness.db 的 memory_profile 单例表，创建后不随连接 owner、模型、安装目录或项目改变。复制整份便携 user-data 会保留身份和事实；另一独立数据库会生成独立身份。没有云端同步，也不会合并不同便携包的数据库。

旧 session/workspace/user key 原样保留，新 global 使用独立命名空间。开启全局读取不会把旧事实搬入全局。升级时记忆执行策略指纹更新，旧策略固定的中断运行须结束后重新发起，避免以变化的范围继续旧执行。

## 管理入口

Waku 高级设置的记忆页提供全局、项目、会话切换。全局不依赖历史会话；项目和会话通过选中的历史会话定位。非全局事实可点击“提升为全局…”，确认后复制原内容，保留原事实。源版本在用户确认后发生变化时拒绝操作并要求刷新；同类别且内容忽略首尾空白及 SQLite lower 后相同的事实不会重复创建。全局删除只影响后续读取，原项目副本保留。

本地管理支持 memory.get/delete/promote；提升必须传 source scope、fact_id、expected_revision，不能由客户端选择全局身份。修改时暂停新运行准入，并拒绝在已有操作执行中修改。

ACP 支持 scope:"global" 的已有 CRUD/search/list，以及 `_deerflow/memory/promote`。提升参数为 sessionId、scope（源范围）、factId、expectedRevision。所有 ACP 操作仍要求拥有有效附着会话；客户端不能指定 profileId。SDK 对应 `PromoteMemoryFact`。deerflow-memory-migrate 的 --scope global 是显式导入入口，无模型调用。

提升事务原子校验源版本、查重及写入，保留 operator 来源、源 fact ID 和源版本以供追溯。

## 验证

仅运行受影响的定向用例，覆盖跨项目共享、本地隔离、提升 CAS 与查重、重启身份与事实保留、全局偏好注入/检索/关闭读取、ACP owner 和身份拒绝、配置保存→Go 启动，以及自动提取不得写全局。Rust 用例检查无会话的全局请求、源范围、审查版本及禁止自提升。未进行视觉自动化或真实模型质量验收。

本地结果：8 个 Go 定向用例通过，Rust 记忆管理请求用例 1/1 通过；git diff --check 通过。Release 构建和交付包检查在对应 BUILD_SOURCE.json、SHA256 及外部构建日志中记录。
