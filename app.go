//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type rgb struct{ R, G, B uint8 }

func (c rgb) css() string { return fmt.Sprintf("rgb(%d,%d,%d)", c.R, c.G, c.B) }
func (c rgb) dark() bool {
	return (0.299*float64(c.R)+0.587*float64(c.G)+0.114*float64(c.B))/255 < 0.5
}

// Layout constants in CSS px (scaled by DPI). Must match ui/shell.html.
const (
	sideWidth   = 76
	stripHeight = 36
	minWidth    = 960
	minHeight   = 620
)

var (
	sideLight = rgb{0xEB, 0xEB, 0xEB}
	sideDark  = rgb{0x1A, 0x1A, 0x1A}
)

type service struct {
	id     string
	name   string
	host   uintptr // child HWND holding the WebView
	wv     *webView
	unread int
	strip  *rgb // last top colour reported by the page
	ready  bool

	// Background policy. A service you leave stays "warm" (fully running,
	// just covered) so switching back is instant and calls keep working.
	// After warmFor without use, and only when it has no call window open and
	// plays no audio, it goes "cold": rendering paused and memory trimmed.
	// It is never unloaded, so it keeps receiving messages and never reloads.
	leftAt time.Time
	cold   bool
}

// warmFor is how long a service you switched away from keeps running at full
// speed before it is paused to save memory. TWINCXT_WARM (e.g. "30s")
// overrides it for testing.
var warmFor = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TWINCXT_WARM")); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}()

// sweepTimer is the WM_TIMER id that checks which services can be paused.
const sweepTimer = 1

// testClickTimer fires the simulated click of the popup test hook.
const testClickTimer = 2

type popup struct {
	wv  *webView
	svc string
	// closing: the window is on its way out (the page closed it, or we gave
	// WhatsApp the chance to take the call back). The next WM_CLOSE destroys.
	closing bool
}

// returnCallTimer fires when WhatsApp did not take a call back in time after
// its call window was closed; the window is then removed anyway.
const returnCallTimer = 3

// moveHereScript clicks WhatsApp's own "Move here" control in the main window,
// so WhatsApp moves the call (and the camera) back itself.
const moveHereScript = `(function(){
  var re=/move here|pindahkan ke sini|pindah ke sini|mover aqu|mover para c|hierher|ici/i;
  var els=document.querySelectorAll('button,[role="button"]');
  for(var i=0;i<els.length;i++){
    var l=(els[i].getAttribute('aria-label')||'')+' '+(els[i].getAttribute('title')||'')+' '+(els[i].textContent||'');
    if(re.test(l)){els[i].click();return;}
  }
  window.chrome.webview.postMessage(JSON.stringify({type:'noMoveHere'}));
})()`

type app struct {
	cfg            config
	hwnd           uintptr
	env            *environment
	shellH         uintptr
	shell          *webView
	shellOK        bool
	svcs           map[string]*service
	settings       bool
	fullSvc        string // service currently showing a fullscreen element
	quitting       bool
	popups         map[uintptr]*popup
	upd            *updater
	locked         bool // a required update must be installed first
	appLocked      bool // the app lock screen is showing (applock.go)
	helloAvailable bool // Windows Hello can be used (hello.go)
	helloBusy      bool
	helloTried     bool // Windows Hello was offered for this lock already
	awaySince      time.Time
	spare          *spareWindow
	sparePending   bool
	popupTested    bool
	taskbarCreated uint32
}

var theApp *app

func (a *app) svc(id string) *service { return a.svcs[id] }

func (a *app) other(id string) string {
	if id == "wa" {
		return "tg"
	}
	return "wa"
}

func (a *app) serviceURL(id string) string {
	if id == "wa" {
		return "https://web.whatsapp.com/"
	}
	return "https://web.telegram.org/" + a.cfg.TelegramVersion + "/"
}

func (a *app) isDark() bool {
	switch a.cfg.Theme {
	case "dark":
		return true
	case "light":
		return false
	}
	return windowsUsesDarkApps()
}

func windowsUsesDarkApps() bool {
	var v, size uint32 = 1, 4
	r, _, _ := pRegGetValue.Call(HKEY_CURRENT_USER,
		utf16ptr(`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`),
		utf16ptr("AppsUseLightTheme"), RRF_RT_REG_DWORD, 0,
		uintptr(unsafe.Pointer(&v)), uintptr(unsafe.Pointer(&size)))
	return r == 0 && v == 0
}

// ---- window classes ---------------------------------------------------------

var (
	mainClass  = utf16("TwincxtWindow-" + instanceKey())
	hostClass  = utf16("TwincxtHost")
	popupClass = utf16("TwincxtPopup")
)

func registerClasses() {
	inst := moduleHandle()
	cursor, _, _ := pLoadCursor.Call(0, IDC_ARROW)
	big := loadAppIcon(32)
	small := loadAppIcon(16)
	for _, c := range []struct {
		name *uint16
		proc uintptr
	}{
		{mainClass, windows.NewCallback(mainWndProc)},
		{hostClass, windows.NewCallback(hostWndProc)},
		{popupClass, windows.NewCallback(popupWndProc)},
	} {
		wc := wndClassEx{
			Size:      uint32(unsafe.Sizeof(wndClassEx{})),
			WndProc:   c.proc,
			Instance:  inst,
			Icon:      big,
			IconSm:    small,
			Cursor:    cursor,
			ClassName: c.name,
		}
		pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	}
}

func (a *app) createWindow() {
	dpi := uint32(96)
	sw, _, _ := pGetSystemMetrics.Call(SM_CXSCREEN)
	sh, _, _ := pGetSystemMetrics.Call(SM_CYSCREEN)
	w, h := int32(1240), int32(800)
	x, y := (int32(sw)-w)/2, (int32(sh)-h)/2
	if ws := a.cfg.Window; ws.W >= minWidth/2 && ws.H >= minHeight/2 {
		x, y, w, h = ws.X, ws.Y, ws.W, ws.H
	}
	a.hwnd, _, _ = pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(mainClass)), utf16ptr(appName),
		WS_OVERLAPPEDWINDOW|WS_CLIPCHILDREN, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0, 0, moduleHandle(), 0)
	_ = dpi
	a.setWindowIcons(a.hwnd)
	dwmSet(a.hwnd, DWMWA_WINDOW_CORNER_PREFERENCE, 2) // round
	a.applyFrameTheme()
	// Re-run WM_NCCALCSIZE so the custom (caption-less) frame takes effect.
	pSetWindowPos.Call(a.hwnd, 0, 0, 0, 0, 0, SWP_NOMOVE|SWP_NOSIZE|SWP_NOZORDER|SWP_FRAMECHANGED)

	a.shellH = a.createHost()
	for _, s := range []*service{a.svcs["wa"], a.svcs["tg"]} {
		s.host = a.createHost()
	}
	a.startLock()
	a.layout()
	interval := time.Minute
	if warmFor < interval {
		interval = warmFor / 2
	}
	pSetTimer.Call(a.hwnd, sweepTimer, uintptr(interval.Milliseconds()), 0)
}

