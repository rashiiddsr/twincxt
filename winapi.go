//go:build windows

package main

import (
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	dwmapi   = windows.NewLazySystemDLL("dwmapi.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")

	pRegisterClassEx        = user32.NewProc("RegisterClassExW")
	pCreateWindowEx         = user32.NewProc("CreateWindowExW")
	pDefWindowProc          = user32.NewProc("DefWindowProcW")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pShowWindow             = user32.NewProc("ShowWindow")
	pUpdateWindow           = user32.NewProc("UpdateWindow")
	pGetMessage             = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessage        = user32.NewProc("DispatchMessageW")
	pPostQuitMessage        = user32.NewProc("PostQuitMessage")
	pPostMessage            = user32.NewProc("PostMessageW")
	pSendMessage            = user32.NewProc("SendMessageW")
	pGetClientRect          = user32.NewProc("GetClientRect")
	pGetWindowRect          = user32.NewProc("GetWindowRect")
	pSetWindowPos           = user32.NewProc("SetWindowPos")
	pMoveWindow             = user32.NewProc("MoveWindow")
	pIsZoomed               = user32.NewProc("IsZoomed")
	pIsIconic               = user32.NewProc("IsIconic")
	pIsWindowVisible        = user32.NewProc("IsWindowVisible")
	pReleaseCapture         = user32.NewProc("ReleaseCapture")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pGetForegroundWindow    = user32.NewProc("GetForegroundWindow")
	pFindWindow             = user32.NewProc("FindWindowW")
	pLoadImage              = user32.NewProc("LoadImageW")
	pLoadCursor             = user32.NewProc("LoadCursorW")
	pGetDpiForWindow        = user32.NewProc("GetDpiForWindow")
	pGetSystemMetricsForDpi = user32.NewProc("GetSystemMetricsForDpi")
	pGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenu             = user32.NewProc("AppendMenuW")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pRegisterWindowMessage  = user32.NewProc("RegisterWindowMessageW")
	pFlashWindowEx          = user32.NewProc("FlashWindowEx")
	pGetKeyState            = user32.NewProc("GetKeyState")
	pMessageBox             = user32.NewProc("MessageBoxW")
	pGetWindowPlacement     = user32.NewProc("GetWindowPlacement")
	pSetWindowPlacement     = user32.NewProc("SetWindowPlacement")
	pMonitorFromWindow      = user32.NewProc("MonitorFromWindow")
	pGetMonitorInfo         = user32.NewProc("GetMonitorInfoW")
	pSetWindowText          = user32.NewProc("SetWindowTextW")
	pSetFocus               = user32.NewProc("SetFocus")
	pFillRect               = user32.NewProc("FillRect")
	pInvalidateRect         = user32.NewProc("InvalidateRect")
	pSetTimer               = user32.NewProc("SetTimer")
	pCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	pDeleteObject           = gdi32.NewProc("DeleteObject")
	pGetModuleHandle        = kernel32.NewProc("GetModuleHandleW")
	pCreateMutex            = kernel32.NewProc("CreateMutexW")
	pDwmSetWindowAttribute  = dwmapi.NewProc("DwmSetWindowAttribute")
	pDwmExtendFrame         = dwmapi.NewProc("DwmExtendFrameIntoClientArea")
	pShellNotifyIcon        = shell32.NewProc("Shell_NotifyIconW")
	pShellExecute           = shell32.NewProc("ShellExecuteW")
	pSetAppUserModelID      = shell32.NewProc("SetCurrentProcessExplicitAppUserModelID")
	pSHGetKnownFolderPath   = shell32.NewProc("SHGetKnownFolderPath")
	pCoInitializeEx         = ole32.NewProc("CoInitializeEx")
	pCoTaskMemFree          = ole32.NewProc("CoTaskMemFree")
	pRegSetKeyValue         = advapi32.NewProc("RegSetKeyValueW")
	pRegDeleteKeyValue      = advapi32.NewProc("RegDeleteKeyValueW")
	pRegGetValue            = advapi32.NewProc("RegGetValueW")
)

