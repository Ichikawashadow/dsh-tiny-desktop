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
//
// 日志：%USERPROFILE%\.dsh\logs\dsh-web.log
package main

import (
	"context"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"
	webview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// 图标嵌入 exe（单文件部署，无需外部资源）
//
//go:embed assets/dsh-v9.ico
var icoBytes []byte

const (
	port   = 3080
	webURL = "http://127.0.0.1:3080"
)

// 页面通知桥：把 WebView2 里的 Notification API 换成 shim，
// 权限恒为 granted，new Notification(...) 转发给 Go 原生浮窗；
// 并挂载授权桥：订阅 DSH 客户端运行时（composer 槽）的 pending 授权请求，
// 推送带「允许一次/拒绝」按钮的通知，按钮结果经 window.__dshRespond 回传应答。
// 不依赖 WebView2 的 PermissionRequested（宿主不处理默认拒绝），
// 也不受"非打包应用需 AUMID 才显示通知"的限制；在真浏览器中打开时不受影响。
const notifyShimScript = `
(function () {
  if (window.__dshNotifyShimInstalled) return;
  window.__dshNotifyShimInstalled = true;
  var Real = window.Notification;
  function route(nt) {
    try {
      if (window.__dshNativeNotify) {
        var p = window.__dshNativeNotify({
          title: String(nt.title || ''),
          body: String(nt.body || ''),
          tag: String(nt.tag || ''),
          requireInteraction: !!nt.requireInteraction
        });
        if (p && typeof p.catch === 'function') p.catch(function () {});
      } else if (Real) {
        new Real(nt.title, { body: nt.body, tag: nt.tag, requireInteraction: nt.requireInteraction });
      }
    } catch (e) {}
  }
  function Shim(title, options) {
    if (!(this instanceof Shim)) return new Shim(title, options);
    this.title = title;
    this.body = (options && options.body) || '';
    this.tag = (options && options.tag) || '';
    this.requireInteraction = !!(options && options.requireInteraction);
    this.onclick = null;
    this.onshow = null;
    this.onerror = null;
    this.onclose = null;
    this.timestamp = Date.now();
    var self = this;
    route(this);
    try {
      if (typeof queueMicrotask === 'function') queueMicrotask(function () {
        if (self.onshow) self.onshow.call(self, { type: 'show' });
      });
    } catch (e) {}
  }
  Shim.permission = 'granted';
  Shim.requestPermission = function () { return Promise.resolve('granted'); };
  Shim.maxActions = 0;
  Shim.prototype.close = function () {};
  try {
    Object.defineProperty(window, 'Notification', { value: Shim, writable: true, configurable: true });
  } catch (e) {
    window.Notification = Shim;
  }

  // ---------- 授权桥（approval bridge） ----------
  // 订阅客户端运行时 composer 槽的 pending 授权，推到通知浮窗；
  // 浮窗按钮点击后 Go 侧 Eval 调用 window.__dshRespond 完成应答。
  var waits = {};      // pending key -> PendingWait
  var current = null;  // 当前已推送的授权 key
  function push(wait) {
    try {
      waits[wait.key] = wait;
      if (current === wait.key) return;
      current = wait.key;
      var p = wait.payload || {};
      var reason = p.reason || (p.toolName ? ('工具 ' + p.toolName + ' 需要授权') : 'DSH 请求授权');
      var pr = window.__dshNativeNotify({
        title: '需要授权',
        body: String(reason || ''),
        tag: 'approval:' + wait.key,
        requireInteraction: true,
        actions: [{ id: 'allowed-once', label: '允许一次' }, { id: 'rejected', label: '拒绝' }],
        ref: wait.key
      });
      if (pr && typeof pr.catch === 'function') pr.catch(function () {});
    } catch (e) {}
  }
  function drop(key) {
    try { delete waits[key]; } catch (e) {}
    if (current === key) current = null;
  }
  function respond(ref, action) {
    var wait = waits[ref];
    if (!wait) return;
    drop(ref);
    var p = wait.payload || {};
    var outcome = (action === 'allowed-once' || action === 'rejected') ? action : 'rejected';
    Promise.resolve().then(function () {
      wait.respond({ ok: true, value: { sessionId: wait.sessionId, approvalId: p.approvalId, outcome: outcome } })
        .catch(function () {});
    });
  }
  window.__dshRespond = function (msg) {
    try { respond(msg && msg.ref, msg && msg.action); } catch (e) {}
  };
  function hook() {
    if (!window.__ModuleLoader__ || window.__dshApprovalBridgeInstalled) return false;
    window.__dshApprovalBridgeInstalled = true;
    try {
      window.__ModuleLoader__.load({ id: 'dsh-tray-approval-bridge', factory: function (require) {
        var react = require('react');
        function Bridge(props) {
          var matched = props.matched;
          react.useEffect(function () {
            if (!matched) return undefined;
            try { push(matched); } catch (e) {}
            return function () { try { drop(matched.key); } catch (e) {} };
          }, [matched]);
          return null;
        }
        return {
          inject: ['slots'],
          apply: function (ctx) {
            try {
              ctx.locale.register('dsh-tray', { zh: {}, en: {} });
            } catch (e) {}
            try {
              ctx.slots.inject('conversation.composer', function () { return ctx.slots.register({
                name: 'conversation.composer',
                select: function (state) {
                  var i = state && state.interactions;
                  if (!i) return null;
                  for (var k = 0; k < i.length; k++) if (i[k] && i[k].kind === 'approval') return i[k];
                  return null;
                },
                priority: 9999,
                locale: 'dsh-tray'
              }, Bridge); });
            } catch (e) {
              window.__dshApprovalBridgeInstalled = false;
              console.warn('[dsh-tray] approval bridge slot failed', e);
            }
          }
        };
      }});
      return true;
    } catch (e) {
      window.__dshApprovalBridgeInstalled = false;
      console.warn('[dsh-tray] approval bridge load failed', e);
      return false;
    }
  }
  // 模块加载器就绪后挂载（轮询等待，最长 60s）
  if (!hook()) {
    var tries = 0;
    var iv = setInterval(function () {
      tries++;
      if (hook() || tries > 120) clearInterval(iv);
    }, 500);
  }
})();
`

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

// 外部链接拦截脚本：DSH 对话中的网页链接（非本服务域名）一律交给 Edge 打开
const externalScript = `
(function () {
  if (window.__dshExternalInstalled) return;
  window.__dshExternalInstalled = true;
  function isExternal(href) {
    if (!href || typeof href !== 'string') return false;
    if (href.indexOf('http:') !== 0 && href.indexOf('https:') !== 0) return false;
    var origin = window.location.origin;
    return href.indexOf(origin) !== 0;
  }
  document.addEventListener('click', function (e) {
    var el = e.target;
    while (el && el !== document) {
      if (el.tagName === 'A' && isExternal(el.href)) {
        e.preventDefault();
        e.stopPropagation();
        window.__dshOpenExternal && window.__dshOpenExternal(el.href);
        return;
      }
      el = el.parentElement;
    }
  }, true);
  var origOpen = window.open;
  window.open = function (u) {
    if (u && typeof u === 'string' && isExternal(u)) {
      window.__dshOpenExternal && window.__dshOpenExternal(u);
      return null;
    }
    return origOpen.apply(this, arguments);
  };
})();
`

const (
	swHide = 0
	swShow = 5
)

// user32 绑定（x/sys/windows 不含 UI 函数，自行声明）
var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	procShowWindow           = user32.NewProc("ShowWindow")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procCreateIconFromRes    = user32.NewProc("CreateIconFromResourceEx")
	procSendMessage          = user32.NewProc("SendMessageW")
	procCreateWindowEx       = user32.NewProc("CreateWindowExW")
	procRegisterClassEx      = user32.NewProc("RegisterClassExW")
	procDefWindowProc        = user32.NewProc("DefWindowProcW")
	procGetMessage           = user32.NewProc("GetMessageW")
	procTranslateMessage     = user32.NewProc("TranslateMessage")
	procDispatchMessage      = user32.NewProc("DispatchMessageW")
	procPostMessage          = user32.NewProc("PostMessageW")
	procPostQuitMessage      = user32.NewProc("PostQuitMessage")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procSetWindowRgn         = user32.NewProc("SetWindowRgn")
	procSetTimer             = user32.NewProc("SetTimer")
	procKillTimer            = user32.NewProc("KillTimer")
	procInvalidateRect       = user32.NewProc("InvalidateRect")
	procIsWindowVisible      = user32.NewProc("IsWindowVisible")
	procGetCursorPos         = user32.NewProc("GetCursorPos")
	procMonitorFromPoint     = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW      = user32.NewProc("GetMonitorInfoW")
	procSystemParametersInfo = user32.NewProc("SystemParametersInfoW")
	procGetDpiForWindow      = user32.NewProc("GetDpiForWindow")
	procGetDC                = user32.NewProc("GetDC")
	procReleaseDC            = user32.NewProc("ReleaseDC")
	procGetClientRect        = user32.NewProc("GetClientRect")
	procDrawText             = user32.NewProc("DrawTextW")
	procDrawIconEx           = user32.NewProc("DrawIconEx")
	procBeginPaint           = user32.NewProc("BeginPaint")
	procEndPaint             = user32.NewProc("EndPaint")
	procTrackMouseEvent      = user32.NewProc("TrackMouseEvent")

	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex         = kernel32.NewProc("CreateMutexW")
	procCreateEvent         = kernel32.NewProc("CreateEventW")
	procOpenEvent           = kernel32.NewProc("OpenEventW")
	procSetEvent            = kernel32.NewProc("SetEvent")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procGetModuleHandle     = kernel32.NewProc("GetModuleHandleW")
	procMulDiv              = kernel32.NewProc("MulDiv")

	gdi32                  = syscall.NewLazyDLL("gdi32.dll")
	procCreateSolidBrush   = gdi32.NewProc("CreateSolidBrush")
	procCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	procCreateFont         = gdi32.NewProc("CreateFontW")
	procCreatePen          = gdi32.NewProc("CreatePen")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procSetBkMode          = gdi32.NewProc("SetBkMode")
	procSetTextColor       = gdi32.NewProc("SetTextColor")
	procGetStockObject     = gdi32.NewProc("GetStockObject")
	procGetPixel           = gdi32.NewProc("GetPixel")
	procRoundRect          = gdi32.NewProc("RoundRect")
	procMoveToEx           = gdi32.NewProc("MoveToEx")
	procLineTo             = gdi32.NewProc("LineTo")
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

// 设置窗口标题栏/任务栏图标（从嵌入 ICO 中提取单图像创建 HICON，无需外部文件）
func setWindowIcon(h uintptr) {
	if h == 0 || len(icoBytes) == 0 {
		return
	}
	big := iconFromIco(icoBytes, 32, 32)
	small := iconFromIco(icoBytes, 16, 16)
	Log("窗口图标: big=0x" + fmt.Sprintf("%X", big) + " small=0x" + fmt.Sprintf("%X", small))
	if big != 0 {
		procSendMessage.Call(h, wmSetIcon, iconBig, big)
	}
	if small != 0 {
		procSendMessage.Call(h, wmSetIcon, iconSmall, small)
	}
}

// 从 ICO 文件字节中提取最接近目标尺寸的单图像，创建 HICON
// （CreateIconFromResourceEx 需要 ICONDIRENTRY 内的图像数据，而非完整 ICO 文件）
func iconFromIco(data []byte, wantW, wantH int) uintptr {
	if len(data) < 6 {
		return 0
	}
	count := int(binary.LittleEndian.Uint16(data[4:6]))
	if len(data) < 6+count*16 {
		return 0
	}
	best := -1
	bestScore := int(^uint(0) >> 1)
	for i := 0; i < count; i++ {
		off := 6 + i*16
		w := int(data[off])
		if w == 0 {
			w = 256
		}
		h := int(data[off+1])
		if h == 0 {
			h = 256
		}
		size := int(binary.LittleEndian.Uint32(data[off+8:]))
		imgOff := int(binary.LittleEndian.Uint32(data[off+12:]))
		if imgOff+size > len(data) || size <= 0 {
			continue
		}
		score := abs(w-wantW) + abs(h-wantH)
		if score < bestScore {
			bestScore = score
			best = i
		}
	}
	if best < 0 {
		return 0
	}
	off := 6 + best*16
	size := int(binary.LittleEndian.Uint32(data[off+8:]))
	imgOff := int(binary.LittleEndian.Uint32(data[off+12:]))
	p := unsafe.Pointer(&data[imgOff])
	// CreateIconFromResourceEx(pResBits, dwResSize, fIcon=TRUE, dwVer=0x30000, cx=0, cy=0, flags=0)
	r, _, _ := procCreateIconFromRes.Call(uintptr(p), uintptr(size), 1, 0x00030000, 0, 0, 0)
	return r
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ---------- 桌面通知（自绘 toast 浮窗，白色卡片 + 每像素 alpha 渲染） ----------
// Windows 11 不再渲染 Shell_NotifyIcon 传统气泡（API 返回成功但无画面），
// 而系统 toast 要求非打包应用注册 AUMID。因此改为自绘置顶浮窗：
// layered window（UpdateLayeredWindow + ARGB 位图）—— 卡片/圆角/阴影按像素绘制，
// 文字用 ClearType 按物理分辨率渲染（高 DPI 不发糊），
// 不经过系统通知路由（专注助手/请勿打扰/通知设置都无法拦截）。
const (
	wmUser         = 0x0400
	wmTimer        = 0x0113
	wmPaint        = 0x000F
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmMouseMove    = 0x0200
	wmMouseLeave   = 0x02A3
	wmClose        = 0x0010
	wmDestroy      = 0x0002
	toastCardW     = 400 // 卡片宽（96 DPI 基准）
	toastMargin    = 16  // 距屏幕右下角
	toastShowMs    = 6000
	toastTimerID   = 1
	toastShowMsg   = wmUser + 60
	toastClass     = "DSHToastClass"
	swpNoActivate  = 0x0010
	swpShowWindow  = 0x0040
	hwndTopmost    = ^uintptr(0) // HWND_TOPMOST = -1
	wsPopup        = 0x80000000
	wsExToolWindow = 0x00000080
	wsExTopmost    = 0x00000008
	wsExNoActivate = 0x08000000
	dTNoPrefix     = 0x00000800
	dTSingleLine   = 0x00000020
	dTEndEllipsis  = 0x00008000
	dTWordBreak    = 0x00000010
	dTCalcRect     = 0x00000400
	dTCenter       = 0x00000001
	dTVCenter      = 0x00000004
	transparent    = 1 // SetBkMode: TRANSPARENT
	monitorNearest = 2 // MONITOR_DEFAULTTONEAREST
	spiGetWorkArea = 0x0030
	diNormal       = 3 // DrawIconEx: DI_NORMAL
	fwSemibold     = 600
	fwRegular      = 400
	clearTypeQ     = 5 // CLEARTYPE_QUALITY
	defaultCharset = 1
	psSolid        = 0 // CreatePen: PS_SOLID
	tmeLeave       = 0x00000002
	// 浅色主题（GDI COLORREF，0x00BBGGRR）
	toastBgGDI       = 0x00FFFFFF // 白底
	toastBorderGDI   = 0x00E2E2E2 // 描边
	toastTileBgGDI   = 0x00F3F4F6 // 瓦片底
	toastTileEdgeGDI = 0x00E2E2E2 // 瓦片描边
	btnRejectBgGDI   = 0x00FFFFFF // 次按钮底
	btnRejectEdgeGDI = 0x00D9D9D9 // 次按钮描边
	btnRejectHotGDI  = 0x00F5F5F5 // 次按钮 hover 底
	btnRejectHotEGDI = 0x00BDBDBD // 次按钮 hover 描边
	btnAllowBgGDI    = 0x007FA310 // 主按钮 RGB(16,163,127)
	btnAllowHotGDI   = 0x0073930E // 主按钮 hover RGB(14,147,115)
	// 布局（96 DPI 基准）
	toastPad      = 16 // 卡片内边距
	toastRadius   = 14 // 卡片圆角
	toastIconSize = 40 // 图标瓦片
	toastIconGap  = 12 // 瓦片与文字间距
	toastTitleTop = 16
	toastTitleH   = 28 // 标题行高（14pt 文字 ~19px，留足余量）
	toastBodyTop  = 50 // 正文顶（标题底 + 8px 间距）
	toastBottom   = 16 // 底部内边距
	toastBtnH     = 34 // 按钮高
	toastBtnTop   = 14 // 按钮距底
	toastBtnGap   = 10 // 按钮间距
	toastBtnPadX  = 18 // 按钮文字左右内边距
	toastBtnR     = 9  // 按钮圆角
	toastCloseSz  = 20 // 关闭按钮区域
	toastCloseTop = 12
	toastTileR    = 10 // 图标瓦片圆角
	toastMinH     = 96
	toastMaxH     = 300
)

var (
	toastMu          sync.Mutex
	toastHwnd        uintptr
	toastTitle       string
	toastBody        string
	toastInteractive bool
	toastActions     []notifyAction
	toastRef         string // 应答回执标识（如 pending key）
	toastHover       int    // 悬停区域：0 无 / 1 关闭 / 2.. 按钮
	toastFontTitle   uintptr
	toastFontBody    uintptr
	toastFontButton  uintptr
	toastFontDpi     uint32
	toastIcon        uintptr // 鲸鱼 HICON，惰性创建
	toastBtnBrushes  map[uint32]uintptr
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	ClsExtra      int32
	WndExtra      int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type rect struct {
	Left, Top, Right, Bottom int32
}

type point struct {
	X, Y int32
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

type paintStruct struct {
	Hdc         uintptr
	FErase      bool
	RcPaint     rect
	FRestore    bool
	FIncUpdate  bool
	RgbReserved [32]byte
}

type trackMouseEvent struct {
	CbSize      uint32
	DwFlags     uint32
	HWndTrack   uintptr
	DwHoverTime uint32
}

type notifyAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type notifyPayload struct {
	Title              string         `json:"title"`
	Body               string         `json:"body"`
	Tag                string         `json:"tag"`
	RequireInteraction bool           `json:"requireInteraction"`
	Actions            []notifyAction `json:"actions"`
	Ref                string         `json:"ref"`
}

// toast 窗口线程：独立消息泵（创建窗口必须与消息循环同线程）
func toastLoop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hinst, _, _ := procGetModuleHandle.Call(0)
	className, _ := syscall.UTF16PtrFromString(toastClass)
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   windows.NewCallback(toastWndProc),
		HInstance:     hinst,
		HbrBackground: toastBrush(toastBgGDI),
		LpszClassName: className,
	}
	procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))

	h, _, _ := procCreateWindowEx.Call(
		wsExToolWindow|wsExTopmost|wsExNoActivate,
		uintptr(unsafe.Pointer(className)),
		0, // 无标题
		wsPopup,
		0, 0, 100, 100, // 先隐藏创建，显示时再定位
		0, 0, hinst, 0,
	)
	toastMu.Lock()
	toastHwnd = h
	toastMu.Unlock()
	Log("通知: toast 窗口就绪 hwnd=0x" + fmt.Sprintf("%X", h))

	var m msg
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	toastMu.Lock()
	toastHwnd = 0
	toastMu.Unlock()
	Log("通知: toast 线程退出")
}