func (a *app) createHost() uintptr {
	h, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(hostClass)), 0,
		WS_CHILD|WS_VISIBLE|WS_CLIPSIBLINGS, 0, 0, 10, 10, a.hwnd, 0, moduleHandle(), 0)
	return h
}

func (a *app) setWindowIcons(hwnd uintptr) {
	dpi := dpiOf(hwnd)
	if ic := loadAppIcon(scale(16, dpi)); ic != 0 {
		pSendMessage.Call(hwnd, WM_SETICON, ICON_SMALL, ic)
	}
	if ic := loadAppIcon(scale(32, dpi)); ic != 0 {
		pSendMessage.Call(hwnd, WM_SETICON, ICON_BIG, ic)
	}
}

func (a *app) applyFrameTheme() {
	dark := a.isDark()
	side := sideLight
	if dark {
		side = sideDark
	}
	dwmSet(a.hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE, uint32(boolArg(dark)))
	dwmSet(a.hwnd, DWMWA_BORDER_COLOR, colorRef(side))
	for h := range a.popups {
		dwmSet(h, DWMWA_USE_IMMERSIVE_DARK_MODE, uint32(boolArg(dark)))
	}
}

// layout positions the shell (whole window) and the service WebViews (the
// content area right of the sidebar and below the title strip).
func (a *app) layout() {
	if a.hwnd == 0 {
		return
	}
	cr := clientRect(a.hwnd)
	dpi := dpiOf(a.hwnd)
	content := rect{scale(sideWidth, dpi), scale(stripHeight, dpi), cr.Right, cr.Bottom}
	pMoveWindow.Call(a.shellH, 0, 0, uintptr(cr.Right), uintptr(cr.Bottom), 1)
	for id, s := range a.svcs {
		r := content
		if a.fullSvc == id {
			r = cr
		}
		pMoveWindow.Call(s.host, uintptr(r.Left), uintptr(r.Top), uintptr(r.w()), uintptr(r.h()), 1)
		onTop := !a.settings && !a.appLocked && (id == a.cfg.Active || a.fullSvc == id)
		if onTop {
			s.leftAt = time.Time{}
			a.setCold(s, false)
			pShowWindow.Call(s.host, SW_SHOWNA)
			pSetWindowPos.Call(s.host, HWND_TOP, 0, 0, 0, 0, SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE)
			continue
		}
		// Not on screen: keep it running underneath the shell (warm) until
		// the sweep decides it can be paused.
		if s.leftAt.IsZero() {
			s.leftAt = time.Now()
		}
		if !s.cold {
			pShowWindow.Call(s.host, SW_SHOWNA)
		}
		pSetWindowPos.Call(s.host, HWND_BOTTOM, 0, 0, 0, 0, SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE)
	}
}

// setCold pauses (true) or resumes (false) a service that is out of view.
// Paused pages keep running scripts in the background, so messages and
// notifications still arrive; only rendering and memory use are reduced.
func (a *app) setCold(s *service, cold bool) {
	if s.cold == cold {
		return
	}
	s.cold = cold
	debugLog(s.id + ": cold=" + strconv.FormatBool(cold))
	if s.wv != nil {
		s.wv.SetVisible(!cold)
		if cold {
			s.wv.SetMemoryTarget(memoryLow)
		} else {
			s.wv.SetMemoryTarget(memoryNormal)
		}
		debugLog(s.id + ": memory target now " + itoa(int64(s.wv.MemoryTarget())))
	}
	if cold {
		pShowWindow.Call(s.host, SW_HIDE)
	} else {
		pShowWindow.Call(s.host, SW_SHOWNA)
	}
}

// busy reports whether a service must stay warm: it has a call/popup window
// open or is playing audio (voice call, voice note, video).
func (a *app) busy(s *service) bool {
	for _, p := range a.popups {
		if p.svc == s.id {
			return true
		}
	}
	return s.wv != nil && s.wv.PlayingAudio()
}

// sweep runs every minute and pauses services that have been out of view
// for longer than warmFor.
func (a *app) sweep() {
	for _, s := range a.svcs {
		if s.leftAt.IsZero() || s.cold || s.wv == nil {
			continue
		}
		if time.Since(s.leftAt) >= warmFor && !a.busy(s) {
			a.setCold(s, true)
		}
	}
}

// ---- main window procedure ------------------------------------------------------

