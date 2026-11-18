//go:build windows

// Twincxt: WhatsApp Web and Telegram Web in one native Windows window.
package main

import (
	_ "embed"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	appName        = "Twincxt"
	appUserModelID = "Twincxt.App" // must match installer.iss
	protocolName   = "twincxt"     // toast clicks open twincxt:open/<svc>
	iconResourceID = 2             // must match gen_resources.py
)

//go:embed ui/shell.html
var shellHTML string

//go:embed ui/icons/app.png
var iconApp []byte

//go:embed ui/icons/wa.png
var iconWA []byte

//go:embed ui/icons/tg.png
var iconTG []byte

var toastIcons = map[string][]byte{"app.png": iconApp, "wa.png": iconWA, "tg.png": iconTG}

func init() {
	// WebView2 and all windows live on one STA thread.
	runtime.LockOSThread()
	pCoInitializeEx.Call(0, 0x2) // COINIT_APARTMENTTHREADED
}

func main() {
	arg := strings.Join(os.Args[1:], " ")

	if !acquireSingleInstance() {
		forwardToRunningInstance(arg)
		return
	}

	id := utf16(appUserModelID)
	pSetAppUserModelID.Call(uintptr(unsafe.Pointer(id)))

	a := &app{
		cfg:    loadConfig(),
		popups: map[uintptr]*popup{},
		svcs: map[string]*service{
			"wa": {id: "wa", name: "WhatsApp"},
			"tg": {id: "tg", name: "Telegram"},
		},
	}
	theApp = a
	writeIcons()

	registerClasses()
	a.createWindow()
	a.upd = newUpdater(a)
	a.upd.start()
	r, _, _ := pRegisterWindowMessage.Call(utf16ptr("TaskbarCreated"))
	a.taskbarCreated = uint32(r)
	a.addTray()

	startHidden := strings.Contains(arg, "--autostart") && a.cfg.StartHidden
	if !startHidden {
		if a.cfg.Window.Maximized {
			pShowWindow.Call(a.hwnd, SW_MAXIMIZE)
		} else {
			pShowWindow.Call(a.hwnd, SW_SHOW)
		}
		pUpdateWindow.Call(a.hwnd)
	}

	dataDir := filepath.Join(appDataDir(), "WebView2")
	migrateLegacyProfile(dataDir)
	err := createEnvironment(dataDir, func(env *environment, hr uintptr) {
		if env == nil {
			messageBox("Microsoft Edge WebView2 Runtime tidak ditemukan atau gagal dijalankan.\n\n"+
				"Pasang WebView2 Runtime dari situs Microsoft, lalu buka Twincxt lagi.", appName, MB_OK|MB_ICONERROR)
			a.quit()
			return
		}
		a.env = env
		a.startWebViews()
	})
	if err != nil {
		messageBox("WebView2 gagal dimuat: "+err.Error(), appName, MB_OK|MB_ICONERROR)
		return
	}

	if strings.Contains(arg, protocolName+":") {
		a.activate(arg)
	}

	var m msg
	for {
		r, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (a *app) startWebViews() {
	a.env.createController(a.shellH, func(wv *webView) {
		if wv == nil {
			return
		}
		a.shell = wv
		wv.FitHost()
		bg := sideLight
		if a.isDark() {
			bg = sideDark
		}
		wv.SetBackground(bg)
		wv.ApplySettings(webViewSettings{})
		wv.OnMessage(a.onShellMessage)
		wv.OnAcceleratorKey(a.handleShortcut)
		wv.OnNavigationStarting(func(uri string) bool {
			return !strings.HasPrefix(uri, "http") // the shell never leaves its own page
		})
		a.applyTheme()
		wv.NavigateToString(shellHTML)
	})
	for _, id := range []string{"wa", "tg"} {
		s := a.svcs[id]
		a.env.createController(s.host, func(wv *webView) {
			if wv != nil {
				a.setupService(s, wv)
			}
		})
	}
}

// debugLog appends to %APPDATA%\Twincxt\debug.log when TWINCXT_DEBUG is set.
func debugLog(line string) {
	if os.Getenv("TWINCXT_DEBUG") == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(appDataDir(), "debug.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(time.Now().Format("15:04:05.000") + "  " + line + "\n")
}

var (
	instanceMutex uintptr
	startedAt     = time.Now()
)

func acquireSingleInstance() bool {
	h, _, err := pCreateMutex.Call(0, 1, utf16ptr(`Local\Twincxt-`+instanceKey()))
	instanceMutex = h
	return h != 0 && err != windows.ERROR_ALREADY_EXISTS
}

// instanceKey identifies one data folder. Twincxt allows one instance per
// data folder, so a test copy started with another %APPDATA% never collides
// with the installed app.
func instanceKey() string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(appDataDir())))
	return fmt.Sprintf("%08x", h.Sum32())
}

func releaseSingleInstance() {
	if instanceMutex != 0 {
		_ = windows.CloseHandle(windows.Handle(instanceMutex))
		instanceMutex = 0
	}
}

func forwardToRunningInstance(arg string) {
	hwnd, _, _ := pFindWindow.Call(uintptr(unsafe.Pointer(mainClass)), 0)
	if hwnd == 0 {
		return
	}
	if arg == "" {
		arg = "show"
	}
	b := []byte(arg)
	cds := copyDataStruct{Size: uint32(len(b)), Ptr: uintptr(unsafe.Pointer(&b[0]))}
	pSetForegroundWindow.Call(hwnd)
	pSendMessage.Call(hwnd, WM_COPYDATA, 0, uintptr(unsafe.Pointer(&cds)))
}