// 页面通知桥入口：JS shim → 这里
func showToast(p notifyPayload) bool {
	toastMu.Lock()
	toastTitle = p.Title
	toastBody = p.Body
	toastInteractive = p.RequireInteraction
	toastActions = p.Actions
	toastRef = p.Ref
	toastHover = 0
	h := toastHwnd
	toastMu.Unlock()
	if h == 0 {
		Log("通知: toast 窗口未就绪")
		return false
	}
	procPostMessage.Call(h, toastShowMsg, 0, 0)
	Log("通知: " + p.Title + " — " + p.Body)
	return true
}

func toastWndProc(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
	switch message {
	case toastShowMsg:
		showToastWindow(hwnd)
		return 0
	case wmTimer:
		if wparam == toastTimerID {
			procShowWindow.Call(hwnd, swHide)
			return 0
		}
	case wmLButtonUp:
		toastClick(hwnd, int32(int16(lparam&0xFFFF)), int32(int16(lparam>>16)))
		return 0
	case wmRButtonUp:
		procShowWindow.Call(hwnd, swHide)
		return 0
	case wmMouseMove:
		h := toastHit(hwnd, int32(int16(lparam&0xFFFF)), int32(int16(lparam>>16)))
		toastMu.Lock()
		changed := h != toastHover
		toastHover = h
		toastMu.Unlock()
		if changed {
			procInvalidateRect.Call(hwnd, 0, 1)
		}
		tme := trackMouseEvent{CbSize: uint32(unsafe.Sizeof(trackMouseEvent{})), DwFlags: tmeLeave, HWndTrack: hwnd}
		procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
		return 0
	case wmMouseLeave:
		toastMu.Lock()
		toastHover = 0
		toastMu.Unlock()
		procInvalidateRect.Call(hwnd, 0, 1)
		return 0
	case wmDestroy:
		// DefWindowProc 不会自动 PostQuitMessage，这里显式结束消息泵
		procPostQuitMessage.Call(0)
		r, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wparam, lparam)
		return r
	case wmPaint:
		paintToast(hwnd)
		return 0
	}
	r, _, _ := procDefWindowProc.Call(hwnd, uintptr(message), wparam, lparam)
	return r
}

