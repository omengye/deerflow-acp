# 单用户持久记忆

本次升级把记忆的事实库、检索索引和模型上下文分开。SQLite 中的当前事实与版本是权威数据；向量可重建，模型的“记住了”文本不算保存凭据。

## 行为与边界

- 同一个服务实例拥有一个稳定的个人资料 ID；个人资料跨会话、跨工作区共享。项目事实仍归工作区，FoloToy 的表达风格单独保存为渠道配置。
- 明确要求记住、更正、忘记时，模型使用 `remember_memory` / `forget_memory`。事实与操作回执在同一事务提交，成功后才确认。
- 每个属性有固定键和独立版本。更正使用 CAS；旧提取任务不能覆盖较新的更正；删除留下墓碑，后台提取不能复活记录。
- 后台任务只读取已接受的用户输入，在模型开始前固定属性版本，前台回合成功后才执行。队列、尝试次数与提取结果持久化，进程重启可继续。最多三次尝试，每天最多 200 次自动提取尝试。
- 核心个人资料有独立的上下文预算，其他资料通过词法与语义融合检索。索引不可用时保留词法检索和核心资料注入。来源与时间随事实保留；地点表示最近告知的位置，不表示实时定位。
- 模型提取结果必须提供用户原文证据，最多四条；拒绝凭据和渠道风格污染。子代理不借用父会话的写入工具权限。
- 管理接口：`memory.personal.get`、`memory.personal.mutate`、`memory.personal.receipt`，通过现有 Waku `deerFlow/manage-envelope` 调用。
- Android 的待确认操作使用 Keystore 加密保存。断网或回执丢失后先查回执，再以同一操作 ID 重试；未确认前不显示保存成功。

## 部署

使用 Go 1.26.8 编译 `deerflow-acpd-go`、`deerflow-config-go`、`deerflow-personal-migrate`，放入 `bin/`。基底是既有 `deerflow-server:mobile-waku-v10-20261026`，保留 Waku、工具运行时及已有包装脚本。

运行 `download_model.py`，以 `MEMORY_MODEL_DIRECTORY` 指定构建目录中的 `model/`。下载固定提交的中文 BGE ONNX 模型，所有文件均校验 SHA256。下载也可在联网构建机完成后上传；镜像运行时无需访问模型仓库。

分别用 `Dockerfile.server` 和 `Dockerfile.embedding` 构建镜像。向量服务在 `deerflow-deploy_default` 内网监听 8080，不映射宿主机端口。公网继续使用现有 Caddy/WSS 路径。

`deploy.py` 只用于本文指定的 Gentoo 部署路径：

1. 在服务器内生成经过核实的 `migration-plan.json`，每项绑定原事实 ID、版本和内容 SHA256；不导出原始记忆。
2. `python3 -B deploy.py stage`：复制数据库和技能到隔离目录，离线迁移，再启动仅绑定宿主机回环地址 18788 的测试实例。
3. `python3 -B acceptance.py --stage`：通过真实 Waku 和模型检查回执重放、冲突、更换工作区后的回忆、遗忘及自动提取。临时事实在退出前遗忘。
4. 运行 `internal/memory` 的 `TestPersonalMemoryChineseEmbeddingAcceptance`，设置 `MEMORY_EMBEDDING_TEST_URL=http://memory-embedding:8080`。使用合成的 100 条中文查询验收召回率与时延，不使用个人资料作为评估集。
5. `python3 -B deploy.py apply`：检查空闲、排空新任务、备份配置和数据库、迁移、切换镜像、确认健康。
6. `python3 -B acceptance.py`：从公网域名验证同一协议；完成后确认原有客户端与渠道健康。

回滚命令是 `python3 -B deploy.py rollback`。它只恢复程序和配置，保留当前业务数据库，避免丢失升级后新增的数据。数据库备份是恢复资产，不属于待清理临时文件。

配置新增 `memory.single_user`、`memory.embedding_url` 和 `memory.embedding_model`。原生作用域字段为 `local_acp.memory_scope`。前述开关默认不改变其他安装，部署时明确启用。

桌面端若选择本地服务，其资料库与 Gentoo 相互独立；升级本地后端不会自动更换连接方式或搬迁本地历史。连接同一 Gentoo 服务的客户端才共享同一个个人资料库。

## 维护

保留代码、测试报告、镜像和回滚备份。验收结束移除隔离容器、隔离数据副本、构建上下文、临时测试二进制和传输文件。不要清理生产卷、技能、用户工作区或其他任务的缓存。

向量模型依据 [BGE 官方说明](https://huggingface.co/BAAI/bge-small-zh-v1.5) 使用；检索参数以本仓库合成语料验证，分数不是概率。上线后的实际召回质量仍需随用户资料类型变化复核。
