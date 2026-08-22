<div align="center">

# dsh-tiny-desktop

**DeepSeek Harness 的轻量原生 Windows 桌面壳（约 7.5 MB）** —— WebView2 原生窗口 + 系统托盘，Go 编写。

[English README](README.md) · [MIT License](LICENSE)

</div>

---

## ✨ 这是什么？

`dsh-tiny-desktop` 把 [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness)（`dsh web` 命令）包装成原生 Windows 桌面应用：

- **极致轻量**：单个 ~7.5 MB 可执行文件——没有 Electron（动辄 100–250 MB）、无需安装器、无外部运行库
- **原生窗口**：基于 WebView2，不用管理浏览器标签页
- **系统托盘驻留**：鲸鱼图标常驻托盘；关闭窗口服务继续运行
- **桌面通知**：WebView2 页面里的 `Notification` API 自动桥接为**白色卡片通知浮窗**（右下角，圆角、浅灰描边，ClearType 清晰文字；带按钮与悬停态，点击聚焦窗口），dsh-notification 等插件开箱即用——无需浏览器式权限弹窗，也不依赖系统通知路由（Win11 已不再渲染传统托盘气泡），普通 GDI 窗口实现，远程桌面/虚拟显示环境同样可见；在真浏览器中打开时则使用浏览器原生通知
- **授权即弹窗**：DSH 的**授权请求**（工具执行需审批）会自动弹窗并带「允许一次 / 拒绝」按钮，直接在浮窗上应答，无需切回窗口；走的是客户端运行时同款应答通道（`approval/requested` → `PendingWait.respond`）
- **内核随心更新**：智能兼容全局 npm / pnpm / npx 缓存，启动自动选用最高版本；托盘右键支持一键「**检查并更新 DSH**」
- **内存友好**：关闭窗口即销毁 WebView2（释放约 500 MB），再次打开按需重建
- **页面缩放**：Ctrl + 滚轮 / Ctrl + `+` `-`（0.5x–2x），Ctrl+`0` 复位
- **单实例**：重复双击快捷方式只会聚焦已有窗口
- **DPI 感知**：任意系统缩放下都清晰锐利

## 🚀 快速开始（普通用户）

**环境要求**：Windows 10/11 x64 · WebView2 运行时（Win11 自带 / 随 Edge 更新）· Node.js 18+ 且在 `PATH` 中（用于运行 `dsh`；找不到时自动回退 `npx`）。

1. 从 [Releases](../../releases) 下载最新的 `dsh-tiny-desktop.zip`
2. 解压到任意目录，双击 **`DSH Tray.exe`**
3. 首次启动会静默拉起最新版 `dsh web` 服务并打开原生窗口；之后鲸鱼图标常驻系统托盘

> 💡 **小技巧**：换机器部署、或想按自己的方式配置——直接把本仓库链接丢给 DeepSeek Harness / Claude / Codex 等任何 AI 编程产品，让 AI 读这份 README 帮你完成部署，是最快的上手方式。

## 🔨 从源码构建

**环境要求**：Go 1.21+ · WebView2 运行时 · Node.js 18+

```powershell
git clone https://github.com/Ichikawashadow/dsh-tiny-desktop
cd dsh-tiny-desktop
./build.ps1          # 一键构建 → DSH Tray.exe
```

或手动执行：

```powershell
go run github.com/akavel/rsrc@latest -ico assets/dsh-v9.ico -o rsrc.syso
go build -ldflags "-H windowsgui" -o "DSH Tray.exe" .
```

就这样——**`DSH Tray.exe` 就是完整应用**（图标已内嵌，零外部文件）。

## 🖱️ 使用说明

| 操作 | 效果 |
|---|---|
| 双击 `DSH Tray.exe` / 快捷方式 | 托盘图标 + 原生窗口打开 |
| 托盘右键菜单 | **打开窗口** · **在浏览器中打开** · **重启服务** · **检查并更新 DSH** · **退出（停止服务）** |
| 关闭窗口 | 服务在托盘继续运行；再次打开自动重建窗口 |
| Ctrl + 滚轮 / Ctrl + `+` `-` | 页面缩放（0.5x–2x）；Ctrl+`0` 复位 |
| 再次双击快捷方式 | 聚焦已有窗口（单实例） |

## 🔄 更新机制

- **自动寻找最新版**：程序启动时会自动扫描全局 npm 目录、pnpm 目录以及 npx 缓存（`%LOCALAPPDATA%\npm-cache\_npx\...`），智能选择语义化版本最高的 `@deepseek-ai/dsh` 执行；
- **托盘一键手动更新**：右键托盘图标点击「**检查并更新 DSH**」，程序将自动连接远端镜像检查官方最新版本、下载更新、清理 Windows 死锁并热重启服务，全程均有桌面卡片通知提醒。

## ⚙️ 配置与日志

- 端口：`3080`（修改 `main.go` 中 `port` 常量后重新编译）
- 日志：`%USERPROFILE%\.dsh\logs\dsh-web.log`（服务输出 + 托盘事件）

## 🧩 架构

```
Go 单文件程序
├── getlantern/systray        — 系统托盘图标与菜单
├── jchv/go-webview2          — WebView2 原生窗口（纯 Go，无 CGO）
├── npx @deepseek-ai/dsh      — 真正的 Harness 网关（隐藏窗口运行）
└── win32 互操作              — DPI 感知、单例互斥、窗口图标
```

## 🌱 生态定位

dsh-tiny-desktop 与 DSH 插件体系**互补而非冲突**：

- **本壳**：独立桌面客户端（Go 单文件），负责 DSH 服务的桌面体验——托盘驻留、原生窗口、服务生命周期管理，不修改你的 DSH profile；
- **DSH 插件**：运行在 DSH Web GUI 内部的 cordis 插件（皮肤、统计、工具集成等），通过 [`dsh-plugin`](https://github.com/topics/dsh-plugin) topic 或插件市场安装。

两者互不依赖：不用本壳也可以照常使用 DSH 及其插件；本壳也可以与任意插件组合使用。

## ⚠️ 免责声明

本项目是**独立社区项目**，与 DeepSeek 官方无关、未获其背书。DeepSeek Harness 本身由 DeepSeek AI 开发，采用 MIT 协议。

## 📄 License

[MIT](LICENSE) — Copyright (c) 2026 dsh-tiny-desktop contributors