// 点击处理：关闭 × / 操作按钮 / 其余区域聚焦窗口
func toastClick(hwnd uintptr, x, y int32) {
	hit := toastHit(hwnd, x, y)
	switch {
	case hit == 1: // 关闭
		Log("通知: 点击关闭")
		procShowWindow.Call(hwnd, swHide)
	case hit >= 2: // 操作按钮
		toastMu.Lock()
		actions := toastActions
		ref := toastRef
		toastMu.Unlock()
		i := hit - 2
		if i < len(actions) {
			Log("通知: 按钮 " + actions[i].ID)
			procShowWindow.Call(hwnd, swHide)
			sendRespond(ref, actions[i].ID)
		}
	default:
		Log("通知: 点击通知浮窗，聚焦窗口")
		procShowWindow.Call(hwnd, swHide)
		showWindow()
	}
}

// 把按钮结果回传给页面（window.__dshRespond，由页面注入模块应答 pending）
func sendRespond(ref, action string) {
	if ref == "" {
		return
	}
	wvMu.Lock()
	w := wv
	wvMu.Unlock()
	if w == nil {
		Log("通知: 页面不存在，无法应答 " + action)
		return
	}
	js := "window.__dshRespond && window.__dshRespond(" + jsString(map[string]string{"ref": ref, "action": action}) + ")"
	w.Dispatch(func() { w.Eval(js) })
}

