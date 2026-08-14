// DSH Tray (Go) —— dsh-tiny-desktop
// DeepSeek Harness 的轻量原生 Windows 桌面壳
// 基于 getlantern/systray（托盘）+ jchv/go-webview2（WebView2 窗口，纯 Go）
// 双击启动：托盘驻留鲸鱼图标 + WebView2 原生窗口（单 exe，无外部依赖）
//   - 端口无服务   -> 隐藏窗口启动最新 npx dsh，就绪后自动进入界面
//   - 右键菜单     -> 打开窗口 / 在浏览器中打开 / 重启服务 / 退出（退出即停止服务）
//   - Ctrl+滚轮 / Ctrl+± 页面缩放（0.5x ~ 2x）
//   - 关窗即销毁窗口（释放 WebView2 内存约 500MB），再次打开自动重建
//   - 单例保护：重复启动仅聚焦已有窗口
//   - 内核永远跟随 npx 最新版 @deepseek-ai/dsh
// 日志：%USERPROFILE%\.dsh\logs\dsh-web.log
package main

import (
	_ "embed"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	webview "github.com/jchv/go-webview2"
)

// 图标嵌入 exe（单文件部署，无需外部资源）
//
//go:embed assets/dsh-v9.ico
var icoBytes []byte

const (
	port   = 3080
	webURL = "http://127.0.0.1:3080"
)

var (
	dshCmd  *exec.Cmd
	appDir  string
	wv      webview.WebView
	wvMu    sync.Mutex
	hwnd    uintptr
	exiting bool
)

// 页面缩放脚本（Ctrl+滚轮 / Ctrl+加减号）
const zoomScript = `
(function () {
  if (window.__dshZoomInstalled) return;
  window.__dshZoomInstalled = true;
  function getZoom() { return parseFloat(document.body.style.zoom) || 1; }
  function setZoom(z) {
    z = Math.min(2.0, Math.max(0.5, z));
    document.body.style.zoom = z;
    document.body.style.transformOrigin = '0 0';
  }
  document.addEventListener('wheel', function (e) {
    if (e.ctrlKey) {
      e.preventDefault();
      setZoom(getZoom() + (e.deltaY < 0 ? 0.1 : -0.1));
    }
  }, { passive: false });
  document.addEventListener('keydown', function (e) {
    if (e.ctrlKey && (e.key === '+' || e.key === '=' || e.key === '-' || e.key === '0')) {
      e.preventDefault();
      if (e.key === '0') setZoom(1);
      else setZoom(getZoom() + (e.key === '-' ? -0.1 : 0.1));
    }
  });
})();
`

const (
	swHide = 0
	swShow = 5
)

// user32 绑定（x/sys/windows 不含 UI 函数，自行声明）
var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procCreateIconFromRes   = user32.NewProc("CreateIconFromResourceEx")
	procSendMessage         = user32.NewProc("SendMessageW")

	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex         = kernel32.NewProc("CreateMutexW")
	procCreateEvent         = kernel32.NewProc("CreateEventW")
	procOpenEvent           = kernel32.NewProc("OpenEventW")
	procSetEvent            = kernel32.NewProc("SetEvent")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
)

const (
	errorAlreadyExists = 183
	eventModifyState   = 0x0002
	eventSynchronize   = 0x00100000
	waitObject0        = 0
	waitTimeout        = 258
)

// 单例保护：已有一个实例时，通知它显示窗口，本进程退出
func acquireSingleton() bool {
	name, _ := syscall.UTF16PtrFromString("DSHTrayAppMutex")
	// 注意：不能用 GetLastError()（goroutine 会切换线程，读不到 CreateMutex 的错误码），
	// 必须用 syscall 调用返回的 err（已在同一线程捕获）
	m, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	e, isErrno := err.(syscall.Errno)
	Log("单例: mutex=0x" + fmt.Sprintf("%X", m) + " errno=" + itoa(int(e)))
	if m == 0 {
		return true // 创建失败不阻塞（极端情况）
	}
	if isErrno && e == syscall.ERROR_ALREADY_EXISTS {
		// 通知主实例显示窗口
		evName, _ := syscall.UTF16PtrFromString("DSHTrayShowEvent")
		ev, _, _ := procOpenEvent.Call(eventModifyState|eventSynchronize, 0, uintptr(unsafe.Pointer(evName)))
		if ev != 0 {
			procSetEvent.Call(ev)
			procCloseHandle.Call(ev)
		}
		procCloseHandle.Call(m)
		return false
	}
	return true
}

