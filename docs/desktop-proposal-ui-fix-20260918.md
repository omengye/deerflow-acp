# DeerFlow Desktop 技能提案审查界面修复

2026-09-18 修复了技能自进化审查中长差异内容覆盖“批准并发布…”和“拒绝提案…”按钮的问题。设置页面继续使用 Waku 的 GPUI 主题与控件样式。

## 原因与修改

`desktop-app/src/app/deerflow_settings.rs` 原先仅给差异容器指定 300 像素高度，没有限制子控件的溢出。多行 `TextInput` 按全部文本高度渲染，文字和鼠标命中区域越过容器边界，遮挡下面的操作按钮。

修复为独立的固定高度滚动区域，同时裁剪文字和命中区域；批准、拒绝按钮保持在区域外，并允许窄窗口下换行。差异容器使用提案 ID 区分滚动状态，切换提案后从开头查看。点击批准或拒绝后，设置页自动滚到确认区域。

相同的边界修复也应用于设置中的多行文本和 JSON 输入框。没有修改提案发布、拒绝的后端逻辑。

## 验证

Windows x64 Release 桌面程序构建通过，输出为 `.build-cache/desktop-build/release/waku.exe`，打包时改名为 `deerflow-desktop.exe`。构建命令在 `desktop-app` 目录运行：

```powershell
$env:CARGO_TARGET_DIR = 'D:\Tools\deerflow-api\.build-cache\desktop-build'
cargo build --offline --locked --release -p waku --bin waku
```

在隔离 smoke 运行目录中，通过真实 `FileEvolutionStore` / `SkillProposal` 接口创建两条合成的 `pending_review` 提案，每份差异 112 行，并通过本地 ACP 管理接口确认可读取。对本次构建的 Release 程序完成了以下 GUI 检查：

- 长差异保持在固定边框内，按钮行完整显示在框外。
- 在差异区域内滚动，可看到 Step 088–100 和末尾 `END OF FIXTURE`，后半段内容没有被截掉。
- 点击批准测试提案的“批准并发布…”，立即显示包含正确提案 ID 的批准确认；随后取消。
- 切换到拒绝测试提案后，差异滚动位置回到开头。
- 点击“拒绝提案…”，立即显示包含正确提案 ID 的拒绝确认。

检查只打开确认界面，没有批准、拒绝或改动真实提案。测试模型地址固定为本机回环地址，关闭自动技能生成与 discovery，没有发送云模型请求。

## 安装补丁

`DeerFlow-Desktop-20260918-ui-fix.zip` 是现有 2026-09-18 便携版的桌面 EXE 补丁，不能单独运行。

1. 结束当前任务并关闭 DeerFlow Desktop。
2. 备份原目录的 `deerflow-desktop.exe`。
3. 将补丁包中的同名 EXE 复制到原目录替换，然后重新启动。

只替换桌面 EXE，保留原来的 `runtime`、`resources`、`waku-daemon.exe`、`deerflow-acp.exe` 和全部 `user-data`。恢复旧版时，关闭程序后还原备份的 EXE。

完整对应源码为 `DeerFlow-Desktop-20260918-ui-fix-source.zip`。它保留原完整源码包的其他文件，更新本次修改的设置源文件并加入本文档。桌面派生代码继续采用 GPL-3.0-only，补丁附带 Waku 许可证与 fork 说明。