// 显示/更新浮窗：定位（不显示）→ UpdateLayeredWindow 上屏 → 再显示
// （layered 窗口必须先 ULW 后 Show，顺序反了内容可能不出现）
func showToastWindow(hwnd uintptr) {
	procKillTimer.Call(hwnd, toastTimerID)

	toastMu.Lock()
	interactive := toastInteractive
	actions := toastActions
	toastMu.Unlock()

	dpi := getToastDpi(hwnd)
	g := toastGeometry(dpi, actions)
	x, y := toastPos(dpi, g.WinW, g.WinH)
	procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(g.WinW), uintptr(g.WinH), swpNoActivate|swpShowWindow)

	// 双步定位：跨 DPI 显示器时按目标 dpi 重新测量
	dpi2 := getToastDpi(hwnd)
	if dpi2 != dpi {
		g = toastGeometry(dpi2, actions)
		x, y = toastPos(dpi2, g.WinW, g.WinH)
		procSetWindowPos.Call(hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(g.WinW), uintptr(g.WinH), swpNoActivate|swpShowWindow)
	}

	// 圆角窗口区域（GDI 普通窗口，兼容远程/虚拟显示环境）
	rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(g.WinW), uintptr(g.WinH), uintptr(scale(toastRadius*2, dpi)), uintptr(scale(toastRadius*2, dpi)))
	if rgn != 0 {
		procSetWindowRgn.Call(hwnd, rgn, 1)
	}
	procInvalidateRect.Call(hwnd, 0, 1)
	if !interactive {
		procSetTimer.Call(hwnd, toastTimerID, toastShowMs, 0)
	}
}

// DPI 精确缩放
func scale(v int32, dpi uint32) int32 {
	return v * int32(dpi) / 96
}

// 浮窗位置：光标所在显示器工作区右下角（含边距）
func toastPos(dpi uint32, w, h int) (int32, int32) {
	work := workArea()
	dx := int32(w) + toastMargin
	dy := int32(h) + toastMargin
	x := work.Right - dx
	y := work.Bottom - dy
	if x < work.Left+toastMargin {
		x = work.Left + toastMargin
	}
	if y < work.Top+toastMargin {
		y = work.Top + toastMargin
	}
	return x, y
}

// 光标所在显示器的工作区；失败回退主屏
func workArea() rect {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	ptv := uintptr(uint32(pt.X)) | (uintptr(uint32(pt.Y)) << 32) // POINT 按值传递
	mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if hMon, _, _ := procMonitorFromPoint.Call(ptv, monitorNearest); hMon != 0 {
		if r, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); r != 0 {
			return mi.RcWork
		}
	}
	var wa rect
	procSystemParametersInfo.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&wa)), 0)
	return wa
}

func getToastDpi(hwnd uintptr) uint32 {
	if dpi, _, _ := procGetDpiForWindow.Call(hwnd); dpi != 0 {
		return uint32(dpi)
	}
	return 96
}

// 几何布局（窗口坐标）
type toastGeom struct {
	Card    rect // 卡片区域（= 窗口客户区）
	Title   rect
	Body    rect
	Close   rect
	Tile    rect
	Buttons []rect
	CardW   int
	CardH   int
	WinW    int
	WinH    int
}

