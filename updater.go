//go:build windows

package main

// Built-in updater (Windows).
//
// How it works:
//   1. Check: the newest GitHub release of releaseRepo is read at start (after
//      a short delay) and every 6 hours, and when you press "Check for updates".
//   2. Download: with automatic checks on, a newer version is downloaded in the
//      background to %LOCALAPPDATA%\Twincxt\updates. Its SHA-256 must match the
//      checksum GitHub publishes for the file (or SHA256SUMS.txt in the
//      release); otherwise it is deleted and never run.
//   3. Install, only when you press "Restart to update": the installer is
//      started on its own (not as a child that could be closed together with
//      Twincxt, the problem Sonara had), Twincxt then closes itself cleanly so
//      logins are saved, and the installer updates in place (same folder, data
//      untouched in %APPDATA%) and starts Twincxt again.
//
// Minimum version: release notes may contain <!-- twincxt-min: 1.3.0 -->.
// Every older version must update: the check runs even with automatic checks
// off, the update cannot be skipped, it downloads at once, and WhatsApp and
// Telegram stay locked behind the settings page until it is installed.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	releaseRepo  = "rashiiddsr/twincxt"
	checkEvery   = 6 * time.Hour
	firstCheck   = 20 * time.Second
	updateTimer  = 4
	installTimer = 5
)

// appVersion is set at build time: -ldflags "-X main.appVersion=1.2.0".
var appVersion = "0.0.0-dev"

type releaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"` // "sha256:<hex>" on current GitHub
}

type release struct {
	Tag    string         `json:"tag_name"`
	Body   string         `json:"body"`
	Draft  bool           `json:"draft"`
	Pre    bool           `json:"prerelease"`
	Assets []releaseAsset `json:"assets"`
}

// updateState mirrors what the settings page shows.
type updateState struct {
	Status   string  `json:"status"` // idle checking latest available downloading ready installing error
	Version  string  `json:"version"`
	Progress float64 `json:"progress"`
	Notes    string  `json:"notes"`
	Error    string  `json:"error"`
	Required bool    `json:"required"`
}

type updater struct {
	a       *app
	mu      sync.Mutex
	state   updateState
	rel     *release
	asset   *releaseAsset
	file    string // verified installer, ready to run
	busy    bool
	toasted string // version already announced with a notification
}

func newUpdater(a *app) *updater {
	return &updater{a: a, state: updateState{Status: "idle"}}
}

func (u *updater) start() {
	pSetTimer.Call(u.a.hwnd, updateTimer, uintptr(firstCheck.Milliseconds()), 0)
}

// onTimer runs on the UI thread.
func (u *updater) onTimer() {
	pSetTimer.Call(u.a.hwnd, updateTimer, uintptr(checkEvery.Milliseconds()), 0)
	// Always looked up so a required update is never missed; with automatic
	// checks off, an optional one is not announced.
	u.check(false)
}

var minVersionRe = regexp.MustCompile(`<!--\s*twincxt-min:\s*v?([0-9][0-9.]*)\s*-->`)

// requiredBy reports whether rel says this version may no longer be used.
func requiredBy(rel *release) bool {
	m := minVersionRe.FindStringSubmatch(rel.Body)
	return m != nil && newerVersion(m[1], appVersion)
}

// set changes the state and shows it in the settings page (any goroutine).
func (u *updater) set(f func(s *updateState)) {
	u.mu.Lock()
	f(&u.state)
	st := u.state
	u.mu.Unlock()
	u.a.ui(func() { u.a.pushUpdate(st) })
}

func (u *updater) snapshot() updateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.state
}

