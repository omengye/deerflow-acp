# Windows Release ACP 启动失败诊断

诊断日期：2026-10-07。目标发行目录：`dist/desktop/DeerFlow-Desktop-Go-20261007-Release-081250`。

## 已确认的启动失败原因

当前 `user-data/config/config.yaml` 声明了以下目录，两个目录实际均不存在：

```yaml
skills:
  managed_sources:
    - id: managed-evolution
      root: ../skills/evolved
      scope: global
  export_root: ../skills-exports
```

这些路径相对于 `user-data/config` 解析，因此要求发行目录下存在 `user-data/skills/evolved` 和 `user-data/skills-exports`。

`launch.ApplyConfig` 首先调用 `skills.ResolveManagedSources`。后者要求受管理源已经存在。`skills.openSource` 调用 `noSymlinkPath`，遇到不存在的目录后，将原始 `os.ErrNotExist` 统一包装为 `harness.ErrPermissionDenied`。因此日志中的 `permission_denied` 在这次故障中实际由目录缺失触发，不能据此判断需要管理员权限。

配置服务的 `service.init` 只创建通用目录、旧 Skills 导入目录并复制初始技能，没有为当前配置创建受管理源和导出目录。配置快照使用 `configschema.ProjectSkillSources`，只投影字段，不检查目录，所以 `snapshot` 能成功且 `skill_source_diagnostics` 为空，而 daemon 的配置加载失败。

## 现场证据和隔离验证

- 09:08:52 初次启动到达 `startup.ready`，09:10:42 关闭。
- 09:10:42、09:11:57 两次后续启动均在 `startup.configuration` 阶段记录 `permission_denied` 并退出。09:11:57 这次从启动到失败约 117 毫秒。
- 对现场配置进行只读 `ApplyConfig` 诊断，得到 `skills.managed_sources.0: permission denied: skills source root contains a link or inaccessible component`。
- `os.Lstat` 确认上述两个目录均为 `not_exists=true`、`permission=false`。
- 在忽略的构建缓存中，用脱敏配置及隔离目录验证：两个目录均缺失时复现原错误；只创建受管理源时转为 `skills.export_root` 错误；两者都存在时完整配置加载成功，共 3 个技能源、19 个安装项。没有调用模型或打开现场数据库。
- 核对了配置加载、桌面服务适配器和界面状态代码，它们与此次发行的冻结源码字节一致。

## 启动中和配置只读的原因及验证范围

Bridge 的 `wait_for_status` 默认等待 120 秒，仅轮询 endpoint，没有检查刚启动的 daemon 是否已经退出。Windows detached 启动也没有保留其 stderr。因此 daemon 已在配置阶段失败，桌面仍会等待 endpoint，最后只能收到泛化的超时错误。

桌面服务在该等待期间保持 `applying=true`；界面 `DeerFlowSettings.can_edit` 要求 `!applying()`，因此字段和重新读取操作处于禁用状态。这解释了约两分钟的“ACP 启动中”及编辑锁定。

将发行包中的实际 Waku daemon、Bridge、Go daemon、配置程序复制到隔离目录，使用不含真实凭据的最小配置复现了相同的目录缺失错误。仅对该隔离子进程将 Bridge 启动超时缩短为 5 秒，得到：

- `start` 在 724 毫秒返回，`applying=true`、`running=false`。
- 等待期间 `snapshot` 可在 307 毫秒返回，配置服务和 RPC 没有被启动线程永久阻塞。
- 超时后 `applying=false`、`running=false`，并记录 `endpoint has not been published` 的启动超时错误。
- 超时后 `snapshot` 在 128 毫秒返回。

验证记录：`.build-cache/acp-startup-probe-6f4f1b0f4caf4c5e940b2c07cd4d579e/REPORT.json`。此验证覆盖发行二进制的后端恢复行为，没有操作用户窗口。超过三分钟仍保持启动中及只读的前端状态问题，尚未在实际窗口复现，不能当作已经确认的永久死锁。

## 最小恢复方式

补齐配置明确声明的两个空目录，然后退出并重新打开桌面程序：

```powershell
$releaseRoot = 'D:\Tools\deerflow-api\dist\desktop\DeerFlow-Desktop-Go-20261007-Release-081250'
New-Item -ItemType Directory -Force -Path (
    Join-Path $releaseRoot 'user-data\skills\evolved'
), (
    Join-Path $releaseRoot 'user-data\skills-exports'
) | Out-Null
```

本次诊断未执行上述恢复命令，也未覆盖发行二进制、重置配置、删除数据或停止用户程序。配置 SHA-256 在诊断前后保持 `5424abc66bae8ecbc6ba83aa9257699286613872c2882755ae189aeaa40ab1bd`。

## 发布级修复建议

1. 在配置初始化/迁移流程中安全初始化产品管理的可写源和导出目录；对自定义路径保留明确校验，不能随意创建任意位置或放宽链接检查。
2. 区分目录不存在、链接被拒绝和真实权限错误，并在可脱敏的错误中标明配置字段。
3. Bridge 保留子进程状态和安全的启动失败信息，配置加载失败时及时返回，避免等待完整 120 秒。
4. 启动失败时让配置修复入口保持可用，错误状态优先显示，并补上失败后编辑恢复的界面验证。
5. 增加“已有配置引用的受管理源/导出目录未迁入发行目录”的验收场景。此前成功配置的协议 smoke 无法覆盖本次异常路径。