// 完整几何：按 dpi 与 actions 计算卡片尺寸与各区域
func toastGeometry(dpi uint32, actions []notifyAction) toastGeom {
	cardW := int(scale(toastCardW, dpi))
	bodyW := cardW - int(scale(toastPad+toastIconSize+toastIconGap, dpi)) - int(scale(toastPad, dpi))
	toastMu.Lock()
	body := toastBody
	toastMu.Unlock()
	bodyH := measureBodyH(body, dpi, bodyW)
	// 卡片高度必须与 layoutToast 的矩形一致地按 DPI 缩放：
	// bodyH 是已按 dpi 实测的高度，若这里仍用 96-DPI 基准常量相加，
	// 高 DPI 下 cardH 偏小，layoutToast 再把各矩形按 dpi 放大后，
	// 正文矩形高度会被压到不足一行 —— DrawText 把字形下半截裁掉（半截字）。
	cardH := int(scale(toastBodyTop, dpi)) + bodyH + int(scale(toastBottom, dpi))
	if len(actions) > 0 {
		cardH = int(scale(toastBodyTop, dpi)) + bodyH + int(scale(12+toastBtnH+toastBtnTop, dpi))
	}
	minH := int(scale(toastMinH, dpi))
	if len(actions) > 0 {
		minH = minH + int(scale(30, dpi))
	}
	if cardH < minH {
		cardH = minH
	}
	maxH := int(scale(toastMaxH, dpi))
	if cardH > maxH {
		cardH = maxH
	}
	return layoutToast(dpi, cardW, cardH, actions)
}

// 布局：卡片/标题/正文/关闭/按钮（按钮从右往左排，最后一个为主按钮）
func layoutToast(dpi uint32, cardW, cardH int, actions []notifyAction) toastGeom {
	g := toastGeom{}
	g.CardW, g.CardH = cardW, cardH
	g.WinW, g.WinH = cardW, cardH
	ox, oy := int32(0), int32(0)
	g.Card = rect{ox, oy, ox + int32(cardW), oy + int32(cardH)}

	pad := scale(toastPad, dpi)
	iconSize := scale(toastIconSize, dpi)
	g.Tile = rect{ox + pad, oy + pad, ox + pad + iconSize, oy + pad + iconSize}

	titleX := ox + pad + iconSize + scale(toastIconGap, dpi)
	titleRight := ox + int32(cardW) - pad
	g.Title = rect{titleX, oy + scale(toastTitleTop, dpi), titleRight, oy + scale(toastTitleTop, dpi) + scale(toastTitleH, dpi)}

	cs := scale(toastCloseSz, dpi)
	g.Close = rect{titleRight - cs, oy + scale(toastCloseTop, dpi), titleRight, oy + scale(toastCloseTop, dpi) + cs}

	bodyBottom := oy + int32(cardH) - scale(toastBottom, dpi)
	if len(actions) > 0 {
		bodyBottom = oy + int32(cardH) - scale(12+toastBtnH+toastBtnTop, dpi)
	}
	g.Body = rect{titleX, oy + scale(toastBodyTop, dpi), titleRight, bodyBottom}

	if len(actions) > 0 {
		hdc, _, _ := procGetDC.Call(0)
		if hdc != 0 {
			_, _, fBtn := toastFonts(dpi)
			by := oy + int32(cardH) - scale(toastBtnTop, dpi) - scale(toastBtnH, dpi)
			x := titleRight
			for i := len(actions) - 1; i >= 0; i-- {
				bw := scale(int32(textWidth(hdc, actions[i].Label, fBtn)+2*toastBtnPadX), dpi)
				r := rect{x - bw, by, x, by + scale(toastBtnH, dpi)}
				g.Buttons = append(g.Buttons, r)
				x = r.Left - scale(toastBtnGap, dpi)
			}
			procReleaseDC.Call(0, hdc)
			for i, j := 0, len(g.Buttons)-1; i < j; i, j = i+1, j-1 {
				g.Buttons[i], g.Buttons[j] = g.Buttons[j], g.Buttons[i]
			}
		}
	}
	return g
}

// 正文自然换行高度（DT_CALCRECT）
func measureBodyH(body string, dpi uint32, width int) int {
	if body == "" {
		return 0
	}
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return int(scale(18, dpi))
	}
	defer procReleaseDC.Call(0, hdc)
	_, fBody, _ := toastFonts(dpi)
	procSelectObject.Call(hdc, fBody)
	b, _ := syscall.UTF16FromString(body)
	r := rect{0, 0, int32(width), 0}
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(&b[0])), ^uintptr(0), uintptr(unsafe.Pointer(&r)), dTWordBreak|dTNoPrefix|dTCalcRect)
	return int(r.Bottom - r.Top)
}

func inRect(x, y int32, r rect) bool {
	return x >= r.Left && x < r.Right && y >= r.Top && y < r.Bottom
}

// 命中测试：0 无 / 1 关闭 / 2.. 按钮
func toastHit(hwnd uintptr, x, y int32) int {
	dpi := getToastDpi(hwnd)
	toastMu.Lock()
	actions := toastActions
	toastMu.Unlock()
	g := toastGeometry(dpi, actions)
	if inRect(x, y, g.Close) {
		return 1
	}
	for i, r := range g.Buttons {
		if inRect(x, y, r) {
			return 2 + i
		}
	}
	return 0
}

// 缓存按 DPI 创建的标题/正文/按钮字体（Segoe UI，ClearType 抗锯齿）
func toastFonts(dpi uint32) (uintptr, uintptr, uintptr) {
	if toastFontTitle != 0 && toastFontDpi == dpi {
		return toastFontTitle, toastFontBody, toastFontButton
	}
	if toastFontTitle != 0 {
		procDeleteObject.Call(toastFontTitle)
		procDeleteObject.Call(toastFontBody)
		procDeleteObject.Call(toastFontButton)
	}
	face, _ := syscall.UTF16PtrFromString("Segoe UI")
	toastFontTitle, _, _ = procCreateFont.Call(
		uintptr(int32(-mulDiv(14, int32(dpi), 72))), // 14pt
		0, 0, 0, fwSemibold, 0, 0, 0, defaultCharset, 0, 0, clearTypeQ, 0,
		uintptr(unsafe.Pointer(face)),
	)
	toastFontBody, _, _ = procCreateFont.Call(
		uintptr(int32(-mulDiv(11, int32(dpi), 72))), // 11pt
		0, 0, 0, fwRegular, 0, 0, 0, defaultCharset, 0, 0, clearTypeQ, 0,
		uintptr(unsafe.Pointer(face)),
	)
	toastFontButton, _, _ = procCreateFont.Call(
		uintptr(int32(-mulDiv(11, int32(dpi), 72))), // 11pt
		0, 0, 0, fwSemibold, 0, 0, 0, defaultCharset, 0, 0, clearTypeQ, 0,
		uintptr(unsafe.Pointer(face)),
	)
	toastFontDpi = dpi
	return toastFontTitle, toastFontBody, toastFontButton
}

