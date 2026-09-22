# DeerFlow Desktop 实施记录

本版直接以本机 Waku 源码为基础，保留 GPUI 会话工作台、任务列表、会话数据库、附件和文件面板。新增的 DeerFlow 设置使用同一套 Theme、TextInput、TextField、开关和按钮，不嵌入旧 Iced 设置窗口。

## 源码与运行结构

- `desktop-app/`：Waku 派生桌面及内部 daemon，来源和 GPL 许可见 `desktop-app/FORK.md`。
- `desktop-app/src/app/deerflow_settings.rs`：DeerFlow 原生设置表单、草稿、错误反馈和管理操作。
- `desktop-app/crates/waku-core/src/deerflow_config.rs`：通过现有 Python `deerflow.config_tool` 和 ACP Bridge 管理配置与执行服务。
- `scripts/build-deerflow-desktop.ps1`：Windows x64 便携打包，生成程序 ZIP 和对应源码 ZIP。
- `resources/desktop-default-config.yaml`：桌面专用默认配置，新安装默认保留会话历史。

执行链为桌面 → 内部 Waku daemon → DeerFlow ACP stdio Bridge → DeerFlow Python 服务。对话数据库位于 `user-data/desktop`，模型配置位于 `user-data/config/config.yaml`，运行数据、技能、日志和 ACP 状态使用同级 `user-data` 子目录。

## 本轮实现

设置入口为 DeerFlow、通用、外观。DeerFlow 内包括概览、模型、智能体、长期记忆、技能、工具与权限、ACP 运行、诊断与恢复。支持校验、保存、手动模型连接测试、应用配置、服务状态及启停、备份恢复、记忆管理和技能提案审查。

配置保存与应用分开。保存仅写配置；应用先停止接收新任务，等当前任务结束，再重启执行服务。可以取消等待。设置草稿在页面切换后保留，退出支持保存、放弃或继续编辑。已有密钥不回填明文，输入字段遮罩，空白保持原密钥。

ACP 接入保留 DeerFlow 自身的计划模式，支持模型与思考开关，显式传递选中的图片附件，并保留计划进度与产物链接。恢复失败会报错，不创建空上下文替代原会话。连接关闭后不接受静默丢弃的消息；后续发送可通过持久化 session id 恢复。

便携包与本机 Waku、旧 ACP 包数据隔离；旧包和用户配置不会被覆盖。上游 Waku 自动更新及遥测禁用。发布包使用新的输出目录，单独提供对应源码和许可证。

## 首次使用

1. 将便携包完整解压到可写目录，运行 `deerflow-desktop.exe`。保留同目录的 `runtime`、`resources` 和其他程序文件；首次配置和后续数据使用包内 `user-data`，无需另装 Python。
2. 点击左下角齿轮打开“设置”，进入“DeerFlow → 模型”。可以修改预置 OpenAI 条目，或点击“添加模型”。预置条目提供填写示例，请配置自己的模型服务和凭据。
3. 填写“配置名称”“模型 ID”“API 地址”和“API Key”。配置名称是本地引用名，模型 ID 是服务商提供的标识。OpenAI 兼容接口可使用 `langchain_openai:ChatOpenAI` 适配器；思考、思考强度和图片输入开关应按服务商实际能力填写。“显示名称”和“模型说明”可选。API Key 留空会保留已保存的值，只有开启“清除已保存的密钥”才会移除它。
4. 点击“设为默认”。可先用“测试连接”发送一条真实的简短模型请求，再点击“校验”和“保存”。保存成功后点击“应用配置”，等待服务启动或应用完成。已有任务运行时会先等待任务结束；“取消等待”取消应用过程，已保存的配置仍然保留。
5. 返回聊天，打开项目文件夹，或选择“不使用项目”，然后新建任务。在输入区选择模型，输入需求并发送。工具审批策略在“DeerFlow 配置 → 工具与权限”管理；DeerFlow 会话不显示 Waku 的 Ask / Full Access 切换。

“ACP 运行”可调整默认模型、默认智能体、计划与思考开关、并发数和超时。“长期记忆”管理本地 DeerMem；“技能”可开关技能、查看自进化提案并审查发布。“诊断与恢复”提供诊断、备份预览以及会话和记忆数据管理。复杂参数位于各页的“展开高级设置”中。

修改先保留在草稿中，切换设置分类或返回聊天不会丢失。模型与智能体改名在“确认名称”或保存时统一更新引用；输入过程不会修改其他条目的引用。“重新加载”遇到未保存草稿会先要求确认放弃。退出时可选择保存并退出、放弃并退出或继续编辑；保存失败会保留窗口和草稿。保存不会自动应用，应用也不会代替保存。

## 边界

- DeerFlow ACP 当前没有真实的分叉及对话回退协议能力，桌面和后端均禁用这两个入口。
- 退出本地桌面会关闭其内部 daemon，并取消相应连接上的执行任务。Python 服务常驻不代表任务能在退出桌面后继续。
- 便携版配置面向本地执行环境和 DeerMem；已有非本地配置须明确确认转换后再保存。
- Agent Profile/Subagent 的动态会话选项、历史导入后的图片卡片重建未在本轮扩展。
- `DEER_FLOW_PORTABLE_ROOT` 指向另一套完整便携目录，不是只存数据的目录。

## 验证

- 桌面、内部 daemon、ACP Bridge 的 Windows x64 Release 构建成功；Python wheel 离线构建成功。
- Python 配置、便携管理、ACP daemon/stdio、产物和提案控制测试 45 项通过。
- GPUI 设置回归 10 项、密码输入 2 项、ACP 核心与连接生命周期 16 项、协议 72 项通过，共 145 项测试。
- TypeScript 协议绑定已生成，`export_types --check` 通过；脚本语法及便携清单检查通过。

- 独立便携目录的完整集成测试通过，使用本地模拟 OpenAI 服务，无真实云端模型调用。覆盖无密钥配置校验/保存、服务启停、ACP 创建/列出/加载/关闭会话、流式回复、配置应用及重连。第二轮保留同一原生会话 ID，并在模型请求中实际包含首轮助手回复。停止后主动进行模型探测，服务仍保持停止。测试进程清理无错误。
- 原生 GUI 检查通过：会话首页、DeerFlow 概览、模型表单和通用设置共用 Waku 主题；显示名称可编辑，跨设置页草稿保留，标题栏关闭触发退出确认，“保存并退出”成功写入测试配置并关闭窗口。已确认没有遗留本次测试包的桌面/daemon/Python 进程。
- 发布 ZIP 全量 CRC 校验通过，三个 EXE 与本轮构建的 SHA-256 一致。包内含 Python、默认配置、22 个技能和许可证，无 `user-data`、私有配置或凭据。

可复现命令：`.venv/Scripts/python.exe scripts/test-deerflow-desktop-smoke.py --package dist/desktop/DeerFlow-Desktop-20260918`。本次报告保存在 `.build-cache/desktop-smoke/20260918-070746-f1d68346/logs/report.json`，后续运行会创建新的独立目录。

便携程序：`dist/desktop/DeerFlow-Desktop-20260918/deerflow-desktop.exe`。发布包：`dist/desktop/DeerFlow-Desktop-20260918-windows-x64.zip`；匹配源码包：`dist/desktop/DeerFlow-Desktop-20260918-source.zip`。