func mainWndProc(hwnd uintptr, m uint32, wp, lp uintptr) uintptr {
	a := theApp
	switch m {
	case WM_NCCALCSIZE:
		if wp == 1 {
			// Drop the caption: keep Windows' invisible resize borders on the
			// left/right/bottom, and let the client area start at the very top.
			p := (*ncCalcSizeParams)(unsafe.Pointer(lp))
			top := p.Rgrc[0].Top
			r, _, _ := pDefWindowProc.Call(hwnd, uintptr(m), wp, lp)
			p.Rgrc[0].Top = top
			if isZoomed(hwnd) {
				dpi := dpiOf(hwnd)
				f, _, _ := pGetSystemMetricsForDpi.Call(SM_CYFRAME, uintptr(dpi))
				pad, _, _ := pGetSystemMetricsForDpi.Call(SM_CXPADDEDBORDER, uintptr(dpi))
				p.Rgrc[0].Top += int32(f + pad)
			}
			return r
		}
	case WM_SIZE:
		if a != nil {
			a.layout()
			a.pushState()
		}
		return 0
	case WM_MOVE, WM_MOVING:
		if a != nil {
			a.notifyMoved()
		}
	case WM_GETMINMAXINFO:
		mm := (*minMaxInfo)(unsafe.Pointer(lp))
		dpi := dpiOf(hwnd)
		mm.MinTrackSize = point{scale(minWidth, dpi), scale(minHeight, dpi)}
		return 0
	case WM_DPICHANGED:
		r := (*rect)(unsafe.Pointer(lp))
		pSetWindowPos.Call(hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.w()), uintptr(r.h()), SWP_NOZORDER|SWP_NOACTIVATE)
		if a != nil {
			a.setWindowIcons(hwnd)
		}
		return 0
	case WM_ERASEBKGND:
		if a != nil {
			c := sideLight
			if a.isDark() {
				c = sideDark
			}
			br, _, _ := pCreateSolidBrush.Call(uintptr(colorRef(c)))
			cr := clientRect(hwnd)
			pFillRect.Call(wp, uintptr(unsafe.Pointer(&cr)), br)
			pDeleteObject.Call(br)
		}
		return 1
	case WM_ACTIVATE:
		if a != nil {
			a.onActivate(loword(wp) != 0)
		}
		if loword(wp) != 0 && a != nil {
			a.focusActive()
		}
	case WM_CLOSE:
		if a != nil && !a.quitting && a.cfg.CloseAction == "tray" {
			pShowWindow.Call(hwnd, SW_HIDE)
			return 0
		}
		if a != nil {
			a.quit()
			return 0
		}
	case WM_QUERYENDSESSION:
		return 1
	case WM_ENDSESSION:
		if wp != 0 && a != nil {
			a.quit() // Windows is signing out or shutting down: save logins first
		}
		return 0
	case WM_DESTROY:
		if a != nil {
			a.removeTray()
			a.cfg.save()
		}
		pPostQuitMessage.Call(0)
		return 0
	case WM_RUNUI:
		runUIQueue()
		return 0
	case WM_TIMER:
		if a != nil && wp == sweepTimer {
			a.sweep()
		}
		if a != nil && wp == lockTimer {
			a.onLockTimer()
		}
		if a != nil && wp == updateTimer {
			a.upd.onTimer()
		}
		if a != nil && wp == installTimer {
			pKillTimer.Call(hwnd, installTimer)
			a.quit()
		}
		if a != nil && wp == returnCallTimer {
			pKillTimer.Call(hwnd, returnCallTimer)
			a.dropClosingPopups("")
		}
		if a != nil && wp == testClickTimer {
			pKillTimer.Call(hwnd, testClickTimer)
			if wa := a.svc("wa"); wa.wv != nil {
				// First a click on the "Video call" test button (must stay in
				// the main window), then on "Move to new window" (must open).
				for _, y := range []string{"100", "30"} {
					for _, t := range []string{"mousePressed", "mouseReleased"} {
						wa.wv.CallCDP("Input.dispatchMouseEvent",
							`{"type":"`+t+`","x":40,"y":`+y+`,"button":"left","clickCount":1}`, nil)
					}
				}
			}
		}
		return 0
	case WM_COPYDATA:
		cds := (*copyDataStruct)(unsafe.Pointer(lp))
		if cds.Ptr != 0 && cds.Size > 0 && a != nil {
			b := unsafe.Slice((*byte)(unsafe.Pointer(cds.Ptr)), cds.Size)
			a.activate(string(b))
		}
		return 1
	case WM_TRAY:
		if a != nil {
			switch uint32(lp) & 0xFFFF {
			case WM_LBUTTONUP:
				a.toggleWindow()
			case WM_RBUTTONUP:
				a.trayMenu()
			}
		}
		return 0
	default:
		if a != nil && a.taskbarCreated != 0 && m == a.taskbarCreated {
			a.addTray() // Explorer restarted
		}
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(m), wp, lp)
	return r
}

func hostWndProc(hwnd uintptr, m uint32, wp, lp uintptr) uintptr {
	switch m {
	case WM_SIZE:
		if a := theApp; a != nil {
			if a.shellH == hwnd && a.shell != nil {
				a.shell.FitHost()
			}
			for _, s := range a.svcs {
				if s.host == hwnd && s.wv != nil {
					s.wv.FitHost()
				}
			}
		}
		return 0
	case WM_ERASEBKGND:
		return 1
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(m), wp, lp)
	return r
}

func (a *app) notifyMoved() {
	if a.shell != nil {
		a.shell.NotifyMoved()
	}
	for _, s := range a.svcs {
		if s.wv != nil {
			s.wv.NotifyMoved()
		}
	}
}

func (a *app) focusActive() {
	if a.settings || a.appLocked {
		if a.shell != nil {
			a.shell.Focus()
			if a.appLocked && a.shellOK {
				// Caret in the PIN / password field, ready to type.
				a.shell.Eval("twincxt.focusLock && twincxt.focusLock()")
			}
		}
		return
	}
	if s := a.svc(a.cfg.Active); s != nil && s.wv != nil {
		s.wv.Focus()
	}
}

func (a *app) showWindow() {
	if isIconic(a.hwnd) {
		pShowWindow.Call(a.hwnd, SW_RESTORE)
	} else if a.cfg.Window.Maximized && !isVisible(a.hwnd) {
		pShowWindow.Call(a.hwnd, SW_MAXIMIZE)
	} else {
		pShowWindow.Call(a.hwnd, SW_SHOW)
	}
	pSetForegroundWindow.Call(a.hwnd)
	a.focusActive()
}

func (a *app) toggleWindow() {
	fg, _, _ := pGetForegroundWindow.Call()
	if isVisible(a.hwnd) && !isIconic(a.hwnd) && fg == a.hwnd {
		pShowWindow.Call(a.hwnd, SW_HIDE)
		return
	}
	a.showWindow()
}

// restart relaunches Twincxt after the WebView2 browser process died (every
// WebView is unusable then). Skipped right after startup to avoid a loop.
func (a *app) restart() {
	if time.Since(startedAt) < time.Minute {
		return
	}
	releaseSingleInstance()
	if exe, err := os.Executable(); err == nil {
		_ = exec.Command(exe).Start()
	}
	a.quit()
}

// quit closes Twincxt without losing logins: the window disappears at once,
// then every WebView is closed and Twincxt waits for the WebView2 browser
// process to finish writing cookies, local storage and databases to disk
// before exiting. Exiting straight away could drop a login that was saved
// only moments earlier (e.g. right after scanning the QR code).
func (a *app) quit() {
	if a.quitting {
		return
	}
	a.quitting = true
	debugLog("quit: start")
	a.saveWindow()
	pShowWindow.Call(a.hwnd, SW_HIDE)
	a.removeTray()

	var pid uint32
	closeWV := func(wv *webView) {
		if wv == nil {
			return
		}
		if pid == 0 {
			pid = wv.BrowserPID()
		}
		wv.Close()
		wv.Release()
	}
	for h, p := range a.popups {
		if p.wv != nil {
			closeWV(p.wv)
			p.wv = nil
		}
		pDestroyWindow.Call(h)
	}
	if a.spare != nil {
		closeWV(a.spare.wv)
		pDestroyWindow.Call(a.spare.hwnd)
		a.spare = nil
	}
	for _, s := range a.svcs {
		closeWV(s.wv)
		s.wv = nil
	}
	closeWV(a.shell)
	a.shell, a.shellOK = nil, false
	if a.env != nil {
		comRelease(a.env.ptr)
		a.env = nil
	}
	debugLog("quit: webviews closed, waiting for browser process " + itoa(int64(pid)))
	t0 := time.Now()
	waitForProcessExit(pid, 8*time.Second)
	debugLog("quit: browser process done after " + time.Since(t0).Round(10*time.Millisecond).String())
	pDestroyWindow.Call(a.hwnd)
}