func textWidth(hdc uintptr, s string, f uintptr) int {
	procSelectObject.Call(hdc, f)
	b, _ := syscall.UTF16FromString(s)
	r := rect{0, 0, 0, 0}
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(&b[0])), ^uintptr(0), uintptr(unsafe.Pointer(&r)), dTSingleLine|dTNoPrefix|dTCalcRect)
	return int(r.Right - r.Left)
}

// × 关闭符号
func drawCross(hdc uintptr, cx, cy, size int32, color uint32) {
	pen, _, _ := procCreatePen.Call(psSolid, 1, uintptr(color))
	if pen == 0 {
		return
	}
	procSelectObject.Call(hdc, pen)
	procMoveToEx.Call(hdc, uintptr(cx-size/2), uintptr(cy-size/2), 0)
	procLineTo.Call(hdc, uintptr(cx+size/2), uintptr(cy+size/2))
	procMoveToEx.Call(hdc, uintptr(cx+size/2), uintptr(cy-size/2), 0)
	procLineTo.Call(hdc, uintptr(cx-size/2), uintptr(cy+size/2))
	procSelectObject.Call(hdc, 0)
	procDeleteObject.Call(pen)
}

// 画刷缓存（不销毁，进程内数量有限）
func toastBrush(color uint32) uintptr {
	if toastBtnBrushes == nil {
		toastBtnBrushes = map[uint32]uintptr{}
	}
	if b := toastBtnBrushes[color]; b != 0 {
		return b
	}
	b, _, _ := procCreateSolidBrush.Call(uintptr(color))
	toastBtnBrushes[color] = b
	return b
}

// 圆角矩形实心填充（NULL_PEN 无描边）
func roundRectFillGDI(hdc uintptr, r rect, radius uint32, color uint32) {
	np, _, _ := procGetStockObject.Call(8) // NULL_PEN
	procSelectObject.Call(hdc, np)
	procSelectObject.Call(hdc, toastBrush(color))
	procRoundRect.Call(hdc, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right), uintptr(r.Bottom), uintptr(radius*2), uintptr(radius*2))
}

// 浅色按钮：次按钮（描边）与主按钮（实心绿）
func drawButtonGDI(hdc uintptr, r rect, label string, primary, hot bool, dpi uint32) {
	var edge, bg uint32 = btnRejectEdgeGDI, btnRejectBgGDI
	if primary {
		bg = btnAllowBgGDI
	}
	if hot {
		if primary {
			bg = btnAllowHotGDI
		} else {
			bg, edge = btnRejectHotGDI, btnRejectHotEGDI
		}
	}
	br := uint32(scale(toastBtnR, dpi))
	roundRectFillGDI(hdc, r, br, edge)
	one := uint32(scale(1, dpi))
	roundRectFillGDI(hdc, rect{r.Left + 1, r.Top + 1, r.Right - 1, r.Bottom - 1}, br-one, bg)
	procSelectObject.Call(hdc, toastFontButton)
	txt := uintptr(0x333333)
	if primary {
		txt = 0xFFFFFF
	}
	procSetTextColor.Call(hdc, txt)
	procSetBkMode.Call(hdc, transparent)
	tr := rect{r.Left + scale(toastBtnPadX, dpi), r.Top, r.Right - scale(toastBtnPadX, dpi), r.Bottom}
	b, _ := syscall.UTF16FromString(label)
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(&b[0])), ^uintptr(0), uintptr(unsafe.Pointer(&tr)), dTSingleLine|dTNoPrefix|dTCenter|dTVCenter)
}

// 绘制浮窗（普通 GDI 窗口：兼容远程桌面/虚拟显示，ClearType 文字）
func paintToast(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	toastMu.Lock()
	title, body, actions, hover := toastTitle, toastBody, toastActions, toastHover
	toastMu.Unlock()

	dpi := getToastDpi(hwnd)
	g := toastGeometry(dpi, actions)
	one := uint32(scale(1, dpi))
	radius := uint32(scale(toastRadius, dpi))

	// 卡片：白底 + 1px 浅灰描边（圆角区域由 SetWindowRgn 裁切）
	roundRectFillGDI(hdc, g.Card, radius, toastBorderGDI)
	roundRectFillGDI(hdc, rect{g.Card.Left + 1, g.Card.Top + 1, g.Card.Right - 1, g.Card.Bottom - 1}, radius-one, toastBgGDI)

	// 图标瓦片
	tileR := uint32(scale(toastTileR, dpi))
	roundRectFillGDI(hdc, g.Tile, tileR, toastTileEdgeGDI)
	roundRectFillGDI(hdc, rect{g.Tile.Left + 1, g.Tile.Top + 1, g.Tile.Right - 1, g.Tile.Bottom - 1}, tileR-one, toastTileBgGDI)

	// 按钮底色
	for i, r := range g.Buttons {
		drawButtonGDI(hdc, r, actions[i].Label, i == len(g.Buttons)-1, hover == 2+i, dpi)
	}

	// 文字（ClearType）
	fTitle, fBody, _ := toastFonts(dpi)
	procSetBkMode.Call(hdc, transparent)

	// 标题
	procSelectObject.Call(hdc, fTitle)
	procSetTextColor.Call(hdc, 0x111111)
	tp, _ := syscall.UTF16FromString(title)
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(&tp[0])), ^uintptr(0), uintptr(unsafe.Pointer(&g.Title)), dTSingleLine|dTNoPrefix|dTEndEllipsis)

	// 正文
	procSelectObject.Call(hdc, fBody)
	procSetTextColor.Call(hdc, 0x5A5A5A)
	bp, _ := syscall.UTF16FromString(body)
	procDrawText.Call(hdc, uintptr(unsafe.Pointer(&bp[0])), ^uintptr(0), uintptr(unsafe.Pointer(&g.Body)), dTWordBreak|dTNoPrefix)

	// 鲸鱼图标（瓦片内 32px）
	if toastIcon == 0 {
		toastIcon = iconFromIco(icoBytes, 32, 32)
	}
	if toastIcon != 0 {
		isz := scale(32, dpi)
		ix := g.Tile.Left + (g.Tile.Right-g.Tile.Left-isz)/2
		iy := g.Tile.Top + (g.Tile.Bottom-g.Tile.Top-isz)/2
		procDrawIconEx.Call(hdc, uintptr(ix), uintptr(iy), toastIcon, uintptr(isz), uintptr(isz), 0, 0, diNormal)
	}

	// 关闭 ×
	crossClr := uint32(0x9A9A9A)
	if hover == 1 {
		crossClr = 0x333333
	}
	drawCross(hdc, (g.Close.Left+g.Close.Right)/2, (g.Close.Top+g.Close.Bottom)/2, scale(10, dpi), crossClr)
}

