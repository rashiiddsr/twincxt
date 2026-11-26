//go:build windows

package main

// Minimal WebView2 COM binding.
//
// go-webview2 only supports one WebView per window and exposes few events, so
// this file talks to the WebView2 COM interfaces directly. The runtime object
// implements the newest ICoreWebView2_N interface on a single vtable, so later
// methods are called by their vtable index on the base pointer.
//
// Vtable indices below follow WebView2.h (IUnknown occupies 0..2).

import (
	"math"
	"syscall"
	"unsafe"

	"github.com/jchv/go-webview2/webviewloader"
	"golang.org/x/sys/windows"
)

// ---- raw COM helpers --------------------------------------------------------

func comCall(obj uintptr, index int, args ...uintptr) uintptr {
	if obj == 0 {
		return 0x80004003 // E_POINTER: object already released
	}
	vtbl := *(*uintptr)(unsafe.Pointer(obj))
	fn := *(*uintptr)(unsafe.Pointer(vtbl + uintptr(index)*unsafe.Sizeof(uintptr(0))))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, obj)
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func comAddRef(obj uintptr) {
	if obj != 0 {
		comCall(obj, 1)
	}
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comCall(obj, 2)
	}
}

func comGetString(obj uintptr, index int) string {
	var p uintptr
	if comCall(obj, index, uintptr(unsafe.Pointer(&p))) != 0 {
		return ""
	}
	return coTaskString(p)
}

func comGetPtr(obj uintptr, index int) uintptr {
	var p uintptr
	if comCall(obj, index, uintptr(unsafe.Pointer(&p))) != 0 {
		return 0
	}
	return p
}

func comGetInt(obj uintptr, index int) int32 {
	var v int32
	comCall(obj, index, uintptr(unsafe.Pointer(&v)))
	return v
}