// check looks for a newer release. manual: you pressed the button, so errors
// and "already newest" are shown and nothing is downloaded automatically.
func (u *updater) check(manual bool) {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return
	}
	u.busy = true
	u.mu.Unlock()
	if manual {
		u.set(func(s *updateState) { *s = updateState{Status: "checking"} })
	}
	go func() {
		defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()
		rel, err := fetchLatest()
		if err != nil {
			debugLog("update: check failed: " + err.Error())
			if manual {
				u.set(func(s *updateState) {
					*s = updateState{Status: "error", Error: "Can't check for updates. Check your connection and try again."}
				})
			}
			return
		}
		version := strings.TrimPrefix(rel.Tag, "v")
		asset := pickAsset(rel)
		if !newerVersion(version, appVersion) || asset == nil {
			if manual {
				u.set(func(s *updateState) { *s = updateState{Status: "latest"} })
			}
			return
		}
		required := requiredBy(rel)
		if !manual && !required && (!u.a.cfg.AutoUpdate || version == u.a.cfg.SkippedVersion) {
			return
		}
		u.mu.Lock()
		u.rel, u.asset = rel, asset
		u.mu.Unlock()
		notes := cleanNotes(rel.Body)
		u.set(func(s *updateState) {
			*s = updateState{Status: "available", Version: version, Notes: notes, Required: required}
		})
		if required {
			u.a.ui(func() { u.a.lockForUpdate() })
		}
		if required || (!manual && u.a.cfg.AutoUpdate) {
			u.download(false)
		}
	}()
}

// download fetches and verifies the installer; install afterwards when asked.
func (u *updater) download(thenInstall bool) {
	u.mu.Lock()
	rel, asset := u.rel, u.asset
	if rel == nil || asset == nil || u.state.Status == "downloading" {
		u.mu.Unlock()
		return
	}
	u.mu.Unlock()
	version := strings.TrimPrefix(rel.Tag, "v")
	notes := cleanNotes(rel.Body)
	required := requiredBy(rel)
	go func() {
		u.set(func(s *updateState) {
			*s = updateState{Status: "downloading", Version: version, Notes: notes, Required: required}
		})
		path, err := fetchAndVerify(rel, asset, func(p float64) {
			u.set(func(s *updateState) { s.Progress = p })
		})
		if err != nil {
			debugLog("update: download failed: " + err.Error())
			u.set(func(s *updateState) {
				*s = updateState{Status: "error", Version: version, Error: "Download failed: " + err.Error(), Required: required}
			})
			return
		}
		u.mu.Lock()
		u.file = path
		u.mu.Unlock()
		u.set(func(s *updateState) {
			*s = updateState{Status: "ready", Version: version, Notes: notes, Required: required}
		})
		if thenInstall {
			u.a.ui(u.install)
			return
		}
		u.mu.Lock()
		announce := u.toasted != version
		u.toasted = version
		u.mu.Unlock()
		if announce {
			toast{
				Title:  "Twincxt " + version + " is ready",
				Body:   "Open Settings to install it.",
				Launch: protocolName + ":settings",
				Icon:   appIconPath(),
				Silent: true,
			}.show()
		}
	}()
}

// install starts the verified installer and closes Twincxt (UI thread).
func (u *updater) install() {
	u.mu.Lock()
	file := u.file
	version := u.state.Version
	u.mu.Unlock()
	if file == "" {
		u.download(true)
		return
	}
	if _, err := os.Stat(file); err != nil {
		u.mu.Lock()
		u.file = ""
		u.mu.Unlock()
		u.download(true)
		return
	}
	// ShellExecute starts the installer as its own process (not tied to
	// Twincxt) and lets Windows ask for admin rights when Twincxt is installed
	// for all users. /SILENT shows only a progress window.
	params := `/SILENT /SUPPRESSMSGBOXES /NORESTART /CLOSEAPPLICATIONS /TWINCXTUPDATE`
	r, _, _ := pShellExecute.Call(0, utf16ptr("open"), utf16ptr(file), utf16ptr(params), 0, SW_SHOWNORMAL)
	if r <= 32 {
		u.set(func(s *updateState) {
			*s = updateState{Status: "error", Version: version, Error: "The installer couldn't start. Try again."}
		})
		return
	}
	debugLog("update: installer started for " + version)
	u.set(func(s *updateState) { s.Status = "installing" })
	// Give the installer a moment to appear, then close cleanly so logins are
	// saved before it replaces the files.
	pSetTimer.Call(u.a.hwnd, installTimer, 1500, 0)
}

func (u *updater) skip() {
	st := u.snapshot()
	if st.Version == "" || st.Required {
		return
	}
	u.a.cfg.SkippedVersion = st.Version
	u.a.cfg.save()
	u.set(func(s *updateState) { *s = updateState{Status: "idle"} })
}

// ---- GitHub ------------------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}

func fetchLatest() (*release, error) {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Twincxt-Updater")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub menjawab %d", resp.StatusCode)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.Draft || rel.Pre {
		return nil, errors.New("rilis terbaru bukan rilis final")
	}
	return &rel, nil
}

