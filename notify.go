//go:build windows

package main

import (
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Windows toast notifications.
//
// The toast XML is written to a file and PowerShell only receives the file
// path. Message text is never placed inside a script, so chat content cannot
// be interpreted as PowerShell (the previous go-toast based code embedded it
// in a double-quoted here-string, where "$(...)" in a message would execute).

const toastScript = `param([string]$XmlPath, [string]$AppId)
$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$doc = New-Object Windows.Data.Xml.Dom.XmlDocument
$doc.LoadXml([System.IO.File]::ReadAllText($XmlPath, [System.Text.Encoding]::UTF8))
$toast = New-Object Windows.UI.Notifications.ToastNotification $doc
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($AppId).Show($toast)
Remove-Item -LiteralPath $XmlPath -ErrorAction SilentlyContinue
`

var (
	toastOnce   sync.Once
	toastDir    string
	toastSerial int64
	toastMu     sync.Mutex
)

type toast struct {
	Title  string
	Body   string
	Launch string // protocol URI opened when the toast is clicked
	Icon   string // local PNG/ICO path
	Silent bool
}

func (t toast) xml() string {
	esc := func(s string) string { return html.EscapeString(strings.TrimSpace(s)) }
	var b strings.Builder
	b.WriteString(`<toast activationType="protocol" launch="` + esc(t.Launch) + `">`)
	b.WriteString(`<visual><binding template="ToastGeneric">`)
	if t.Icon != "" {
		b.WriteString(`<image placement="appLogoOverride" hint-crop="circle" src="` + esc(t.Icon) + `"/>`)
	}
	b.WriteString(`<text>` + esc(truncate(t.Title, 120)) + `</text>`)
	if t.Body != "" {
		b.WriteString(`<text>` + esc(truncate(t.Body, 400)) + `</text>`)
	}
	b.WriteString(`</binding></visual>`)
	if t.Silent {
		b.WriteString(`<audio silent="true"/>`)
	}
	b.WriteString(`</toast>`)
	return b.String()
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func (t toast) show() {
	toastOnce.Do(func() {
		toastDir = filepath.Join(os.TempDir(), appName+"-toast")
		_ = os.MkdirAll(toastDir, 0o755)
		_ = os.WriteFile(filepath.Join(toastDir, "show.ps1"), []byte(toastScript), 0o644)
	})
	toastMu.Lock()
	toastSerial++
	name := filepath.Join(toastDir, time.Now().Format("150405")+"-"+itoa(toastSerial)+".xml")
	toastMu.Unlock()
	if err := os.WriteFile(name, []byte(t.xml()), 0o644); err != nil {
		return
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-WindowStyle", "Hidden", "-File", filepath.Join(toastDir, "show.ps1"), name, appUserModelID)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// watchDownload waits for a download to finish, then shows a toast that opens
// the containing folder.
func watchDownload(path string) {
	go func() {
		deadline := time.Now().Add(2 * time.Hour)
		var lastSize int64 = -1
		for time.Now().Before(deadline) {
			time.Sleep(700 * time.Millisecond)
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.Size() == lastSize {
				toast{
					Title:  "Download complete",
					Body:   filepath.Base(path),
					Launch: "file:///" + filepath.ToSlash(filepath.Dir(path)),
					Icon:   appIconPath(),
				}.show()
				return
			}
			lastSize = info.Size()
		}
	}()
}