// 主实例监听"显示窗口"事件（其他实例双击快捷方式时触发）
func watchShowEvent() {
	evName, _ := syscall.UTF16PtrFromString("DSHTrayShowEvent")
	ev, _, _ := procCreateEvent.Call(0, 0, 0, uintptr(unsafe.Pointer(evName)))
	if ev == 0 {
		Log("单例: 创建事件失败")
		return
	}
	for {
		if exiting {
			return
		}
		r, _, _ := procWaitForSingleObject.Call(ev, 1000) // 1 秒超时轮询
		if r == waitObject0 {
			Log("收到显示窗口请求（重复启动）")
			showWindow()
		}
	}
}

const (
	imageIcon      = 1
	lrLoadFromFile = 0x10
	wmSetIcon      = 0x0080
	iconSmall      = 0
	iconBig        = 1
)

func showWin(h uintptr, cmd int) {
	if h != 0 {
		procShowWindow.Call(h, uintptr(cmd))
	}
}

// 设置窗口标题栏图标（从嵌入的 ICO 数据创建 HICON，无需外部文件）
func setWindowIcon(h uintptr) {
	if h == 0 || len(icoBytes) == 0 {
		return
	}
	p := unsafe.Pointer(&icoBytes[0])
	// CreateIconFromResourceEx: (pResBits, dwResSize, fIcon=TRUE, dwVer=0x30000, cx, cy, flags=0)
	big, _, _ := procCreateIconFromRes.Call(uintptr(p), uintptr(len(icoBytes)), 1, 0x00030000, 32, 32, 0)
	small, _, _ := procCreateIconFromRes.Call(uintptr(p), uintptr(len(icoBytes)), 1, 0x00030000, 16, 16, 0)
	if big != 0 {
		procSendMessage.Call(h, wmSetIcon, iconBig, big)
	}
	if small != 0 {
		procSendMessage.Call(h, wmSetIcon, iconSmall, small)
	}
}

func main() {
	initDpiAware() // 必须在任何窗口创建前声明 DPI 感知（否则 WebView2 内容被位图缩放导致模糊）
	if !acquireSingleton() {
		// 已有实例在运行：已通知其显示窗口，本进程退出
		return
	}
	appDir, _ = filepath.Abs(filepath.Dir(os.Args[0]))
	systray.Run(onReady, onExit)
}

// 声明 Per-Monitor V2 DPI 感知，失败则回退 System DPI aware
func initDpiAware() {
	user32 := syscall.NewLazyDLL("user32.dll")
	if p := user32.NewProc("SetProcessDpiAwarenessContext"); p.Find() == nil {
		// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = -4
		r, _, _ := p.Call(uintptr(^uint(3)))
		if r != 0 {
			return
		}
	}
	if p := user32.NewProc("SetProcessDPIAware"); p.Find() == nil {
		p.Call()
	}
}

func onReady() {
	// 托盘图标（嵌入 exe）
	systray.SetIcon(icoBytes)
	systray.SetTitle("DeepSeek Harness")
	systray.SetTooltip("DeepSeek Harness")

	mOpen := systray.AddMenuItem("打开 DeepSeek Harness", "显示窗口")
	mBrowser := systray.AddMenuItem("在浏览器中打开", "用默认浏览器打开")
	mRestart := systray.AddMenuItem("重启服务", "重启 DSH 服务")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出（停止服务）", "停止服务并退出托盘")

	// 监听其他实例的"显示窗口"请求（双击桌面图标时）
	go watchShowEvent()

	// 预创建窗口：立即显示转圈加载页，服务就绪后自动切换到界面
	if portOpen(port) {
		Log("端口 " + itoa(port) + " 已有服务，直接驻留")
		go runWebview()
	} else {
		Log("端口 " + itoa(port) + " 无服务，启动 dsh web")
		go runWebview()
		startDsh()
	}

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				showWindow()
			case <-mBrowser.ClickedCh:
				openBrowser()
			case <-mRestart.ClickedCh:
				restartService()
			case <-mQuit.ClickedCh:
				exiting = true
				stopDsh()
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	Log("===== DSH Tray 已退出 =====")
}