func pickAsset(rel *release) *releaseAsset {
	for i := range rel.Assets {
		n := strings.ToLower(rel.Assets[i].Name)
		if strings.HasPrefix(n, "twincxt-setup-") && strings.HasSuffix(n, ".exe") {
			return &rel.Assets[i]
		}
	}
	return nil
}

// expectedSHA256 returns the checksum GitHub lists for the asset, or the one
// in the release's SHA256SUMS.txt.
func expectedSHA256(rel *release, asset *releaseAsset) (string, error) {
	if d := strings.TrimPrefix(strings.ToLower(asset.Digest), "sha256:"); len(d) == 64 {
		return d, nil
	}
	for _, a := range rel.Assets {
		if n := strings.ToLower(a.Name); !strings.HasPrefix(n, "sha256sums") || !strings.HasSuffix(n, ".txt") {
			continue
		}
		req, _ := http.NewRequest("GET", a.URL, nil)
		req.Header.Set("User-Agent", "Twincxt-Updater")
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset.Name && len(f[0]) == 64 {
				return strings.ToLower(f[0]), nil
			}
		}
	}
	return "", errors.New("no checksum published for this file")
}

func updatesDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, appName, "updates")
}

func fetchAndVerify(rel *release, asset *releaseAsset, progress func(float64)) (string, error) {
	want, err := expectedSHA256(rel, asset)
	if err != nil {
		return "", err
	}
	dir := updatesDir()
	_ = os.MkdirAll(dir, 0o755)
	final := filepath.Join(dir, filepath.Base(asset.Name))
	// Reuse a previous, complete download of the same file.
	if got, err := fileSHA256(final); err == nil && got == want {
		progress(1)
		return final, nil
	}
	// Remove older installers.
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	req, _ := http.NewRequest("GET", asset.URL, nil)
	req.Header.Set("User-Agent", "Twincxt-Updater")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("server menjawab %d", resp.StatusCode)
	}
	partial := final + ".partial"
	f, err := os.Create(partial)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	total := resp.ContentLength
	if total <= 0 {
		total = asset.Size
	}
	var done int64
	last := time.Now()
	buf := make([]byte, 64<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return "", werr
			}
			h.Write(buf[:n])
			done += int64(n)
			if total > 0 && time.Since(last) > 200*time.Millisecond {
				progress(float64(done) / float64(total))
				last = time.Now()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			return "", rerr
		}
	}
	f.Close()
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(partial)
		return "", errors.New("file doesn't match the release checksum")
	}
	if err := os.Rename(partial, final); err != nil {
		return "", err
	}
	progress(1)
	return final, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var versionPart = regexp.MustCompile(`\d+`)

// newerVersion reports whether a is a higher version than b ("1.10.0" > "1.9.2").
func newerVersion(a, b string) bool {
	pa, pb := versionPart.FindAllString(a, 4), versionPart.FindAllString(strings.SplitN(b, "-", 2)[0], 4)
	for i := 0; i < 4; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// cleanNotes turns release notes into short plain text for the settings page.
func cleanNotes(body string) string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "<!--") {
			continue
		}
		line = strings.TrimLeft(line, "#")
		line = strings.ReplaceAll(strings.ReplaceAll(line, "**", ""), "`", "")
		out = append(out, strings.TrimSpace(line))
		if len(out) >= 12 {
			out = append(out, "…")
			break
		}
	}
	return strings.Join(out, "\n")
}

// ---- running work on the UI thread -------------------------------------------

const WM_RUNUI = WM_APP + 2

var (
	uiMu    sync.Mutex
	uiQueue []func()
)

// ui runs f on the UI thread (COM and window calls must happen there).
func (a *app) ui(f func()) {
	uiMu.Lock()
	uiQueue = append(uiQueue, f)
	uiMu.Unlock()
	pPostMessage.Call(a.hwnd, WM_RUNUI, 0, 0)
}

func runUIQueue() {
	uiMu.Lock()
	q := uiQueue
	uiQueue = nil
	uiMu.Unlock()
	for _, f := range q {
		f()
	}
}

func (a *app) pushUpdate(st updateState) {
	if !a.shellOK {
		return
	}
	b, _ := json.Marshal(st)
	a.shell.Eval("twincxt.update(" + string(b) + ")")
}