func (a *app) saveWindow() {
	var wp windowPlacement
	wp.Length = uint32(unsafe.Sizeof(wp))
	if r, _, _ := pGetWindowPlacement.Call(a.hwnd, uintptr(unsafe.Pointer(&wp))); r != 0 {
		n := wp.NormalPosition
		a.cfg.Window = windowState{X: n.Left, Y: n.Top, W: n.w(), H: n.h(), Maximized: wp.ShowCmd == SW_MAXIMIZE}
	}
	a.cfg.save()
}

// activate handles "twincxt:open/wa" style requests from toasts and from a
// second launch of the exe.
func (a *app) activate(arg string) {
	arg = strings.TrimSpace(arg)
	switch {
	case strings.Contains(arg, "open/wa"):
		a.selectService("wa", "notification")
	case strings.Contains(arg, "open/tg"):
		a.selectService("tg", "notification")
	case strings.Contains(arg, protocolName+":settings"):
		a.settings = true
		a.layout()
		a.pushState()
		a.applyStrip()
	}
	a.showWindow()
}

// ---- shell ------------------------------------------------------------------

type shellMsg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func (a *app) onShellMessage(raw string) {
	debugLog("shell: " + raw)
	var m shellMsg
	if json.Unmarshal([]byte(raw), &m) != nil {
		return
	}
	str := func() string { var s string; _ = json.Unmarshal(m.Data, &s); return s }
	switch m.Type {
	case "ready":
		a.shellOK = true
		a.pushState()
		if fg, _, _ := pGetForegroundWindow.Call(); a.appLocked && fg == a.hwnd {
			a.focusActive() // the lock screen takes the keyboard right away
			a.autoHello()
		}
		for _, s := range a.svcs {
			a.pushUnread(s)
			a.shell.Eval(fmt.Sprintf("twincxt.loading(%q,%v)", s.id, !s.ready))
		}
		a.applyStrip()
	case "select":
		a.selectService(str(), "sidebar")
	case "settings":
		var on bool
		_ = json.Unmarshal(m.Data, &on)
		a.settings = on || a.locked
		a.layout()
		a.applyStrip()
		a.focusActive()
	case "zoom":
		var z struct {
			Svc   string `json:"svc"`
			Value int    `json:"value"`
		}
		_ = json.Unmarshal(m.Data, &z)
		a.setZoom(z.Svc, clampZoom(z.Value))
	case "set":
		var kv struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		_ = json.Unmarshal(m.Data, &kv)
		a.setOption(kv.Key, kv.Value)
	case "lock":
		a.onLockMessage(m.Data)
	case "action":
		a.runAction(str())
	case "window":
		a.windowCommand(str())
	case "update":
		switch str() {
		case "check":
			a.upd.check(true)
		case "install":
			a.upd.install()
		case "skip":
			a.upd.skip()
		}
	}
}

func (a *app) windowCommand(cmd string) {
	switch cmd {
	case "min":
		pShowWindow.Call(a.hwnd, SW_MINIMIZE)
	case "max":
		if isZoomed(a.hwnd) {
			pShowWindow.Call(a.hwnd, SW_RESTORE)
		} else {
			pShowWindow.Call(a.hwnd, SW_MAXIMIZE)
		}
	case "close":
		pPostMessage.Call(a.hwnd, WM_CLOSE, 0, 0)
	case "drag":
		pReleaseCapture.Call()
		pPostMessage.Call(a.hwnd, WM_NCLBUTTONDOWN, HTCAPTION, 0)
	case "resizeTop":
		if !isZoomed(a.hwnd) {
			pReleaseCapture.Call()
			pPostMessage.Call(a.hwnd, WM_NCLBUTTONDOWN, HTTOP, 0)
		}
	}
}

func (a *app) pushState() {
	if !a.shellOK {
		return
	}
	st := map[string]any{
		"active":          a.cfg.Active,
		"theme":           a.cfg.Theme,
		"zoom":            map[string]int{"wa": a.cfg.ZoomWA, "tg": a.cfg.ZoomTG},
		"autostart":       a.cfg.Autostart,
		"startHidden":     a.cfg.StartHidden,
		"closeAction":     a.cfg.CloseAction,
		"notifyWa":        a.cfg.NotifyWA,
		"notifyTg":        a.cfg.NotifyTG,
		"telegramVersion": a.cfg.TelegramVersion,
		"notifyMode":      a.cfg.NotifyMode,
		"autoUpdate":      a.cfg.AutoUpdate,
		"blurMessages":    a.cfg.BlurMessages,
		"blurPreviews":    a.cfg.BlurPreviews,
		"blurNames":       a.cfg.BlurNames,
		"blurPhotos":      a.cfg.BlurPhotos,
		"blurMedia":       a.cfg.BlurMedia,
		"lockEnabled":     a.cfg.LockEnabled,
		"locked":          a.appLocked,
		"lockAfter":       a.cfg.LockAfter,
		"lockType":        a.cfg.LockType,
		"lockBiometric":   a.cfg.LockBiometric,
		"biometric":       map[string]any{"available": a.helloAvailable, "name": "Windows Hello"},
		"platform":        "windows",
		"version":         appVersion,
		"maximized":       isZoomed(a.hwnd),
		"settingsOpen":    a.settings,
	}
	b, _ := json.Marshal(st)
	a.shell.Eval("twincxt.state(" + string(b) + ")")
}

// pushPrivacy hands the blur settings to a service page (ui/service.js keeps
// them in the site's localStorage so the next load is blurred from the start).
func (a *app) pushPrivacy(s *service) {
	if s == nil || s.wv == nil {
		return
	}
	b, _ := json.Marshal(map[string]bool{
		"blurMessages": a.cfg.BlurMessages,
		"blurPreviews": a.cfg.BlurPreviews,
		"blurNames":    a.cfg.BlurNames,
		"blurPhotos":   a.cfg.BlurPhotos,
		"blurMedia":    a.cfg.BlurMedia,
	})
	s.wv.Eval("window.twincxtPrivacy && window.twincxtPrivacy(" + string(b) + ")")
}

