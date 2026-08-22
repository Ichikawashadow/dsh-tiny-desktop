package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/getlantern/systray"
	webview "github.com/jchv/go-webview2"
)

// TestToastMechanics：验证自绘通知浮窗 —— 窗口创建、显示、超时自动隐藏、长文本。
// 运行时会短暂在屏幕右下角出现一个深色通知卡片。
func TestToastMechanics(t *testing.T) {
	loopDone := make(chan bool)
	go func() {
		defer func() { loopDone <- true }()
		toastLoop()
	}()

	var h uintptr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		toastMu.Lock()
		h = toastHwnd
		toastMu.Unlock()
		if h != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h == 0 {
		t.Fatal("toast 窗口未创建")
	}

	if !showToast(notifyPayload{Title: "DSH 通知测试", Body: "这是一条自绘通知浮窗"}) {
		t.Fatal("showToast 返回 false")
	}
	waitVisible := func() bool {
		for i := 0; i < 50; i++ {
			r, _, _ := procIsWindowVisible.Call(h)
			if r != 0 {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	if !waitVisible() {
		t.Fatal("toast 未显示")
	}
	// 长文本 + 操作按钮（标题/正文超长）
	if !showToast(notifyPayload{
		Title:   strings.Repeat("超长标题", 40),
		Body:    strings.Repeat("长正文内容", 200),
		Actions: []notifyAction{{ID: "rejected", Label: "拒绝"}, {ID: "allowed-once", Label: "允许一次"}},
		Ref:     "a:1",
	}) {
		t.Fatal("长文本 showToast 失败")
	}
	if !waitVisible() {
		t.Fatal("长文本 toast 未显示")
	}
	// 强制触发定时器 → 自动隐藏
	procPostMessage.Call(h, wmTimer, toastTimerID, 0)
	time.Sleep(300 * time.Millisecond)
	if r, _, _ := procIsWindowVisible.Call(h); r != 0 {
		t.Error("toast 未按时隐藏")
	}

	procPostMessage.Call(h, wmClose, 0, 0)
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Error("toast 线程未退出")
	}
}

// TestToastPixels：像素级验证 GDI 绘制 —— 卡片白底、主按钮绿色
// （COLORREF 0x007FA310）、次按钮白底、图标瓦片浅灰。从窗口 DC 读取像素。
func TestToastPixels(t *testing.T) {
	loopDone := make(chan bool)
	go func() {
		defer func() { loopDone <- true }()
		toastLoop()
	}()
	var h uintptr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		toastMu.Lock()
		h = toastHwnd
		toastMu.Unlock()
		if h != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h == 0 {
		t.Fatal("toast 窗口未创建")
	}
	defer func() {
		procPostMessage.Call(h, wmClose, 0, 0)
		select {
		case <-loopDone:
		case <-time.After(5 * time.Second):
		}
	}()

	if !showToast(notifyPayload{
		Title:              "需要授权",
		Body:               "工具 bash 需要授权",
		RequireInteraction: true,
		Actions:            []notifyAction{{ID: "rejected", Label: "拒绝"}, {ID: "allowed-once", Label: "允许一次"}},
		Ref:                "a:1",
	}) {
		t.Fatal("showToast 失败")
	}
	visible := false
	for i := 0; i < 50; i++ {
		r, _, _ := procIsWindowVisible.Call(h)
		if r != 0 {
			visible = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !visible {
		t.Fatal("授权通知未显示")
	}

	dpi := getToastDpi(h)
	g := toastGeometry(dpi, []notifyAction{{ID: "rejected", Label: "拒绝"}, {ID: "allowed-once", Label: "允许一次"}})
	if len(g.Buttons) != 2 {
		t.Fatalf("按钮布局异常: %d", len(g.Buttons))
	}
	getPx := func(x, y int) uint32 {
		dc, _, _ := procGetDC.Call(h)
		if dc == 0 {
			return 0
		}
		defer procReleaseDC.Call(h, dc)
		c, _, _ := procGetPixel.Call(dc, uintptr(x), uintptr(y))
		return uint32(c)
	}
	primary := g.Buttons[1]
	reject := g.Buttons[0]
	by := int(primary.Top+primary.Bottom) / 2
	pxBtn := int(primary.Left) + 6
	pxRej := int(reject.Left) + 6
	pxCard := int(g.Card.Right) - 14
	pxCardY := int(g.Card.Top) + 30
	pxTileX, pxTileY := int(g.Tile.Left)+4, int(g.Tile.Top)+int(scale(6, dpi))

	// 等待绘制完成（主按钮出现绿色）
	green := false
	for i := 0; i < 80; i++ {
		if getPx(pxBtn, by) == 0x007FA310 {
			green = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !green {
		t.Fatalf("主按钮未绘制为绿色 0x007FA310（当前 0x%06X）", getPx(pxBtn, by))
	}
	if c := getPx(pxCard, pxCardY); c != 0xFFFFFF {
		t.Errorf("卡片应为白底 0xFFFFFF，实际 0x%06X", c)
	}
	if c := getPx(pxRej, int(reject.Top+reject.Bottom)/2); c != 0xFFFFFF {
		t.Errorf("次按钮应为白底 0xFFFFFF，实际 0x%06X", c)
	}
	if c := getPx(pxTileX, pxTileY); c != 0xF3F4F6 {
		t.Errorf("图标瓦片应为 0xF3F4F6，实际 0x%06X", c)
	}
}

// TestToastActionsAndRespond：授权场景 —— 带 actions 的通知浮窗，
// 点击「允许一次」按钮 → Go 侧把 {ref, action} 通过 Eval 回传页面 window.__dshRespond。
func TestToastActionsAndRespond(t *testing.T) {
	loopDone := make(chan bool)
	go func() {
		defer func() { loopDone <- true }()
		toastLoop()
	}()
	var h uintptr
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		toastMu.Lock()
		h = toastHwnd
		toastMu.Unlock()
		if h != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h == 0 {
		t.Fatal("toast 窗口未创建")
	}
	defer func() {
		procPostMessage.Call(h, wmClose, 0, 0)
		select {
		case <-loopDone:
		case <-time.After(5 * time.Second):
		}
	}()

	// WebView2 测试页：模拟页面侧的 __dshRespond（真实环境由注入模块定义）
	payloads := make(chan notifyPayload, 16)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	w := webview.New(false)
	if w == nil {
		t.Skip("WebView2 运行时不可用")
	}
	defer w.Destroy()
	w.SetTitle("dsh-notify-actions-test")
	w.SetSize(640, 480, webview.HintNone)
	w.Bind("__dshNativeNotify", func(p notifyPayload) {
		payloads <- p
	})
	w.Init(notifyShimScript)
	// 页面侧应答入口：文档创建时覆盖 shim 的 __dshRespond（注册顺序在 shim 之后），
	// 捕获 Go 侧回传的 {ref, action}（真实环境由桥模块的 respond 应答 pending）
	w.Init(`window.__dshRespond = function (m) {
		window.__dshNativeNotify({title:"__respond_capture__", body: JSON.stringify(m || {}), tag:"", requireInteraction:false});
	};`)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Write([]byte("<html><body><h1>actions test</h1></body></html>"))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	w.Navigate("http://" + ln.Addr().String() + "/")

	driverDone := make(chan bool)
	go func() {
		defer func() { driverDone <- true }()
		time.Sleep(6000 * time.Millisecond)
		w.Destroy()
	}()

	wvMu.Lock()
	wv = w
	wvMu.Unlock()
	defer func() {
		wvMu.Lock()
		wv = nil
		wvMu.Unlock()
	}()

	// 推送带按钮的授权通知
	if !showToast(notifyPayload{
		Title:              "需要授权",
		Body:               "工具 bash 需要授权",
		RequireInteraction: true,
		Actions:            []notifyAction{{ID: "rejected", Label: "拒绝"}, {ID: "allowed-once", Label: "允许一次"}},
		Ref:                "a:42",
	}) {
		t.Fatal("showToast 失败")
	}
	visible := false
	for i := 0; i < 50; i++ {
		r, _, _ := procIsWindowVisible.Call(h)
		if r != 0 {
			visible = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !visible {
		t.Fatal("授权通知未显示")
	}

	// 计算「允许一次」按钮（最后一个）中心点并模拟点击
	dpi := getToastDpi(h)
	g := toastGeometry(dpi, []notifyAction{{ID: "rejected", Label: "拒绝"}, {ID: "allowed-once", Label: "允许一次"}})
	if len(g.Buttons) != 2 {
		t.Fatalf("按钮布局异常: %d", len(g.Buttons))
	}
	// 关闭 × 命中测试
	if hit := toastHit(h, (g.Close.Left+g.Close.Right)/2, (g.Close.Top+g.Close.Bottom)/2); hit != 1 {
		t.Errorf("关闭区域命中测试失败: %d", hit)
	}
	primary := g.Buttons[len(g.Buttons)-1]
	toastClick(h, (primary.Left+primary.Right)/2, (primary.Top+primary.Bottom)/2)

	// 收集应答回传（与 w.Run 消息泵并发）
	captureDone := make(chan notifyPayload, 1)
	go func() {
		var got notifyPayload
		waitFor := time.Now().Add(10 * time.Second)
		for time.Now().Before(waitFor) {
			select {
			case p := <-payloads:
				if p.Title == "__respond_capture__" {
					got = p
					captureDone <- got
					return
				}
			default:
			}
			time.Sleep(100 * time.Millisecond)
		}
		captureDone <- got
	}()
	w.Run() // 泵消息循环；driver 调 Destroy 后返回
	got := <-captureDone
	<-driverDone

	if got.Title != "__respond_capture__" {
		t.Fatalf("未收到页面应答回传（缓冲 %d 条）", len(payloads))
	}
	want := `{"action":"allowed-once","ref":"a:42"}`
	if got.Body != want {
		t.Errorf("应答载荷错误: %s (期望 %s)", got.Body, want)
	}
}

// TestNotifyShimWebView：端到端回归 —— 页面里 new Notification(...) 经过 shim
// 转发到 Go 侧绑定函数；permission 恒为 granted；requestPermission 解析 granted。
// 运行时会短暂出现一个 WebView2 窗口。
func TestNotifyShimWebView(t *testing.T) {
	payloads := make(chan notifyPayload, 16)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w := webview.New(false)
	if w == nil {
		t.Skip("WebView2 运行时不可用")
	}
	defer w.Destroy()

	w.SetTitle("dsh-notify-test")
	w.SetSize(640, 480, webview.HintNone)
	w.Bind("__dshNativeNotify", func(p notifyPayload) {
		payloads <- p
	})
	w.Init(notifyShimScript)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Write([]byte("<html><body><h1>notify shim test</h1></body></html>"))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	w.Navigate("http://" + ln.Addr().String() + "/")

	driverDone := make(chan bool)
	go func() {
		defer func() { driverDone <- true }()
		time.Sleep(2500 * time.Millisecond)
		w.Dispatch(func() {
			w.Eval(`
				try {
					window.__dshNativeNotify({title:"__probe_permission__", body:String(Notification.permission), tag:"", requireInteraction:false});
					var n = new Notification("DSH 测试通知", {body:"正文内容", tag:"t1", requireInteraction:true});
					n.onclick = function () {};
					n.close();
					Notification.requestPermission().then(function (p) {
						window.__dshNativeNotify({title:"__probe_request__", body:p, tag:"", requireInteraction:false});
					});
				} catch (e) {
					window.__dshNativeNotify({title:"__probe_error__", body:String(e && e.message || e), tag:"", requireInteraction:false});
				}
			`)
		})
		time.Sleep(3000 * time.Millisecond)
		w.Destroy()
	}()

	gotCh := make(chan map[string]notifyPayload, 1)
	go func() {
		got := map[string]notifyPayload{}
		timeout := time.After(20 * time.Second)
		for len(got) < 3 {
			select {
			case p := <-payloads:
				got[p.Title] = p
			case <-timeout:
				gotCh <- got
				return
			}
		}
		gotCh <- got
	}()

	w.Run() // 泵消息循环；driver 调 Destroy 后返回
	<-driverDone
	got := <-gotCh

	if len(got) < 3 {
		t.Fatalf("通知回调不足（%d 条）: %+v", len(got), got)
	}
	if p, ok := got["__probe_permission__"]; !ok || p.Body != "granted" {
		t.Errorf("Notification.permission 应恒为 granted，实际: %+v", got["__probe_permission__"])
	}
	if p, ok := got["__probe_request__"]; !ok || p.Body != "granted" {
		t.Errorf("Notification.requestPermission() 应解析 granted，实际: %+v", got["__probe_request__"])
	}
	if p, ok := got["__probe_error__"]; ok {
		t.Fatalf("页面脚本出错: %s", p.Body)
	}
	n := got["DSH 测试通知"]
	if n.Body != "正文内容" || n.Tag != "t1" || !n.RequireInteraction {
		t.Errorf("通知载荷透传错误: %+v", n)
	}
}

// TestToastGeometryDpiScaling：回归测试——高 DPI（125%/150%/200%）下正文矩形
// 高度必须 >= 实测正文高度，否则 DrawText 会把字形下半截裁掉（半截字）。
// 96 DPI 基准下旧代码恰好不出问题，>96 DPI 必现（cardH 混用未缩放常量）。
func TestToastGeometryDpiScaling(t *testing.T) {
	body := "恢复ccf.json会话"
	toastMu.Lock()
	toastBody = body
	toastMu.Unlock()
	for _, dpi := range []uint32{96, 120, 144, 192} {
		g := toastGeometry(dpi, nil)
		bodyH := measureBodyH(body, dpi, int(g.Body.Right-g.Body.Left))
		got := int(g.Body.Bottom - g.Body.Top)
		if got < bodyH {
			t.Fatalf("dpi=%d 正文矩形高度 %d < 实测 %d（半截字回归）", dpi, got, bodyH)
		}
		// 带操作按钮时同样不能挤压正文
		g2 := toastGeometry(dpi, []notifyAction{{ID: "allowed-once", Label: "允许一次"}, {ID: "rejected", Label: "拒绝"}})
		got2 := int(g2.Body.Bottom - g2.Body.Top)
		if got2 < bodyH {
			t.Fatalf("dpi=%d 带按钮正文矩形高度 %d < 实测 %d（半截字回归）", dpi, got2, bodyH)
		}
	}
}

func TestCompareVersion(t *testing.T) {
	tests := []struct {
		v1       string
		v2       string
		expected int
	}{
		{"0.1.1-rc.2", "0.1.0-rc.8", 1},
		{"0.1.0-rc.8", "0.1.1-rc.2", -1},
		{"0.1.1-rc.2", "0.1.1-rc.2", 0},
		{"v0.1.1-rc.2", "0.1.1-rc.2", 0},
		{"0.1.1", "0.1.1-rc.2", 1},
		{"0.1.1-rc.2", "0.1.1", -1},
		{"0.1.1-rc.10", "0.1.1-rc.2", 1},
		{"0.1.1-rc.2", "0.1.1-rc.10", -1},
		{"1.0.0", "0.9.9", 1},
		{"0.9.9", "1.0.0", -1},
		{"", "0.1.0", -1},
		{"0.1.0", "", 1},
		{"", "", 0},
	}

	for _, tt := range tests {
		got := compareVersion(tt.v1, tt.v2)
		if got != tt.expected {
			t.Errorf("compareVersion(%q, %q) = %d; want %d", tt.v1, tt.v2, got, tt.expected)
		}
	}
}

func TestParsePackageVersion(t *testing.T) {
	tmpDir := t.TempDir()
	pkgFile := filepath.Join(tmpDir, "package.json")
	if err := os.WriteFile(pkgFile, []byte(`{"name":"@deepseek-ai/dsh","version":"0.1.1-rc.2"}`), 0644); err != nil {
		t.Fatal(err)
	}

	ver := parsePackageVersion(pkgFile)
	if ver != "0.1.1-rc.2" {
		t.Errorf("parsePackageVersion = %q; want 0.1.1-rc.2", ver)
	}

	if ver2 := parsePackageVersion(filepath.Join(tmpDir, "nonexistent.json")); ver2 != "" {
		t.Errorf("parsePackageVersion nonexistent = %q; want empty string", ver2)
	}
}
