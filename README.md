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
- **Desktop notifications**: the page's `Notification` API is bridged to a **white-card toast** (bottom-right, rounded corners with a light border, crisp ClearType text; hover states and action buttons, clicking focuses the window) — plugins like `dsh-notification` work out of the box, with no browser-style permission prompt and no reliance on the OS notification pipeline (Windows 11 no longer renders legacy tray balloons). It's a plain GDI window, so it renders reliably even over remote desktop / virtual displays; opening the same UI in a real browser still uses the browser's native notifications
- **Approve from the toast**: DSH **approval requests** (tool execution needing permission) pop up with **Allow once / Reject** buttons answered directly on the toast — no need to switch back to the window; it uses the same client-runtime response channel as the GUI (`approval/requested` → `PendingWait.respond`)
- **Smart & Manual Updates**: automatically detects and runs the highest version from global npm, pnpm, and npx cache; right-click tray menu provides one-click "**Check & Update DSH**"
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
| Tray menu | **Open window** · **Open in browser** · **Restart service** · **Check & Update DSH** · **Exit (stop service)** |
| Close window | Service keeps running in tray; reopening rebuilds the window |
| Ctrl + scroll / Ctrl + `+`/`-` | Page zoom (0.5x–2x); Ctrl+`0` resets |
| Double-click shortcut again | Focuses the existing window (single instance) |

## 🔄 How updates work

- **Automatic discovery**: searches global npm, pnpm, and npx cache (`%LOCALAPPDATA%\npm-cache\_npx\...`) to pick the highest semantic version at startup;
- **One-click manual update**: click "**Check & Update DSH**" in the tray menu to fetch and install the latest official release on demand with auto-restart, Windows lock self-healing, and native desktop notifications.

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

## 🌱 Ecosystem positioning

dsh-tiny-desktop is **complementary to, not competing with**, the DSH plugin ecosystem:

- **This shell**: a standalone desktop client (single Go binary) that handles the desktop experience of the DSH service — tray resident, native window, service lifecycle. It does **not** modify your DSH profile;
- **DSH plugins**: cordis plugins that run *inside* the DSH Web GUI (skins, stats, tool integrations…), installed via the [`dsh-plugin`](https://github.com/topics/dsh-plugin) topic or plugin marketplaces.

Either can be used without the other: DSH and its plugins work fine without this shell, and this shell works fine alongside any plugins.

## ⚠️ Disclaimer

This is an **independent community project**, not affiliated with or endorsed by DeepSeek. DeepSeek Harness itself is developed by DeepSeek AI and licensed under MIT.

## 📄 License

[MIT](LICENSE) — Copyright (c) 2026 dsh-tiny-desktop contributors
