# Changelog

## v1.0.2

- 修复：启动 dsh 服务时浏览器标签页被同时打开（dsh `web` 命令默认会自动打开默认浏览器，现改为传入 `--no-open`，仅保留 WebView2 原生窗口）
- 新增：CHANGELOG.md

## v1.0.1

- 修复：窗口标题栏 / 任务栏左上角图标未生效（正确解析 ICO 图像条目后应用鲸鱼图标）

## v1.0.0

- 新增：在 DSH 对话中点击网页链接时，用 Microsoft Edge 打开（不再交给系统默认浏览器）
- 初始发布：dsh-tiny-desktop —— 一个约 7.2 MB 的原生 Windows 桌面外壳（WebView2 + 系统托盘，Go 编写）