func mulDiv(a, b, c int32) int32 {
	r, _, _ := procMulDiv.Call(uintptr(a), uintptr(b), uintptr(c))
	return int32(r)
}

func jsString(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
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
	mUpdate := systray.AddMenuItem("检查并更新 DSH", "检查并更新 DSH 内核至最新版")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("退出（停止服务）", "停止服务并退出托盘")

	// 监听其他实例的"显示窗口"请求（双击桌面图标时）
	go watchShowEvent()

	// 通知浮窗线程（自绘 toast，独立消息泵）
	go toastLoop()

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
			case <-mUpdate.ClickedCh:
				go updateDshKernel()
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
	// 外部链接回调：DSH 对话中的网页链接 → Edge 打开
	w.Bind("__dshOpenExternal", func(url string) {
		openEdge(url)
	})
	// 页面通知桥：JS 端 Notification shim → Go 原生通知浮窗
	w.Bind("__dshNativeNotify", func(p notifyPayload) {
		showToast(p)
	})
	// 在页面任何脚本执行前注入 Notification shim（每次新文档自动生效）
	w.Init(notifyShimScript)

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
		// 注入缩放脚本 + 外部链接拦截脚本（页面加载后）
		time.Sleep(2500 * time.Millisecond)
		if exiting {
			return
		}
		w.Dispatch(func() { w.Eval(zoomScript + externalScript) })
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
		w.Dispatch(func() { w.Eval(zoomScript + externalScript) })
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
	cmd := exec.Command(node, bin, "web", "--port", itoa(port), "--no-open")
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

// ---------- 浏览器（默认使用 Microsoft Edge） ----------
func openBrowser() {
	openEdge(webURL)
}

// 用 Edge 打开链接（避免系统默认浏览器；找不到 Edge 时回退系统默认）
func openEdge(url string) {
	Log("Edge 打开: " + url)
	paths := []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			exec.Command(p, url).Start()
			return
		}
	}
	// 兜底：start msedge（App Paths 解析）
	cmd := exec.Command("cmd", "/c", "start", "", "msedge", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.Start()
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

func findNpm() string {
	if p := findOnPath("npm.cmd"); p != "" {
		return p
	}
	if p := findOnPath("npm.exe"); p != "" {
		return p
	}
	if p := findOnPath("npm"); p != "" {
		return p
	}
	alt := `C:\Program Files\nodejs\npm.cmd`
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	return ""
}

func findNpx() string {
	if p := findOnPath("npx.cmd"); p != "" {
		return p
	}
	if p := findOnPath("npx.exe"); p != "" {
		return p
	}
	if p := findOnPath("npx"); p != "" {
		return p
	}
	alt := `C:\Program Files\nodejs\npx.cmd`
	if _, err := os.Stat(alt); err == nil {
		return alt
	}
	return ""
}

type dshCandidate struct {
	path    string
	version string
	modTime time.Time
}

func parsePackageVersion(pkgPath string) string {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return ""
	}
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	return p.Version
}

// 语义化版本号比较 (支持 rc/beta/alpha 等预发布标签)
// 返回: 1 (v1 > v2), -1 (v1 < v2), 0 (v1 == v2)
func compareVersion(v1, v2 string) int {
	v1 = strings.TrimPrefix(strings.TrimSpace(v1), "v")
	v2 = strings.TrimPrefix(strings.TrimSpace(v2), "v")
	if v1 == v2 {
		return 0
	}
	if v1 == "" {
		return -1
	}
	if v2 == "" {
		return 1
	}

	splitPre := func(v string) (string, string) {
		if idx := strings.Index(v, "-"); idx >= 0 {
			return v[:idx], v[idx+1:]
		}
		return v, ""
	}

	main1, pre1 := splitPre(v1)
	main2, pre2 := splitPre(v2)

	parts1 := strings.Split(main1, ".")
	parts2 := strings.Split(main2, ".")
	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var n1, n2 int
		if i < len(parts1) {
			fmt.Sscanf(parts1[i], "%d", &n1)
		}
		if i < len(parts2) {
			fmt.Sscanf(parts2[i], "%d", &n2)
		}
		if n1 != n2 {
			if n1 > n2 {
				return 1
			}
			return -1
		}
	}

	// 主版本相同，处理 pre-release（有 pre-release 的版本低于无 pre-release 的正式版）
	if pre1 == "" && pre2 != "" {
		return 1
	}
	if pre1 != "" && pre2 == "" {
		return -1
	}
	if pre1 == pre2 {
		return 0
	}

	preParts1 := strings.Split(pre1, ".")
	preParts2 := strings.Split(pre2, ".")
	maxPre := len(preParts1)
	if len(preParts2) > maxPre {
		maxPre = len(preParts2)
	}
	for i := 0; i < maxPre; i++ {
		var p1, p2 string
		if i < len(preParts1) {
			p1 = preParts1[i]
		}
		if i < len(preParts2) {
			p2 = preParts2[i]
		}
		if p1 == p2 {
			continue
		}
		var num1, num2 int
		c1, _ := fmt.Sscanf(p1, "%d", &num1)
		c2, _ := fmt.Sscanf(p2, "%d", &num2)
		if c1 == 1 && c2 == 1 {
			if num1 != num2 {
				if num1 > num2 {
					return 1
				}
				return -1
			}
		} else {
			if p1 > p2 {
				return 1
			}
			return -1
		}
	}

	return 0
}