func (a *app) pushUnread(s *service) {
	if a.shellOK {
		a.shell.Eval(fmt.Sprintf("twincxt.unread(%q,%d)", s.id, s.unread))
	}
}

// applyStrip colours the title strip like the top of the active page so the
// two read as one surface.
func (a *app) applyStrip() {
	if !a.shellOK {
		return
	}
	s := a.svc(a.cfg.Active)
	if a.settings || a.appLocked || s == nil || s.strip == nil {
		a.shell.Eval("twincxt.strip('', '')")
		return
	}
	fg := "#3B4A54"
	if s.strip.dark() {
		fg = "#D1D7DB"
	}
	a.shell.Eval(fmt.Sprintf("twincxt.strip(%q,%q)", s.strip.css(), fg))
}

func (a *app) selectService(id, source string) {
	if a.svc(id) == nil {
		return
	}
	if a.locked {
		a.settings = true
		a.layout()
		a.pushState()
		return
	}
	debugLog("select " + id + " by " + source)
	a.cfg.Active = id
	a.settings = false
	a.layout()
	a.pushState()
	a.applyStrip()
	a.focusActive()
	a.cfg.save()
}

func (a *app) setZoom(id string, z int) {
	if id == "wa" {
		a.cfg.ZoomWA = z
	} else {
		a.cfg.ZoomTG = z
	}
	if s := a.svc(id); s != nil && s.wv != nil {
		s.wv.SetZoom(float64(z) / 100)
	}
	a.cfg.save()
}

func (a *app) setOption(key string, raw json.RawMessage) {
	var b bool
	var s string
	_ = json.Unmarshal(raw, &b)
	_ = json.Unmarshal(raw, &s)
	switch key {
	case "theme":
		a.cfg.Theme = s
		a.applyTheme()
	case "autostart":
		setAutostart(b)
		a.cfg.Autostart = autostartEnabled()
	case "startHidden":
		a.cfg.StartHidden = b
	case "closeAction":
		a.cfg.CloseAction = s
	case "notifyMode":
		if s == "toast" || s == "attention" || s == "both" {
			a.cfg.NotifyMode = s
		}
	case "autoUpdate":
		a.cfg.AutoUpdate = b
		if b {
			a.upd.check(false)
		}
	case "blurMessages", "blurPreviews", "blurNames", "blurPhotos", "blurMedia":
		switch key {
		case "blurMessages":
			a.cfg.BlurMessages = b
		case "blurPreviews":
			a.cfg.BlurPreviews = b
		case "blurNames":
			a.cfg.BlurNames = b
		case "blurPhotos":
			a.cfg.BlurPhotos = b
		case "blurMedia":
			a.cfg.BlurMedia = b
		}
		for _, s := range []*service{a.svc("wa"), a.svc("tg")} {
			a.pushPrivacy(s)
		}
	case "lockBiometric":
		a.cfg.LockBiometric = b
	case "lockAfter":
		n, err := strconv.Atoi(s)
		if err != nil {
			_ = json.Unmarshal(raw, &n)
		}
		if n == 0 || n == 1 || n == 5 || n == 15 {
			a.cfg.LockAfter = n
		}
	case "notifyWa":
		a.cfg.NotifyWA = b
	case "notifyTg":
		a.cfg.NotifyTG = b
	case "telegramVersion":
		if s != a.cfg.TelegramVersion && (s == "a" || s == "k") {
			a.cfg.TelegramVersion = s
			if tg := a.svc("tg"); tg.wv != nil {
				tg.wv.Navigate(a.serviceURL("tg"))
			}
		}
	}
	a.cfg.save()
	a.pushState()
}

func (a *app) applyTheme() {
	scheme := 0
	switch a.cfg.Theme {
	case "light":
		scheme = 1
	case "dark":
		scheme = 2
	}
	if a.shell != nil {
		a.shell.SetColorScheme(scheme)
	}
	a.applyFrameTheme()
	pInvalidateRect.Call(a.hwnd, 0, 1)
}

func (a *app) runAction(action string) {
	parts := strings.SplitN(action, ":", 2)
	if len(parts) != 2 {
		return
	}
	s := a.svc(parts[1])
	if s == nil || s.wv == nil {
		return
	}
	switch parts[0] {
	case "reload":
		s.wv.Navigate(a.serviceURL(s.id))
	case "reset":
		u, _ := url.Parse(a.serviceURL(s.id))
		origin := u.Scheme + "://" + u.Host
		params, _ := json.Marshal(map[string]string{"origin": origin, "storageTypes": "all"})
		s.wv.CallCDP("Storage.clearDataForOrigin", string(params), func() {
			s.unread = 0
			a.pushUnread(s)
			s.wv.Navigate(a.serviceURL(s.id))
		})
	}
}

// ---- services -----------------------------------------------------------------

var unreadRe = regexp.MustCompile(`\((\d+)\)`)

