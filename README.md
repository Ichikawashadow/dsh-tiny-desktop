<div align="center">

# dsh-tiny-desktop

**A tiny (~7.5 MB) native Windows desktop shell for DeepSeek Harness** — WebView2 window + system tray, powered by Go.

[中文版 README](README.zh-CN.md) · [MIT License](LICENSE)

</div>

---

## ✨ What is it?

`dsh-tiny-desktop` wraps [DeepSeek Harness](https://github.com/deepseek-ai/deepseek-harness) (the `dsh web` CLI) into a native Windows desktop experience:

- **Tiny**: a single ~7.5 MB executable — no Electron (100–250 MB), no installer, no external runtime files
- **Native window**: WebView2-based, no browser tabs to manage
- **System tray**: whale icon resident in the tray; close the window and the service keeps running
- **Always fresh**: automatically uses the **latest** `@deepseek-ai/dsh` from your npx cache — no manual updates
- **Memory-friendly**: closing the window destroys WebView2 (~500 MB freed); reopening rebuilds on demand
- **Zoom**: Ctrl + scroll / Ctrl + `+` `-` to zoom the page (0.5x–2x)
- **Single instance**: double-clicking the shortcut again just focuses the existing window
- **DPI-aware**: crisp rendering on any display scaling

## 🚀 Quick Start (end users)

**Requirements**: Windows 10/11 x64 · WebView2 Runtime (built into Win11 / updated with Edge) · Node.js 18+ on `PATH` (used to run `dsh`; falls back to `npx` automatically).

1. Download the latest `dsh-tiny-desktop.zip` from [Releases](../../releases)
2. Extract to any folder and double-click **`DSH Tray.exe`**
3. First launch starts the newest `dsh web` service silently and opens the native window; afterwards the whale icon lives in your system tray

> 💡 **Tip**: run it on another machine, or set it up differently — just paste this repository link into DeepSeek Harness / Claude / Codex or any AI coding product and let the AI read this README and handle the deployment for you. That is the fastest way to get going.

## 🔨 Build from source

**Requirements**: Go 1.21+ · WebView2 Runtime · Node.js 18+

```powershell
git clone https://github.com/Ichikawashadow/dsh-tiny-desktop
cd dsh-tiny-desktop
./build.ps1          # one-click build → DSH Tray.exe
```

Or manually:

```powershell
go run github.com/akavel/rsrc@latest -ico assets/dsh-v9.ico -o rsrc.syso
go build -ldflags "-H windowsgui" -o "DSH Tray.exe" .
```

That's it — **`DSH Tray.exe` is the whole app** (icon embedded, zero external files).

## 🖱️ Usage

| Action | Result |
|---|---|
| Double-click `DSH Tray.exe` / shortcut | Tray icon + native window opens |
| Tray menu | **Open window** · **Open in browser** · **Restart service** · **Exit (stop service)** |
| Close window | Service keeps running in tray; reopening rebuilds the window |
| Ctrl + scroll / Ctrl + `+`/`-` | Page zoom (0.5x–2x); Ctrl+`0` resets |
| Double-click shortcut again | Focuses the existing window (single instance) |

## 🔄 How updates work

The app locates the **newest** `@deepseek-ai/dsh` CLI under your npx cache (`%LOCALAPPDATA%\npm-cache\_npx\...`) at startup — new official releases are picked up automatically, no action needed.

## ⚙️ Configuration & Logs

- Port: `3080` (change the `port` constant in `main.go`, rebuild)
- Logs: `%USERPROFILE%\.dsh\logs\dsh-web.log` (service stdout/stderr + tray events)

## 🧩 Architecture

```
Go binary (single exe)
├── getlantern/systray        — system tray icon & menu
├── jchv/go-webview2          — WebView2 native window (pure Go, no CGO)
├── npx @deepseek-ai/dsh      — the actual Harness gateway (hidden window)
└── win32 interop             — DPI awareness, singleton mutex, window icon
```

## ⚠️ Disclaimer

This is an **independent community project**, not affiliated with or endorsed by DeepSeek. DeepSeek Harness itself is developed by DeepSeek AI and licensed under MIT.

## 📄 License

[MIT](LICENSE) — Copyright (c) 2026 dsh-tiny-desktop contributors