func boolArg(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

// ---- callback objects -------------------------------------------------------

// handler is a COM object implementing any WebView2 "…Handler" interface:
// they are all IUnknown + Invoke(arg1, arg2). Instances are kept alive in
// liveHandlers for the lifetime of the process.
type handler struct {
	vtbl *[4]uintptr
	fn   func(a, b uintptr) uintptr
}

var (
	handlerVtbl = [4]uintptr{
		windows.NewCallback(func(this *handler, riid, out uintptr) uintptr {
			*(*uintptr)(unsafe.Pointer(out)) = uintptr(unsafe.Pointer(this))
			return 0
		}),
		windows.NewCallback(func(this *handler) uintptr { return 1 }),
		windows.NewCallback(func(this *handler) uintptr { return 1 }),
		windows.NewCallback(func(this *handler, a, b uintptr) uintptr {
			if this.fn != nil {
				return this.fn(a, b)
			}
			return 0
		}),
	}
	liveHandlers []*handler
)

func newHandler(fn func(a, b uintptr) uintptr) uintptr {
	h := &handler{vtbl: &handlerVtbl, fn: fn}
	liveHandlers = append(liveHandlers, h)
	return uintptr(unsafe.Pointer(h))
}

// ---- environment ------------------------------------------------------------

type environment struct {
	ptr     uintptr
	version string
}

// createEnvironment starts the WebView2 browser process for dataDir and calls
// done on the UI thread when it is ready (or with err != 0 on failure).
func createEnvironment(dataDir string, done func(env *environment, hr uintptr)) error {
	h := newHandler(func(hr, envPtr uintptr) uintptr {
		if hr != 0 || envPtr == 0 {
			done(nil, hr)
			return 0
		}
		comAddRef(envPtr)
		env := &environment{ptr: envPtr}
		env.version = comGetString(envPtr, 5) // get_BrowserVersionString
		done(env, 0)
		return 0
	})
	res, err := webviewloader.CreateCoreWebView2EnvironmentWithOptions(nil, utf16(dataDir), 0, h)
	if err != nil {
		return err
	}
	if res != 0 {
		return syscall.Errno(res)
	}
	return nil
}

// createController creates a WebView inside parent and calls done when ready.
func (e *environment) createController(parent uintptr, done func(wv *webView)) {
	h := newHandler(func(hr, ctrl uintptr) uintptr {
		if hr != 0 || ctrl == 0 {
			done(nil)
			return 0
		}
		comAddRef(ctrl)
		core := comGetPtr(ctrl, 25) // get_CoreWebView2
		done(&webView{ctrl: ctrl, core: core, host: parent})
		return 0
	})
	comCall(e.ptr, 3, parent, h) // CreateCoreWebView2Controller
}

// ---- webview ----------------------------------------------------------------

type webView struct {
	ctrl uintptr // ICoreWebView2Controller
	core uintptr // ICoreWebView2
	host uintptr // parent HWND
}

type eventToken struct{ v int64 }

func (w *webView) on(index int, fn func(sender, args uintptr)) {
	var tok eventToken
	h := newHandler(func(a, b uintptr) uintptr { fn(a, b); return 0 })
	comCall(w.core, index, h, uintptr(unsafe.Pointer(&tok)))
}

func (w *webView) Navigate(url string)          { comCall(w.core, 5, utf16ptr(url)) }
func (w *webView) NavigateToString(html string) { comCall(w.core, 6, utf16ptr(html)) }
func (w *webView) AddInitScript(js string)      { comCall(w.core, 27, utf16ptr(js), 0) }
func (w *webView) Eval(js string)               { comCall(w.core, 29, utf16ptr(js), 0) }
func (w *webView) Reload()                      { comCall(w.core, 31) }
func (w *webView) Title() string                { return comGetString(w.core, 48) }
func (w *webView) Source() string               { return comGetString(w.core, 4) }
func (w *webView) FullScreen() bool             { return comGetInt(w.core, 54) != 0 }

func (w *webView) SetVisible(v bool) { comCall(w.ctrl, 4, boolArg(v)) }
func (w *webView) SetBounds(r rect)  { comCall(w.ctrl, 6, uintptr(unsafe.Pointer(&r))) }
func (w *webView) SetZoom(f float64) { comCall(w.ctrl, 8, uintptr(math.Float64bits(f))) }
func (w *webView) Focus()            { comCall(w.ctrl, 12, 0) } // COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC
func (w *webView) NotifyMoved()      { comCall(w.ctrl, 23) }
func (w *webView) Close()            { comCall(w.ctrl, 24) }

// Release drops our references so the browser process can shut down.
func (w *webView) Release() {
	comRelease(w.core)
	comRelease(w.ctrl)
	w.core, w.ctrl = 0, 0
}

// BrowserPID: ICoreWebView2::get_BrowserProcessId.
func (w *webView) BrowserPID() uint32 { return uint32(comGetInt(w.core, 37)) }

// SetBackground sets the colour shown before the page paints (avoids white flashes).
func (w *webView) SetBackground(c rgb) {
	packed := uint32(0xFF) | uint32(c.R)<<8 | uint32(c.G)<<16 | uint32(c.B)<<24 // {A,R,G,B}
	comCall(w.ctrl, 27, uintptr(packed))
}

func (w *webView) FitHost() {
	r := clientRect(w.host)
	w.SetBounds(rect{0, 0, r.Right, r.Bottom})
}

// Settings applies the "app, not browser" defaults.
type webViewSettings struct {
	userAgent    string
	contextMenus bool
	statusBar    bool
}

func (w *webView) ApplySettings(s webViewSettings) {
	set := comGetPtr(w.core, 3) // get_Settings
	if set == 0 {
		return
	}
	defer comRelease(set)
	comCall(set, 10, boolArg(s.statusBar))    // put_IsStatusBarEnabled
	comCall(set, 12, 0)                       // put_AreDevToolsEnabled
	comCall(set, 14, boolArg(s.contextMenus)) // put_AreDefaultContextMenusEnabled
	comCall(set, 18, 0)                       // put_IsZoomControlEnabled
	if s.userAgent != "" {
		comCall(set, 22, utf16ptr(s.userAgent)) // put_UserAgent
	}
	comCall(set, 24, 0) // put_AreBrowserAcceleratorKeysEnabled
	comCall(set, 26, 0) // put_IsPasswordAutosaveEnabled
	comCall(set, 30, 0) // put_IsPinchZoomEnabled
	comCall(set, 32, 0) // put_IsSwipeNavigationEnabled
}

// SetColorScheme: 0 = follow Windows, 1 = light, 2 = dark. Applies to every
// WebView sharing this profile.
func (w *webView) SetColorScheme(v int) {
	profile := comGetPtr(w.core, 105) // ICoreWebView2_13::get_Profile
	if profile == 0 {
		debugLog("profile: unavailable")
		return
	}
	hr := comCall(profile, 9, uintptr(v)) // put_PreferredColorScheme
	debugLog("profile: " + comGetString(profile, 3) + " path=" + comGetString(profile, 5) +
		" scheme=" + itoa(int64(comGetInt(profile, 8))) + " hr=" + itoa(int64(hr)))
	comRelease(profile)
}

// CallCDP runs a Chrome DevTools Protocol method (used to clear site data).
func (w *webView) CallCDP(method, paramsJSON string, done func()) {
	h := newHandler(func(hr, result uintptr) uintptr {
		if done != nil {
			done()
		}
		return 0
	})
	comCall(w.core, 36, utf16ptr(method), utf16ptr(paramsJSON), h)
}

// ---- events -----------------------------------------------------------------

func (w *webView) OnMessage(fn func(string)) {
	w.on(34, func(_, args uintptr) { // add_WebMessageReceived
		var p uintptr
		if comCall(args, 5, uintptr(unsafe.Pointer(&p))) == 0 { // TryGetWebMessageAsString
			fn(coTaskString(p))
		}
	})
}

func (w *webView) OnTitleChanged(fn func(title string)) {
	w.on(46, func(_, _ uintptr) { fn(w.Title()) }) // add_DocumentTitleChanged
}

// OnNavigationStarting: return false from fn to cancel the navigation.
func (w *webView) OnNavigationStarting(fn func(uri string) bool) {
	w.on(7, func(_, args uintptr) {
		if !fn(comGetString(args, 3)) {
			comCall(args, 8, 1) // put_Cancel
		}
	})
}

func (w *webView) OnNavigationCompleted(fn func()) {
	w.on(15, func(_, _ uintptr) { fn() })
}

// COREWEBVIEW2_PROCESS_FAILED_KIND values that matter to us.
const (
	procBrowserExited = 0 // every WebView is gone
	procRenderExited  = 1 // this page's renderer died: the page is blank
)

// OnProcessFailed reports the failure kind. Most kinds (GPU, utility, audio
// or a single iframe process restarting) recover on their own and need no
// action; reloading on them caused needless "loading" flashes.
func (w *webView) OnProcessFailed(fn func(kind int32)) {
	w.on(25, func(_, args uintptr) { fn(comGetInt(args, 3)) }) // get_ProcessFailedKind
}

// PlayingAudio: ICoreWebView2_8::get_IsDocumentPlayingAudio.
func (w *webView) PlayingAudio() bool { return comGetInt(w.core, 87) != 0 }

const (
	memoryNormal = 0
	memoryLow    = 1
)

// SetMemoryTarget: ICoreWebView2_19::put_MemoryUsageTargetLevel. "Low" asks
// the browser to trim caches for a page that is out of view.
func (w *webView) SetMemoryTarget(level int) { comCall(w.core, 120, uintptr(level)) }

// MemoryTarget reads the level back (used to verify the binding in debug logs).
func (w *webView) MemoryTarget() int32 { return comGetInt(w.core, 119) }

func (w *webView) OnFullScreenChanged(fn func(bool)) {
	w.on(52, func(_, _ uintptr) { fn(w.FullScreen()) })
}

func (w *webView) OnWindowCloseRequested(fn func()) {
	w.on(59, func(_, _ uintptr) { fn() })
}

const (
	permMicrophone    = 1
	permCamera        = 2
	permGeolocation   = 3
	permNotifications = 4
	permAllow         = 1
	permDeny          = 2
)

// OnPermission: fn returns permAllow / permDeny (or 0 for the default prompt).
func (w *webView) OnPermission(fn func(uri string, kind int32) int32) {
	w.on(23, func(_, args uintptr) {
		uri := comGetString(args, 3)
		kind := comGetInt(args, 4)
		if state := fn(uri, kind); state != 0 {
			comCall(args, 7, uintptr(state)) // put_State
		}
	})
}

// newWindowRequest wraps ICoreWebView2NewWindowRequestedEventArgs.
type newWindowRequest struct {
	args uintptr
	URI  string
}

func (r *newWindowRequest) Handled() { comCall(r.args, 6, 1) } // put_Handled

// UserInitiated reports whether the page asked for the window in response to
// a click or key press. WebView2 has no popup blocker, so this is how we tell
// "Move to new window" (a click) from a page opening windows on its own.
func (r *newWindowRequest) UserInitiated() bool { return comGetInt(r.args, 8) != 0 } // get_IsUserInitiated

// Attach hands an already-created WebView to the page synchronously, so
// window.open() returns it immediately. (Creating the WebView asynchronously
// with a deferral loses whatever the opener writes into the new window in the
// meantime; WhatsApp's call window then stays blank.)
func (r *newWindowRequest) Attach(wv *webView) { comCall(r.args, 4, wv.core) } // put_NewWindow

// Size returns the width/height the page asked for in window.open features
// (CSS px), or 0, 0 when it did not ask.
func (r *newWindowRequest) Size() (w, h int32) {
	f := comGetPtr(r.args, 10) // get_WindowFeatures
	if f == 0 {
		return 0, 0
	}
	defer comRelease(f)
	if comGetInt(f, 4) == 0 { // get_HasSize
		return 0, 0
	}
	h, w = comGetInt(f, 7), comGetInt(f, 8) // get_Height, get_Width
	if w < 200 || h < 150 || w > 8000 || h > 8000 {
		return 0, 0
	}
	return w, h
}

func (w *webView) OnNewWindow(fn func(req *newWindowRequest)) {
	w.on(44, func(_, args uintptr) {
		fn(&newWindowRequest{args: args, URI: comGetString(args, 3)})
	})
}

// OnDownload hides the Edge download flyout. fn receives WebView2's proposed
// path and must call finish (now or later) with the path to use, or cancel.
func (w *webView) OnDownload(fn func(path string, finish func(newPath string, cancel bool))) {
	w.on(75, func(_, args uintptr) { // ICoreWebView2_4::add_DownloadStarting
		path := comGetString(args, 6) // get_ResultFilePath
		comCall(args, 9, 1)           // put_Handled: no default download UI
		var def uintptr
		comCall(args, 10, uintptr(unsafe.Pointer(&def))) // GetDeferral
		comAddRef(args)
		done := false
		fn(path, func(newPath string, cancel bool) {
			if done {
				return
			}
			done = true
			if cancel {
				comCall(args, 5, 1) // put_Cancel
			} else if newPath != "" && newPath != path {
				p := utf16ptr(newPath)
				comCall(args, 7, uintptr(unsafe.Pointer(p))) // put_ResultFilePath
			}
			if def != 0 {
				comCall(def, 3) // ICoreWebView2Deferral::Complete
				comRelease(def)
			}
			comRelease(args)
		})
	})
}

// OnContextMenu removes browser-only entries (Back, Reload, Save as, Print…)
// from the default context menu, keeping editing commands like Copy/Paste.
func (w *webView) OnContextMenu(remove map[string]bool) {
	w.on(100, func(_, args uintptr) { // ICoreWebView2_11::add_ContextMenuRequested
		items := comGetPtr(args, 3) // get_MenuItems
		if items == 0 {
			return
		}
		defer comRelease(items)
		var count uint32
		comCall(items, 3, uintptr(unsafe.Pointer(&count)))
		if count > 200 {
			return
		}
		lastWasSep := true
		for i := int(count) - 1; i >= 0; i-- {
			var item uintptr
			if comCall(items, 4, uintptr(i), uintptr(unsafe.Pointer(&item))) != 0 || item == 0 {
				continue
			}
			name := comGetString(item, 3)
			kind := comGetInt(item, 8)
			comRelease(item)
			if remove[name] {
				comCall(items, 5, uintptr(i)) // RemoveValueAtIndex
			}
			_ = kind
		}
		// Tidy separators: drop leading, trailing and doubled ones.
		comCall(items, 3, uintptr(unsafe.Pointer(&count)))
		for i := 0; i < int(count); {
			var item uintptr
			if comCall(items, 4, uintptr(i), uintptr(unsafe.Pointer(&item))) != 0 || item == 0 {
				i++
				continue
			}
			isSep := comGetInt(item, 8) == 3 // COREWEBVIEW2_CONTEXT_MENU_ITEM_KIND_SEPARATOR
			comRelease(item)
			if isSep && (lastWasSep || i == int(count)-1) {
				comCall(items, 5, uintptr(i))
				count--
				continue
			}
			lastWasSep = isSep
			i++
		}
	})
}

const (
	keyDownEvent       = 0
	systemKeyDownEvent = 2
)

// OnAcceleratorKey lets the app handle shortcuts while a page has focus.
// fn returns true when it handled the key.
func (w *webView) OnAcceleratorKey(fn func(vk uint32) bool) {
	var tok eventToken
	h := newHandler(func(_, args uintptr) uintptr {
		kind := comGetInt(args, 3)
		if kind != keyDownEvent && kind != systemKeyDownEvent {
			return 0
		}
		var vk uint32
		comCall(args, 4, uintptr(unsafe.Pointer(&vk)))
		if fn(vk) {
			comCall(args, 8, 1) // put_Handled
		}
		return 0
	})
	comCall(w.ctrl, 19, h, uintptr(unsafe.Pointer(&tok))) // controller add_AcceleratorKeyPressed
}