func (a *app) setupService(s *service, wv *webView) {
	s.wv = wv
	wv.FitHost()
	bg := rgb{0xFF, 0xFF, 0xFF}
	if a.isDark() {
		bg = rgb{0x16, 0x17, 0x17}
	}
	wv.SetBackground(bg)
	a.configureWebView(wv, s.id)
	wv.AddInitScript(serviceScript(s.id))
	if s.id == "wa" {
		wv.SetZoom(float64(a.cfg.ZoomWA) / 100)
	} else {
		wv.SetZoom(float64(a.cfg.ZoomTG) / 100)
	}

	wv.OnMessage(func(raw string) { a.onServiceMessage(s, raw) })
	wv.OnTitleChanged(func(title string) {
		n := 0
		if m := unreadRe.FindStringSubmatch(title); m != nil {
			n, _ = strconv.Atoi(m[1])
		}
		if n != s.unread {
			s.unread = n
			a.pushUnread(s)
			a.updateTray()
		}
	})
	wv.OnNavigationCompleted(func() {
		a.pushPrivacy(s)
		// Test hook (TWINCXT_TEST_POPUP=1): one popup opened by the page on
		// its own (must be blocked) and one opened from a real click on a test
		// button (must open, with content written right after window.open).
		if s.id == "wa" && os.Getenv("TWINCXT_TEST_POPUP") != "" && !a.popupTested {
			a.popupTested = true
			wv.Eval(`(function(){
  function open(t){var w=window.open('','twincxt-'+t,'width=520,height=360');
    if(!w){console.log('blocked '+t);return;}
    window.__tw=w;
    var m=document.createElement('button');m.textContent='Move here';
    m.style.cssText='position:fixed;left:0;top:140px;width:120px;height:40px;z-index:2147483647';
    m.onclick=function(){var w=window.__tw;if(!w)return;window.chrome.webview.postMessage(JSON.stringify({type:'testvis',v:w.document.visibilityState}));setTimeout(function(){window.chrome.webview.postMessage(JSON.stringify({type:'testvis',v:w.document.visibilityState+' (after 500ms)'}));w.close();},500);};document.documentElement.appendChild(m);
    w.document.title='Popup '+t;w.document.body.innerHTML='<h1 style="font:600 34px Segoe UI;margin:40px">POPUP '+t+' OK</h1>';}
  var b=document.createElement('button');b.setAttribute('aria-label','Move to new window');b.textContent='test';
  b.style.cssText='position:fixed;left:0;top:0;width:120px;height:60px;z-index:2147483647';
  b.onclick=function(){open('click')};document.documentElement.appendChild(b);
  var c=document.createElement('button');c.setAttribute('aria-label','Video call');c.textContent='call';
  c.style.cssText='position:fixed;left:0;top:70px;width:120px;height:60px;z-index:2147483647';
  c.onclick=function(){open('callstart')};document.documentElement.appendChild(c);
  setTimeout(function(){open('auto')},9000);
})()`)
			pSetTimer.Call(a.hwnd, testClickTimer, 14000, 0)
		}
		s.ready = true
		if a.shellOK {
			a.shell.Eval(fmt.Sprintf("twincxt.loading(%q,false)", s.id))
		}
	})
	wv.OnProcessFailed(func(kind int32) {
		debugLog(s.id + ": process failed, kind " + itoa(int64(kind)))
		switch kind {
		case procRenderExited:
			wv.Reload()
		case procBrowserExited:
			a.restart()
		}
	})
	// Loading indicator on the sidebar half for real page loads.
	wv.OnNavigationStarting(func(uri string) bool {
		if trustedURL(uri) && a.shellOK {
			s.ready = false
			a.shell.Eval(fmt.Sprintf("twincxt.loading(%q,true)", s.id))
		}
		return true
	})
	wv.OnFullScreenChanged(func(on bool) {
		if on {
			a.fullSvc = s.id
			if !isZoomed(a.hwnd) {
				pShowWindow.Call(a.hwnd, SW_MAXIMIZE)
			}
		} else {
			a.fullSvc = ""
		}
		a.layout()
	})
	wv.Navigate(a.serviceURL(s.id))
	a.layout()
	a.prepareSpare()
}

// configureWebView applies settings shared by service WebViews and popups.
func (a *app) configureWebView(wv *webView, svcID string) {
	wv.ApplySettings(webViewSettings{userAgent: a.userAgent(), contextMenus: true})
	wv.OnContextMenu(browserMenuItems)
	wv.OnPermission(func(uri string, kind int32) int32 {
		if !trustedURL(uri) {
			return permDeny
		}
		if kind == permNotifications {
			// Pages get notifications through our native bridge instead, so
			// Edge-style notifications never appear.
			return permDeny
		}
		return permAllow
	})
	wv.OnNavigationStarting(func(uri string) bool {
		if trustedURL(uri) || !strings.HasPrefix(uri, "http") {
			return true
		}
		shellOpen(uri)
		return false
	})
	wv.OnNewWindow(func(req *newWindowRequest) {
		if trustedURL(req.URI) || req.URI == "" || strings.HasPrefix(req.URI, "about:") {
			debugLog("popup request: " + req.URI + " userInitiated=" + strconv.FormatBool(req.UserInitiated()))
			if !req.UserInitiated() {
				// Same as a browser's popup blocker: windows the page opens on
				// its own are refused, so a call stays in the main window until
				// you click "Move to new window".
				debugLog("popup: blocked, not opened by a click: " + req.URI)
				req.Handled()
				return
			}
			a.openPopup(req, svcID)
			return
		}
		if strings.HasPrefix(req.URI, "http") || strings.HasPrefix(req.URI, "mailto:") || strings.HasPrefix(req.URI, "tel:") {
			shellOpen(req.URI)
		}
		req.Handled()
	})
	wv.OnDownload(func(path string) {
		name := "Twincxt"
		if s := a.svc(svcID); s != nil {
			name = s.name
		}
		watchDownload(path, name, serviceIconPath(svcID))
	})
	wv.OnAcceleratorKey(a.handleShortcut)
}

var browserMenuItems = map[string]bool{
	"back": true, "forward": true, "reload": true, "saveAs": true, "print": true,
	"createQrCode": true, "inspectElement": true, "share": true, "webCapture": true,
	"openLinkInNewWindow": true, "saveLinkAs": true, "saveImageAs": true, "saveMediaAs": true,
	"copyLinkToHighlight": true, "readAloud": true, "translate": true, "searchWebFor": true,
}

func trustedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, d := range []string{"whatsapp.com", "whatsapp.net", "telegram.org"} {
		if h == d || strings.HasSuffix(h, "."+d) {
			return true
		}
	}
	return false
}

func (a *app) userAgent() string {
	v := "130.0.0.0"
	if a.env != nil && a.env.version != "" {
		if i := strings.IndexByte(a.env.version, '.'); i > 0 {
			v = a.env.version[:i] + ".0.0.0"
		}
	}
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" + v + " Safari/537.36"
}

type serviceMsg struct {
	Type  string `json:"type"`
	Title string `json:"title"`
	Body  string `json:"body"`
	R     int    `json:"r"`
	G     int    `json:"g"`
	B     int    `json:"b"`
	Label string `json:"label"`
}

func (a *app) onServiceMessage(s *service, raw string) {
	debugLog(s.id + ": " + raw)
	var m serviceMsg
	if json.Unmarshal([]byte(raw), &m) != nil {
		return
	}
	switch m.Type {
	case "frame":
		c := rgb{uint8(m.R), uint8(m.G), uint8(m.B)}
		if s.strip == nil || *s.strip != c {
			s.strip = &c
			if s.id == a.cfg.Active {
				a.applyStrip()
			}
		}
	case "notify":
		a.notify(s, m.Title, m.Body)
	case "noMoveHere":
		debugLog(s.id + ": no Move here button found, closing the call window")
		a.dropClosingPopups(s.id)
	case "popupBlocked":
		debugLog(s.id + ": call window kept in main window (last click: " + m.Label + ")")
	}
}

