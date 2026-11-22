//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"unsafe"
)

type windowState struct {
	X, Y, W, H int32
	Maximized  bool
}

type config struct {
	Active          string      `json:"active"`
	Theme           string      `json:"theme"`
	ZoomWA          int         `json:"zoomWa"`
	ZoomTG          int         `json:"zoomTg"`
	Autostart       bool        `json:"autostart"`
	StartHidden     bool        `json:"startHidden"`
	CloseAction     string      `json:"closeAction"`
	NotifyWA        bool        `json:"notifyWa"`
	NotifyTG        bool        `json:"notifyTg"`
	TelegramVersion string      `json:"telegramVersion"`
	NotifyMode      string      `json:"notifyMode"` // toast | attention | both
	AutoUpdate      bool        `json:"autoUpdate"`
	SkippedVersion  string      `json:"skippedVersion"`
	BlurMessages    bool        `json:"blurMessages"`
	BlurPreviews    bool        `json:"blurPreviews"`
	BlurNames       bool        `json:"blurNames"`
	BlurPhotos      bool        `json:"blurPhotos"`
	BlurMedia       bool        `json:"blurMedia"`
	LockEnabled     bool        `json:"lockEnabled"`
	LockType        string      `json:"lockType"` // pin | password
	LockBiometric   bool        `json:"lockBiometric"`
	LockHash        string      `json:"lockHash"`  // PBKDF2-HMAC-SHA256 of the PIN, hex
	LockSalt        string      `json:"lockSalt"`  // hex
	LockAfter       int         `json:"lockAfter"` // minutes in the background; 0 = only on start
	LockFails       int         `json:"lockFails"`
	LockUntil       int64       `json:"lockUntil"` // unix seconds; no tries before this
	Window          windowState `json:"window"`
}

func defaultConfig() config {
	return config{
		Active:          "wa",
		Theme:           "system",
		ZoomWA:          100,
		ZoomTG:          100,
		CloseAction:     "tray",
		NotifyWA:        true,
		NotifyTG:        true,
		TelegramVersion: "a",
		NotifyMode:      "toast",
		AutoUpdate:      true,
		LockAfter:       5,
	}
}

func appDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = os.Getenv("APPDATA")
	}
	return filepath.Join(dir, appName)
}

func configPath() string { return filepath.Join(appDataDir(), "config.json") }

func loadConfig() config {
	c := defaultConfig()
	if b, err := os.ReadFile(configPath()); err == nil {
		// Tolerate a UTF-8 BOM (added by some editors); json.Unmarshal rejects it
		// and every setting would silently fall back to its default.
		b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
		_ = json.Unmarshal(b, &c)
	}
	if c.Active != "wa" && c.Active != "tg" {
		c.Active = "wa"
	}
	if c.Theme != "light" && c.Theme != "dark" {
		c.Theme = "system"
	}
	if c.CloseAction != "quit" {
		c.CloseAction = "tray"
	}
	if c.LockAfter != 0 && c.LockAfter != 1 && c.LockAfter != 5 && c.LockAfter != 15 {
		c.LockAfter = 5
	}
	if c.LockType != "password" {
		c.LockType = "pin"
	}
	if c.LockEnabled && c.LockHash == "" {
		c.LockEnabled = false
	}
	if c.NotifyMode != "attention" && c.NotifyMode != "both" {
		c.NotifyMode = "toast"
	}
	if c.TelegramVersion != "k" {
		c.TelegramVersion = "a"
	}
	c.ZoomWA = clampZoom(c.ZoomWA)
	c.ZoomTG = clampZoom(c.ZoomTG)
	c.Autostart = autostartEnabled()
	return c
}

func clampZoom(z int) int {
	if z < 50 || z > 200 {
		return 100
	}
	return z
}

func (c config) save() {
	_ = os.MkdirAll(appDataDir(), 0o755)
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := configPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, configPath())
	}
}

// ---- autostart (HKCU\...\Run) ------------------------------------------------

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func autostartCommand() string {
	exe, _ := os.Executable()
	return `"` + exe + `" --autostart`
}

func autostartEnabled() bool {
	var buf [1024]uint16
	size := uint32(len(buf) * 2)
	r, _, _ := pRegGetValue.Call(HKEY_CURRENT_USER, utf16ptr(runKey), utf16ptr(appName),
		RRF_RT_REG_SZ, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	return r == 0
}

func setAutostart(on bool) {
	if !on {
		pRegDeleteKeyValue.Call(HKEY_CURRENT_USER, utf16ptr(runKey), utf16ptr(appName))
		return
	}
	cmd := autostartCommand()
	data := utf16(cmd)
	size := uintptr((len(cmd) + 1) * 2)
	pRegSetKeyValue.Call(HKEY_CURRENT_USER, utf16ptr(runKey), utf16ptr(appName), REG_SZ,
		uintptr(unsafe.Pointer(data)), size)
}

// migrateLegacyProfile moves the WebView2 profile of the old single-app
// "WhatsApp Desktop" build so the existing WhatsApp login keeps working.
func migrateLegacyProfile(dataDir string) {
	if _, err := os.Stat(dataDir); err == nil {
		return
	}
	legacy := filepath.Join(filepath.Dir(appDataDir()), "WhatsAppDesktopLight", "UserData")
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(dataDir), 0o755)
	_ = os.Rename(legacy, dataDir)
}