func findAllBinJsCandidates() []dshCandidate {
	var cands []dshCandidate
	seen := make(map[string]bool)

	addCand := func(jsPath string) {
		jsPath = filepath.Clean(jsPath)
		if seen[jsPath] {
			return
		}
		fi, err := os.Stat(jsPath)
		if err != nil || fi.IsDir() {
			return
		}
		seen[jsPath] = true
		pkgPath := filepath.Join(filepath.Dir(filepath.Dir(jsPath)), "package.json")
		ver := parsePackageVersion(pkgPath)
		cands = append(cands, dshCandidate{
			path:    jsPath,
			version: ver,
			modTime: fi.ModTime(),
		})
	}

	// 1. 全局 npm 路径 (如 %APPDATA%\npm\node_modules\@deepseek-ai\dsh\lib\bin.js)
	if appData := os.Getenv("APPDATA"); appData != "" {
		addCand(filepath.Join(appData, "npm", "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
		addCand(filepath.Join(appData, "pnpm", "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
	}

	// 2. 全局 pnpm / local 路径
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		addCand(filepath.Join(localAppData, "pnpm", "global", "5", "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
	}

	// 3. npx 缓存目录 (%LOCALAPPDATA%\npm-cache\_npx\*\node_modules\@deepseek-ai\dsh\lib\bin.js)
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		npxRoot := filepath.Join(localAppData, "npm-cache", "_npx")
		if dirs, err := os.ReadDir(npxRoot); err == nil {
			for _, d := range dirs {
				if d.IsDir() {
					addCand(filepath.Join(npxRoot, d.Name(), "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
				}
			}
		}
	}

	// 4. NodeJS 全局目录
	addCand(`C:\Program Files\nodejs\node_modules\@deepseek-ai\dsh\lib\bin.js`)

	// 5. 当前可执行文件同级目录
	if appDir != "" {
		addCand(filepath.Join(appDir, "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"))
	}

	return cands
}

func findBinJs() string {
	candidates := findAllBinJsCandidates()
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool {
		cmp := compareVersion(candidates[i].version, candidates[j].version)
		if cmp != 0 {
			return cmp > 0
		}
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	return candidates[0].path
}

func getInstalledDshVersion() string {
	bin := findBinJs()
	if bin == "" {
		return ""
	}
	pkgPath := filepath.Join(filepath.Dir(filepath.Dir(bin)), "package.json")
	return parsePackageVersion(pkgPath)
}

func fetchLatestDshVersion() (string, error) {
	urls := []string{
		"https://registry.npmmirror.com/@deepseek-ai/dsh/latest",
		"https://registry.npmjs.org/@deepseek-ai/dsh/latest",
	}
	client := &http.Client{Timeout: 6 * time.Second}
	var lastErr error
	for _, u := range urls {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "dsh-tiny-desktop")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		var p struct {
			Version string `json:"version"`
		}
		err = json.NewDecoder(resp.Body).Decode(&p)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if p.Version != "" {
			return p.Version, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("无法获取远端版本")
	}
	return "", lastErr
}

func cleanNpxLocks() {
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData == "" {
		return
	}
	npxRoot := filepath.Join(localAppData, "npm-cache", "_npx")
	dirs, err := os.ReadDir(npxRoot)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		lockPath := filepath.Join(npxRoot, d.Name(), "concurrency.lock")
		if _, err := os.Stat(lockPath); err == nil {
			os.RemoveAll(lockPath)
			Log("清理 npx 死锁文件: " + lockPath)
		}
	}
}

var (
	updateMu   sync.Mutex
	isUpdating bool
)

// 手动检查并更新 DSH 内核
func updateDshKernel() {
	updateMu.Lock()
	if isUpdating {
		updateMu.Unlock()
		showToast(notifyPayload{
			Title: "DSH 更新中",
			Body:  "DSH 内核更新正在进行中，请稍候...",
		})
		return
	}
	isUpdating = true
	updateMu.Unlock()

	defer func() {
		updateMu.Lock()
		isUpdating = false
		updateMu.Unlock()
	}()

	currentVer := getInstalledDshVersion()
	curText := currentVer
	if curText == "" {
		curText = "未知/未安装"
	}
	Log(fmt.Sprintf("手动检查更新: 当前本地版本 %s", curText))

	showToast(notifyPayload{
		Title: "检查 DSH 更新",
		Body:  fmt.Sprintf("当前本地版本: %s\n正在检查远端最新版本并准备更新...", curText),
	})

	latestVer, err := fetchLatestDshVersion()
	if err != nil {
		Log("获取远端最新版本提示: " + err.Error() + "，将直接执行 npm 更新")
	} else {
		Log(fmt.Sprintf("远端最新版本: %s", latestVer))
		if currentVer != "" && compareVersion(currentVer, latestVer) >= 0 {
			showToast(notifyPayload{
				Title: "DSH 已是最新版",
				Body:  fmt.Sprintf("当前版本 (v%s) 已是官方最新版本，无需更新。", currentVer),
			})
			return
		}
		showToast(notifyPayload{
			Title: "正在更新 DSH 内核",
			Body:  fmt.Sprintf("发现新版本 v%s (当前: v%s)\n正在下载安装，请稍候...", latestVer, curText),
		})
	}

	// 1. 清理可能导致卡死的 npx lockfile
	cleanNpxLocks()

	// 2. 执行安装更新命令 (优先 npm 全局安装，其次 npx)
	npm := findNpm()
	var cmd *exec.Cmd
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if npm != "" {
		Log("使用 npm 进行全局更新: " + npm + " install -g @deepseek-ai/dsh@latest")
		cmd = exec.CommandContext(ctx, npm, "install", "-g", "@deepseek-ai/dsh@latest")
	} else if npx := findNpx(); npx != "" {
		Log("使用 npx 拉取最新包: " + npx + " -y @deepseek-ai/dsh@latest --version")
		cmd = exec.CommandContext(ctx, npx, "-y", "@deepseek-ai/dsh@latest", "--version")
	} else {
		Log("系统未找到 npm 或 npx 命令")
		showToast(notifyPayload{
			Title:              "DSH 更新失败",
			Body:               "未在系统中找到 npm/npx，请确认已正确安装 Node.js 环境。",
			RequireInteraction: true,
		})
		return
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	output, err := cmd.CombinedOutput()
	if err != nil {
		Log("执行更新失败: " + err.Error() + "\n输出: " + string(output))
		showToast(notifyPayload{
			Title:              "DSH 更新失败",
			Body:               fmt.Sprintf("更新执行失败: %s\n可查看日志了解详情", err.Error()),
			RequireInteraction: true,
		})
		return
	}

	Log("更新执行成功:\n" + string(output))

	// 3. 获取更新后的版本号
	newVer := getInstalledDshVersion()
	if newVer == "" {
		if latestVer != "" {
			newVer = latestVer
		} else {
			newVer = "最新版"
		}
	}

	// 4. 若 DSH 服务正在运行，自动热重启服务以应用新版本
	if portOpen(port) || dshCmd != nil {
		Log("DSH 服务正在运行，自动重启以应用新版本")
		restartService()
		showToast(notifyPayload{
			Title: "DSH 更新完成",
			Body:  fmt.Sprintf("DSH 内核已成功更新至 v%s！\n服务已自动重启生效。", newVer),
		})
	} else {
		showToast(notifyPayload{
			Title: "DSH 更新完成",
			Body:  fmt.Sprintf("DSH 内核已成功更新至 v%s！", newVer),
		})
	}
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