func (a *app) notify(s *service, title, body string) {
	if (s.id == "wa" && !a.cfg.NotifyWA) || (s.id == "tg" && !a.cfg.NotifyTG) {
		return
	}
	fg, _, _ := pGetForegroundWindow.Call()
	focused := fg == a.hwnd && isVisible(a.hwnd) && !isIconic(a.hwnd)
	if focused && s.id == a.cfg.Active && !a.settings {
		return // the user is already looking at it
	}
	if title == "" {
		title = s.name
	}
	if a.appLocked {
		// Locked: say that something arrived, not what.
		title, body = s.name, "New message"
	}
	t := toast{
		Title:  title,
		Body:   body,
		Launch: protocolName + ":open/" + s.id,
		Icon:   serviceIconPath(s.id),
		Silent: true, // the pages play their own notification sound
	}
	if a.cfg.NotifyMode != "attention" {
		t.show()
	}
	// "Flash taskbar": the taskbar button flashes until Twincxt is opened.
	if a.cfg.NotifyMode != "toast" && !focused {
		fw := flashWInfo{Hwnd: a.hwnd, Flags: FLASHW_TRAY | FLASHW_TIMERNOFG}
		fw.Size = uint32(unsafe.Sizeof(fw))
		pFlashWindowEx.Call(uintptr(unsafe.Pointer(&fw)))
	}
}

// handleShortcut implements app shortcuts while any WebView has focus.
func (a *app) handleShortcut(vk uint32) bool {
	ctrl, alt, shift := keyDown(VK_CONTROL), keyDown(VK_MENU), keyDown(VK_SHIFT)
	if ctrl && !alt && !shift && vk == 'L' {
		a.lockNow()
		return true
	}
	if a.appLocked {
		return (alt && (vk == '1' || vk == '2')) || (ctrl && (vk == VK_TAB || vk == VK_OEM_COMMA))
	}
	switch {
	case alt && !ctrl && vk == '1':
		a.selectService("wa", "Alt+1")
	case alt && !ctrl && vk == '2':
		a.selectService("tg", "Alt+2")
	case ctrl && !alt && vk == VK_TAB:
		a.selectService(a.other(a.cfg.Active), "Ctrl+Tab")
	case ctrl && !alt && !shift && vk == VK_OEM_COMMA:
		a.settings = !a.settings
		a.layout()
		a.pushState()
		a.applyStrip()
		a.focusActive()
	default:
		return false
	}
	return true
}

// ---- popups (call windows etc.) ----------------------------------------------

// A popup WebView is kept ready in a hidden window so a window.open() request
// (e.g. WhatsApp's call window) can be answered synchronously.
type spareWindow struct {
	hwnd uintptr
	wv   *webView
}

func (a *app) createPopupWindow(title string) uintptr {
	dpi := dpiOf(a.hwnd)
	w, h := scale(960, dpi), scale(680, dpi)
	hwnd, _, _ := pCreateWindowEx.Call(0, uintptr(unsafe.Pointer(popupClass)), utf16ptr(title),
		WS_OVERLAPPEDWINDOW|WS_CLIPCHILDREN, 0, 0, uintptr(w), uintptr(h), 0, 0, moduleHandle(), 0)
	a.setWindowIcons(hwnd)
	dark := a.isDark()
	dwmSet(hwnd, DWMWA_USE_IMMERSIVE_DARK_MODE, uint32(boolArg(dark)))
	if dark {
		dwmSet(hwnd, DWMWA_CAPTION_COLOR, colorRef(rgb{0x16, 0x17, 0x17}))
	}
	return hwnd
}

// prepareSpare creates the next ready-to-use popup WebView in the background.
func (a *app) prepareSpare() {
	if a.env == nil || a.spare != nil || a.sparePending || a.quitting {
		return
	}
	a.sparePending = true
	hwnd := a.createPopupWindow(appName)
	a.env.createController(hwnd, func(wv *webView) {
		a.sparePending = false
		if wv == nil || a.quitting {
			if wv != nil {
				wv.Close()
				wv.Release()
			}
			pDestroyWindow.Call(hwnd)
			return
		}
		bg := rgb{0xFF, 0xFF, 0xFF}
		if a.isDark() {
			bg = rgb{0x16, 0x17, 0x17}
		}
		wv.SetBackground(bg)
		wv.FitHost()
		wv.SetVisible(false) // hidden until handed to a page
		a.configureWebView(wv, "wa")
		wv.AddInitScript(popupScript("wa"))
		wv.OnTitleChanged(func(t string) {
			if t != "" {
				pSetWindowText.Call(hwnd, utf16ptr(t))
			}
		})
		wv.OnWindowCloseRequested(func() {
			// The page closed its window (e.g. "Move here", call ended).
			debugLog("popup: closed by the page")
			if p := a.popups[hwnd]; p != nil {
				p.closing = true
			}
			pShowWindow.Call(hwnd, SW_HIDE) // disappear at once; cleanup follows
			pPostMessage.Call(hwnd, WM_CLOSE, 0, 0)
		})
		a.spare = &spareWindow{hwnd: hwnd, wv: wv}
		debugLog("popup: spare ready")
	})
}

func (a *app) openPopup(req *newWindowRequest, svcID string) {
	sp := a.spare
	if sp == nil {
		// Nothing ready: let WebView2 open its own window. It looks plainer,
		// but its content always works.
		debugLog("popup: no spare ready, default window for " + req.URI)
		a.prepareSpare()
		return
	}
	a.spare = nil
	debugLog("popup: " + svcID + " " + req.URI)
	a.popups[sp.hwnd] = &popup{svc: svcID, wv: sp.wv}
	req.Attach(sp.wv)

	title := "WhatsApp"
	if svcID == "tg" {
		title = "Telegram"
	}
	pSetWindowText.Call(sp.hwnd, utf16ptr(title))
	dpi := dpiOf(a.hwnd)
	w, h := scale(960, dpi), scale(680, dpi)
	if fw, fh := req.Size(); fw > 0 {
		// The page gives the content size; add room for the window frame.
		w, h = scale(fw+16, dpi), scale(fh+39, dpi)
	}
	var wr rect
	pGetWindowRect.Call(a.hwnd, uintptr(unsafe.Pointer(&wr)))
	x, y := wr.Left+(wr.w()-w)/2, wr.Top+(wr.h()-h)/2
	pSetWindowPos.Call(sp.hwnd, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), SWP_NOZORDER)
	pShowWindow.Call(sp.hwnd, SW_SHOW)
	// The spare was created in a hidden window; WebView2 does not start
	// painting on its own when that window appears, so switch it on here.
	sp.wv.FitHost()
	sp.wv.SetVisible(true)
	sp.wv.NotifyMoved()
	sp.wv.Focus()
	a.prepareSpare() // ready for the next one
}

