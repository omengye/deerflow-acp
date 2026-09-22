# DeerFlow Desktop 首次设置加载优化

设置页读取配置时会启动 `python -m deerflow.config_tool`。此前只读取技能元数据和内置子 Agent 定义，也会经由包的 `__init__.py` 导入技能安装器、模型 SDK、LangChain/LangGraph 和子 Agent 执行器。首次创建配置需要先执行 `init`，再执行 `snapshot`，重复承担这部分进程启动成本。

现在技能安装接口和子 Agent 执行/注册接口按需导入。设置读取仍使用原来的 YAML 解析、技能扫描和配置结构；实际安装技能时仍加载原安装器与安全扫描，实际执行 Agent 时仍加载原执行器。公开接口名称、类型和异常保持兼容。

本次仅修改两个 Python 包入口，不需要重新编译原生程序。累计补丁包含上一版的桌面程序、daemon 和会话管理修复，保留单次确认发布、移除诊断与恢复、保存自动应用及会话删除修复。

## 性能测量

在同一台 Windows 机器、同一便携运行时和默认资源上，各测量 3 轮，每轮使用全新的隔离配置目录。每次操作均启动新 Python 进程，计时包含进程启动和退出。没有清空操作系统文件缓存，结果不代表所有机器的磁盘冷启动时间，也不包含原生界面绘制时间。

| 配置加载路径 | 修改前中位耗时 | 修改后中位耗时 |
| --- | ---: | ---: |
| 首次初始化 + 读取（init + snapshot） | 14.66 秒 | 4.16 秒 |
| 已有配置读取（snapshot） | 7.28 秒 | 1.76 秒 |
| 再次读取（新进程） | 7.46 秒 | 2.04 秒 |

首次配置加载减少约 72%。开发环境的单次 `-X importtime` 分析也确认配置模块累计导入耗时从 37.98 秒降至 0.92 秒；该数据只用于定位导入链，不与便携包耗时混用。

可复现命令：

```powershell
.venv/Scripts/python.exe scripts/benchmark-deerflow-settings.py --package <解压后的完整便携包目录> --runs 3
```

脚本仅在仓库 `.build-cache/settings-benchmarks` 中创建测试配置，不读写指定包内的用户配置。修改前报告为 `run-x0386fn2/timings.json`，修改后报告为 `run-t2hxtds_/timings.json`。

## 回归验证

新增独立进程测试，禁止 `init` / `snapshot` 导入模型 SDK、安装器或 Agent 执行器，防止重依赖重新进入首次加载路径。同时验证初始技能与子 Agent 元数据、公开接口兼容性、保存后的重新读取、版本冲突及损坏配置的错误返回。测试通过导入隔离检查性能边界，不以容易受机器负载影响的秒数作为单元测试断言。

85 项 Python 回归通过，覆盖配置读写、技能安装安全检查、技能自进化、子 Agent 设置与工具、会话管理及提案发布控制。

完整便携包隔离 smoke 测试通过。真实 Waku daemon 的首次 `snapshot` 请求（含首次配置创建）耗时 4.98 秒，随后的 `snapshot` 请求耗时 1.15 秒。验证了无效保存不启动服务、保存并自动应用、应用后继续聊天保留历史、会话删除及重启后的持久化；退出无进程清理错误。报告位于 `.build-cache/desktop-smoke/20260919-080828-d27e07e3/logs/report.json`。测试只使用隔离用户目录和本机模拟模型服务。

```powershell
.venv/Scripts/python.exe -m pytest tests/test_config_tool_startup.py tests/test_config_tool.py tests/test_skills_validation.py tests/test_subagent_generation_settings.py tests/test_subagent_deferred_tools.py tests/test_portable_management.py tests/test_acp_proposal_control.py tests/test_skill_installer_security.py tests/test_skill_evolution.py -q
.venv/Scripts/python.exe scripts/test-deerflow-desktop-smoke.py --package <解压后的完整便携包目录>
```

本次未执行视觉测试；测量与回归验证针对配置读取及桌面通信链路。

## 更新方法

补丁适用于 2026-09-18 的完整 Windows x64 便携版，也可覆盖此前的设置修复版。先结束任务、停止 ACP 服务并退出桌面，再将补丁按目录结构覆盖到便携版目录。保留 `user-data`。需要覆盖补丁内的 `runtime` 子目录，仅替换 EXE 不会得到加载优化。

补丁不是独立安装包。对应完整源码包附带此前修复及本次性能测试脚本。