// ---------- WebView2 窗口 ----------
func runWebview() {
	// webview 要求创建与消息循环在同一 OS 线程（COM）
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w := webview.New(false)
	if w == nil {
		Log("WebView2 创建失败（可能缺少运行时）")
		return
	}
	w.SetTitle("DeepSeek Harness")
	w.SetSize(1280, 800, webview.HintNone)

	h := uintptr(w.Window())
	wvMu.Lock()
	wv = w
	hwnd = h
	wvMu.Unlock()

	// 窗口标题栏图标（V9 设计）
	setWindowIcon(h)

	// 立即显示，等端口就绪后导航到真实界面（服务未就绪时避免连接错误页）
	showWin(h, swShow)
	go func() {
		for i := 0; i < 60; i++ {
			if exiting {
				return
			}
			if portOpen(port) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		w.Dispatch(func() { w.Navigate(webURL) })
		// 注入缩放脚本（页面加载后）
		time.Sleep(2500 * time.Millisecond)
		if exiting {
			return
		}
		w.Dispatch(func() { w.Eval(zoomScript) })
	}()

	w.Run() // 消息循环，窗口关闭后返回

	// 窗口已关闭：销毁 WebView2，释放约 500MB 内存
	wvMu.Lock()
	wv = nil
	hwnd = 0
	wvMu.Unlock()
	Log("窗口已关闭，WebView2 内存已释放")
}

func showWindow() {
	wvMu.Lock()
	h := hwnd
	w := wv
	wvMu.Unlock()

	if h == 0 {
		// 窗口已关闭销毁，重建（重新显示转圈页并自动进入界面）
		Log("窗口已释放，重新创建")
		go runWebview()
		return
	}
	showWin(h, swShow)
	procSetForegroundWindow.Call(h)
	if w != nil {
		w.Dispatch(func() { w.Eval(zoomScript) })
	}
	Log("显示窗口")
}

// ---------- 端口 ----------
func portOpen(p int) bool {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(p), 1500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ---------- 服务进程 ----------
func startDsh() {
	node := findNode()
	bin := findBinJs()
	if node == "" || bin == "" {
		Log("找不到 node/dsh CLI: node=" + node + " bin=" + bin)
		return
	}
	cmd := exec.Command(node, bin, "web", "--port", itoa(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	lf, err := openLogFile()
	if err == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	}
	if err := cmd.Start(); err != nil {
		Log("启动 dsh 失败: " + err.Error())
		return
	}
	dshCmd = cmd
	Log("已启动 dsh web (PID " + itoa(cmd.Process.Pid) + ")")
	go func() {
		cmd.Wait()
		Log("dsh web 进程退出")
	}()
}

func stopDsh() {
	if dshCmd != nil && dshCmd.Process != nil {
		Log("停止 dsh (PID " + itoa(dshCmd.Process.Pid) + ")")
		exec.Command("taskkill", "/PID", itoa(dshCmd.Process.Pid), "/T", "/F").Run()
		dshCmd = nil
	} else {
		if pid := pidOnPort(port); pid > 0 {
			Log("停止外部实例 (PID " + itoa(pid) + ")")
			exec.Command("taskkill", "/PID", itoa(pid), "/T", "/F").Run()
		}
	}
}

func restartService() {
	Log("===== 重启服务 =====")
	stopDsh()
	time.Sleep(1200 * time.Millisecond)
	startDsh()
}

// ---------- 浏览器 ----------
func openBrowser() {
	Log("打开浏览器 " + webURL)
	exec.Command("rundll32", "url.dll,FileProtocolHandler", webURL).Start()
}

// ---------- 路径解析 ----------
func findNode() string {
	if p := findOnPath("node.exe"); p != "" {
		return p
	}
	alt := `C:\Program Files\nodejs\node.exe`
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	return ""
}

func findOnPath(name string) string {
	path := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		full := filepath.Join(dir, name)
		if _, err := os.Stat(full); err == nil {
			return full
		}
	}
	return ""
}

func findBinJs() string {
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	npxRoot := filepath.Join(local, "npm-cache", "_npx")
	dirs, err := os.ReadDir(npxRoot)
	if err != nil {
		return ""
	}
	var cands []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		js := filepath.Join(npxRoot, d.Name(), "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js")
		if _, err := os.Stat(js); err == nil {
			cands = append(cands, js)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		ti, _ := os.Stat(cands[i])
		tj, _ := os.Stat(cands[j])
		return ti.ModTime().After(tj.ModTime())
	})
	return cands[0]
}

// ---------- 日志 ----------
func openLogFile() (*os.File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".dsh", "logs")
	os.MkdirAll(dir, 0755)
	return os.OpenFile(filepath.Join(dir, "dsh-web.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

func Log(msg string) {
	lf, err := openLogFile()
	if err != nil {
		return
	}
	defer lf.Close()
	fmt.Fprintf(lf, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

// ---------- 工具 ----------
func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

func pidOnPort(p int) int {
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0
	}
	target := "127.0.0.1:" + itoa(p)
	for _, line := range splitLines(string(out)) {
		if !contains(line, target) {
			continue
		}
		fields := splitFields(line)
		if len(fields) >= 5 && fields[3] == "LISTENING" {
			var pid int
			fmt.Sscanf(fields[4], "%d", &pid)
			return pid
		}
	}
	return 0
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\r' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