func popupWndProc(hwnd uintptr, m uint32, wp, lp uintptr) uintptr {
	a := theApp
	switch m {
	case WM_SIZE:
		if p := a.popups[hwnd]; p != nil && p.wv != nil {
			wv := p.wv
			wv.FitHost()
		}
		return 0
	case WM_MOVE:
		if p := a.popups[hwnd]; p != nil && p.wv != nil {
			wv := p.wv
			wv.NotifyMoved()
		}
	case WM_CLOSE:
		// You closed the call window: hide it at once and let WhatsApp move
		// the call back to the main window (its "Move here"), which also hands
		// the camera back. Destroyed when the page closes it, or after 3 s.
		if p := a.popups[hwnd]; p != nil && !p.closing && p.svc == "wa" {
			if wa := a.svc("wa"); wa != nil && wa.wv != nil {
				p.closing = true
				// Make it invisible instead of hiding it: a hidden window counts
				// as "not shown", which pauses its video and makes WhatsApp
				// restart the camera. Fully transparent and click-through, it
				// is gone for you while WhatsApp moves the call as smoothly as
				// with its own "Move here" button.
				makeInvisible(hwnd)
				debugLog("popup: closed by you, asking WhatsApp to move the call back")
				wa.wv.Eval(moveHereScript)
				pSetTimer.Call(a.hwnd, returnCallTimer, 3000, 0)
				return 0
			}
		}
	case WM_DESTROY:
		if p := a.popups[hwnd]; p != nil && p.wv != nil {
			wv := p.wv
			wv.Close()
		}
		delete(a.popups, hwnd)
		return 0
	}
	r, _, _ := pDefWindowProc.Call(hwnd, uintptr(m), wp, lp)
	return r
}

// dropClosingPopups removes call windows that are already hidden and closing.
func (a *app) dropClosingPopups(svc string) {
	for h, p := range a.popups {
		if p.closing && (svc == "" || p.svc == svc) {
			debugLog("popup: removed")
			pDestroyWindow.Call(h)
		}
	}
}

// ---- tray ----------------------------------------------------------------------

func (a *app) trayData() notifyIconData {
	d := notifyIconData{Wnd: a.hwnd, ID: 1, Flags: NIF_MESSAGE | NIF_ICON | NIF_TIP, CallbackMessage: WM_TRAY}
	d.Size = uint32(unsafe.Sizeof(d))
	d.Icon = loadAppIcon(scale(16, dpiOf(a.hwnd)))
	tip := appName
	if total := a.svcs["wa"].unread + a.svcs["tg"].unread; total > 0 {
		tip = fmt.Sprintf("%s · %d unread", appName, total)
	}
	copy(d.Tip[:len(d.Tip)-1], windows.StringToUTF16(tip))
	return d
}

func (a *app) addTray() {
	d := a.trayData()
	pShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&d)))
}

func (a *app) updateTray() {
	d := a.trayData()
	pShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&d)))
}

func (a *app) removeTray() {
	d := a.trayData()
	pShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&d)))
}

func (a *app) trayMenu() {
	const (
		cmdOpen = iota + 1
		cmdWA
		cmdTG
		cmdSettings
		cmdQuit
		cmdLock
	)
	menu, _, _ := pCreatePopupMenu.Call()
	label := func(name string, n int) string {
		if n > 0 {
			return fmt.Sprintf("%s\t%d", name, n)
		}
		return name
	}
	pAppendMenu.Call(menu, MF_STRING, cmdOpen, utf16ptr("Open "+appName))
	pAppendMenu.Call(menu, MF_SEPARATOR, 0, 0)
	pAppendMenu.Call(menu, MF_STRING, cmdWA, utf16ptr(label("WhatsApp", a.svcs["wa"].unread)))
	pAppendMenu.Call(menu, MF_STRING, cmdTG, utf16ptr(label("Telegram", a.svcs["tg"].unread)))
	pAppendMenu.Call(menu, MF_SEPARATOR, 0, 0)
	if a.cfg.LockEnabled && !a.appLocked {
		pAppendMenu.Call(menu, MF_STRING, cmdLock, utf16ptr("Lock"))
	}
	pAppendMenu.Call(menu, MF_STRING, cmdSettings, utf16ptr("Settings"))
	pAppendMenu.Call(menu, MF_STRING, cmdQuit, utf16ptr("Quit"))
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(a.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(menu, TPM_RETURNCMD|TPM_RIGHTBUTTON|TPM_BOTTOMALIGN, uintptr(pt.X), uintptr(pt.Y), 0, a.hwnd, 0)
	pPostMessage.Call(a.hwnd, WM_NULL, 0, 0)
	pDestroyMenu.Call(menu)
	switch cmd {
	case cmdOpen:
		a.showWindow()
	case cmdWA:
		a.selectService("wa", "tray")
		a.showWindow()
	case cmdTG:
		a.selectService("tg", "tray")
		a.showWindow()
	case cmdSettings:
		a.settings = true
		a.layout()
		a.pushState()
		a.applyStrip()
		a.showWindow()
	case cmdLock:
		a.lockNow()
	case cmdQuit:
		a.quit()
	}
}

// ---- icons for toasts --------------------------------------------------------

func appIconPath() string { return filepath.Join(appDataDir(), "icons", "app.png") }

func serviceIconPath(id string) string {
	p := filepath.Join(appDataDir(), "icons", id+".png")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return appIconPath()
}

func writeIcons() {
	dir := filepath.Join(appDataDir(), "icons")
	_ = os.MkdirAll(dir, 0o755)
	for name, data := range toastIcons {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && info.Size() == int64(len(data)) {
			continue
		}
		_ = os.WriteFile(p, data, 0o644)
	}
}

// lockForUpdate keeps WhatsApp and Telegram behind the settings page while a
// required update (twincxt-min) is not installed yet.
func (a *app) lockForUpdate() {
	if a.locked {
		return
	}
	a.locked = true
	debugLog("update: this version is below the minimum, services locked")
	a.settings = true
	a.layout()
	a.pushState()
	a.applyStrip()
	a.showWindow()
}