const (
	WS_OVERLAPPEDWINDOW = 0x00CF0000
	WS_CHILD            = 0x40000000
	WS_VISIBLE          = 0x10000000
	WS_CLIPCHILDREN     = 0x02000000
	WS_CLIPSIBLINGS     = 0x04000000

	SW_HIDE          = 0
	SW_SHOWNORMAL    = 1
	SW_SHOWMINIMIZED = 2
	SW_MAXIMIZE      = 3
	SW_SHOW          = 5
	SW_SHOWNA        = 8
	HWND_TOP         = 0
	HWND_BOTTOM      = 1
	WM_TIMER         = 0x0113
	SW_MINIMIZE      = 6
	SW_RESTORE       = 9

	WM_DESTROY       = 0x0002
	WM_MOVE          = 0x0003
	WM_SIZE          = 0x0005
	WM_ACTIVATE      = 0x0006
	WM_SETFOCUS      = 0x0007
	WM_CLOSE         = 0x0010
	WM_ERASEBKGND    = 0x0014
	WM_SHOWWINDOW    = 0x0018
	WM_GETMINMAXINFO = 0x0024
	WM_COPYDATA      = 0x004A
	WM_NCCALCSIZE    = 0x0083
	WM_NCLBUTTONDOWN = 0x00A1
	WM_COMMAND       = 0x0111
	WM_SYSCOMMAND    = 0x0112
	WM_LBUTTONUP     = 0x0202
	WM_LBUTTONDBLCLK = 0x0203
	WM_RBUTTONUP     = 0x0205
	WM_MOVING        = 0x0216
	WM_DPICHANGED    = 0x02E0
	WM_SETICON       = 0x0080
	WM_NULL          = 0x0000
	WM_APP           = 0x8000
	WM_TRAY          = WM_APP + 1

	SC_MINIMIZE = 0xF020
	SC_MAXIMIZE = 0xF030
	SC_RESTORE  = 0xF120

	HTCAPTION = 2
	HTTOP     = 12

	SWP_NOSIZE       = 0x0001
	SWP_NOMOVE       = 0x0002
	SWP_NOZORDER     = 0x0004
	SWP_NOACTIVATE   = 0x0010
	SWP_FRAMECHANGED = 0x0020
	SWP_SHOWWINDOW   = 0x0040

	SM_CXSCREEN       = 0
	SM_CYSCREEN       = 1
	SM_CYFRAME        = 33
	SM_CXPADDEDBORDER = 92

	IMAGE_ICON = 1
	ICON_SMALL = 0
	ICON_BIG   = 1
	IDC_ARROW  = 32512

	DWMWA_USE_IMMERSIVE_DARK_MODE  = 20
	DWMWA_WINDOW_CORNER_PREFERENCE = 33
	DWMWA_BORDER_COLOR             = 34
	DWMWA_CAPTION_COLOR            = 35
	DWMWA_TEXT_COLOR               = 36

	NIM_ADD     = 0
	NIM_MODIFY  = 1
	NIM_DELETE  = 2
	NIF_MESSAGE = 0x1
	NIF_ICON    = 0x2
	NIF_TIP     = 0x4

	MF_STRING       = 0x0
	MF_SEPARATOR    = 0x800
	MF_GRAYED       = 0x1
	TPM_RIGHTBUTTON = 0x2
	TPM_RETURNCMD   = 0x100
	TPM_BOTTOMALIGN = 0x20

	FLASHW_TRAY      = 0x2
	FLASHW_TIMERNOFG = 0xC

	VK_CONTROL   = 0x11
	VK_MENU      = 0x12
	VK_SHIFT     = 0x10
	VK_TAB       = 0x09
	VK_ESCAPE    = 0x1B
	VK_OEM_COMMA = 0xBC

	MB_OK        = 0x0
	MB_ICONERROR = 0x10

	MONITOR_DEFAULTTONEAREST = 2

	HKEY_CURRENT_USER = 0x80000001
	REG_SZ            = 1
	RRF_RT_REG_SZ     = 0x2
	RRF_RT_REG_DWORD  = 0x10
)

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

func (r rect) w() int32 { return r.Right - r.Left }
func (r rect) h() int32 { return r.Bottom - r.Top }

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type minMaxInfo struct {
	Reserved     point
	MaxSize      point
	MaxPosition  point
	MinTrackSize point
	MaxTrackSize point
}

type ncCalcSizeParams struct {
	Rgrc  [3]rect
	Lppos uintptr
}

type windowPlacement struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    point
	MaxPosition    point
	NormalPosition rect
	Device         rect
}

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

type copyDataStruct struct {
	Data uintptr
	Size uint32
	Ptr  uintptr
}

type flashWInfo struct {
	Size    uint32
	Hwnd    uintptr
	Flags   uint32
	Count   uint32
	Timeout uint32
}

type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        windows.GUID
	BalloonIcon     uintptr
}

func utf16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func utf16ptr(s string) uintptr { return uintptr(unsafe.Pointer(utf16(s))) }

func loword(v uintptr) uint32 { return uint32(v) & 0xFFFF }
func hiword(v uintptr) uint32 { return (uint32(v) >> 16) & 0xFFFF }

func moduleHandle() uintptr {
	h, _, _ := pGetModuleHandle.Call(0)
	return h
}

func dpiOf(hwnd uintptr) uint32 {
	d, _, _ := pGetDpiForWindow.Call(hwnd)
	if d == 0 {
		return 96
	}
	return uint32(d)
}

func scale(v int32, dpi uint32) int32 { return int32(int64(v) * int64(dpi) / 96) }

func clientRect(hwnd uintptr) rect {
	var r rect
	pGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func isZoomed(hwnd uintptr) bool  { r, _, _ := pIsZoomed.Call(hwnd); return r != 0 }
func isIconic(hwnd uintptr) bool  { r, _, _ := pIsIconic.Call(hwnd); return r != 0 }
func isVisible(hwnd uintptr) bool { r, _, _ := pIsWindowVisible.Call(hwnd); return r != 0 }

func keyDown(vk uintptr) bool {
	r, _, _ := pGetKeyState.Call(vk)
	return int16(r) < 0
}

func dwmSet(hwnd uintptr, attr uintptr, value uint32) {
	pDwmSetWindowAttribute.Call(hwnd, attr, uintptr(unsafe.Pointer(&value)), 4)
}

func colorRef(c rgb) uint32 { return uint32(c.R) | uint32(c.G)<<8 | uint32(c.B)<<16 }

func messageBox(text, title string, flags uintptr) {
	pMessageBox.Call(0, utf16ptr(text), utf16ptr(title), flags)
}

func shellOpen(target string) {
	pShellExecute.Call(0, utf16ptr("open"), utf16ptr(target), 0, 0, SW_SHOWNORMAL)
}

func loadAppIcon(size int32) uintptr {
	h, _, _ := pLoadImage.Call(moduleHandle(), iconResourceID, IMAGE_ICON, uintptr(size), uintptr(size), 0)
	return h
}

func coTaskString(p uintptr) string {
	if p == 0 {
		return ""
	}
	s := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(p)))
	pCoTaskMemFree.Call(p)
	return s
}

func knownFolder(id *windows.GUID) string {
	var p uintptr
	r, _, _ := pSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(id)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if r != 0 {
		return ""
	}
	return coTaskString(p)
}

// waitForProcessExit waits (while still pumping window messages, which COM
// needs) until process pid has exited or timeout passes.
func waitForProcessExit(pid uint32, timeout time.Duration) {
	if pid == 0 {
		return
	}
	const synchronize = 0x00100000
	h, _, _ := pOpenProcess.Call(synchronize, 0, uintptr(pid))
	if h == 0 {
		return
	}
	defer pCloseHandle.Call(h)
	deadline := time.Now().Add(timeout)
	var m msg
	for time.Now().Before(deadline) {
		if r, _, _ := pWaitForSingleObject.Call(h, 50); r == 0 { // WAIT_OBJECT_0
			return
		}
		// Handle what is queued now, but never more than a batch, so a steady
		// stream of messages cannot keep us from checking the deadline.
		for i := 0; i < 64; i++ {
			r, _, _ := pPeekMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1) // PM_REMOVE
			if r == 0 {
				break
			}
			if m.Message == WM_TIMER || m.Message == WM_CLOSE {
				continue // no sweeps or second close while shutting down
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
		}
	}
}

var (
	pPeekMessage         = user32.NewProc("PeekMessageW")
	pOpenProcess         = kernel32.NewProc("OpenProcess")
	pWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	pCloseHandle         = kernel32.NewProc("CloseHandle")
)

const (
	WM_QUERYENDSESSION = 0x0011
	WM_ENDSESSION      = 0x0016
)

var pKillTimer = user32.NewProc("KillTimer")

var (
	pGetWindowLongPtr           = user32.NewProc("GetWindowLongPtrW")
	pSetWindowLongPtr           = user32.NewProc("SetWindowLongPtrW")
	pSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
)

// makeInvisible turns a window fully transparent and click-through while it
// stays "visible" to Windows (so the page inside keeps running normally).
func makeInvisible(hwnd uintptr) {
	const (
		gwlExStyle      = ^uintptr(19) // GWL_EXSTYLE (-20)
		wsExLayered     = 0x00080000
		wsExTransparent = 0x00000020
		wsExToolWindow  = 0x00000080 // also drops its taskbar button
		wsExAppWindow   = 0x00040000
		lwaAlpha        = 0x2
	)
	style, _, _ := pGetWindowLongPtr.Call(hwnd, gwlExStyle)
	style = (style | wsExLayered | wsExTransparent | wsExToolWindow) &^ wsExAppWindow
	pSetWindowLongPtr.Call(hwnd, gwlExStyle, style)
	pSetLayeredWindowAttributes.Call(hwnd, 0, 0, lwaAlpha)
}
