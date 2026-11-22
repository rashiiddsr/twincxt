package main

// App lock: a PIN that hides WhatsApp and Telegram behind a lock screen.
//
//   - The PIN (4 to 8 digits) is never stored: only a PBKDF2-HMAC-SHA256 hash
//     (200,000 rounds, random 16-byte salt) in config.json.
//   - Twincxt locks when it starts, when you press the lock button / Ctrl+L /
//     the tray's Lock, and after it has been in the background for LockAfter
//     minutes (0 = only on start). A call window counts as Twincxt, and no
//     automatic lock happens during a call (a call window is open or a page
//     is playing sound).
//   - Wrong PINs: after 5 in a row, each further try waits 30 s, doubling up
//     to 5 minutes. The count survives restarts.
//   - Forgot the PIN: "Forgot PIN?" signs out of WhatsApp and Telegram (their
//     site data is deleted) and removes the PIN. Whoever holds the phone can
//     sign back in by scanning the QR codes; nobody else sees the chats.
//
// This keeps people at your desk out of your chats. It does not encrypt the
// data on disk.

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

const (
	lockTimer      = 6
	pinRounds      = 200000
	freeTries      = 5
	firstWait      = 30 * time.Second
	maxWait        = 5 * time.Minute
	lockCheckEvery = 15 * time.Second
)

var pinRe = regexp.MustCompile(`^[0-9]{4,8}$`)

func hashPin(pin, saltHex string) string {
	salt, _ := hex.DecodeString(saltHex)
	key, err := pbkdf2.Key(sha256.New, pin, salt, pinRounds, 32)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(key)
}

func newSalt() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// startLock runs once at startup, before the window is shown.
func (a *app) startLock() {
	if a.cfg.LockEnabled && a.cfg.LockHash != "" {
		a.appLocked = true
	}
	pSetTimer.Call(a.hwnd, lockTimer, uintptr(lockCheckEvery.Milliseconds()), 0)
}

func (a *app) lockNow() {
	if !a.cfg.LockEnabled || a.appLocked {
		return
	}
	debugLog("lock: locked")
	a.appLocked = true
	a.layout()
	a.pushState()
	a.applyStrip()
	a.focusActive()
	a.updateTray()
}

func (a *app) unlockApp() {
	a.appLocked = false
	a.awaySince = time.Time{}
	debugLog("lock: unlocked")
	a.layout()
	a.pushState()
	a.applyStrip()
	a.focusActive()
	a.updateTray()
}

// onActivate is called on WM_ACTIVATE: active=false when Twincxt goes to the
// background.
func (a *app) onActivate(active bool) {
	if !active {
		if a.awaySince.IsZero() {
			a.awaySince = time.Now()
		}
		return
	}
	a.checkAway()
	a.awaySince = time.Time{}
}

// checkAway locks when Twincxt has been in the background long enough. It
// runs on a timer too, so the window is already locked when it comes back
// (and in the taskbar preview).
func (a *app) checkAway() {
	if !a.cfg.LockEnabled || a.appLocked || a.cfg.LockAfter <= 0 || a.awaySince.IsZero() || a.inCall() {
		return
	}
	if time.Since(a.awaySince) >= time.Duration(a.cfg.LockAfter)*time.Minute {
		a.lockNow()
	}
}

// inCall reports a call in progress: a call window is open, or WhatsApp or
// Telegram is playing sound (a call in the main window, a voice note).
func (a *app) inCall() bool {
	if len(a.popups) > 0 {
		return true
	}
	for _, s := range a.svcs {
		if s.wv != nil && s.wv.PlayingAudio() {
			return true
		}
	}
	return false
}

func (a *app) onLockTimer() {
	if a.inCall() {
		a.awaySince = time.Time{}
		return
	}
	fg, _, _ := pGetForegroundWindow.Call()
	if _, ours := a.popups[fg]; ours {
		a.awaySince = time.Time{}
		return
	}
	if fg != a.hwnd || !isVisible(a.hwnd) || isIconic(a.hwnd) {
		if a.awaySince.IsZero() {
			a.awaySince = time.Now()
		}
		a.checkAway()
	}
}

// checkPin verifies a PIN and applies the wrong-PIN delays. It returns "" when
// the PIN is right, otherwise the message for the lock screen.
func (a *app) checkPin(pin string) string {
	now := time.Now()
	if a.cfg.LockUntil > now.Unix() {
		return fmt.Sprintf("Too many tries. Try again in %s.", waitText(time.Unix(a.cfg.LockUntil, 0).Sub(now)))
	}
	want := a.cfg.LockHash
	got := hashPin(pin, a.cfg.LockSalt)
	if want != "" && got != "" && subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1 {
		a.cfg.LockFails = 0
		a.cfg.LockUntil = 0
		a.cfg.save()
		return ""
	}
	a.cfg.LockFails++
	msg := "Wrong PIN."
	if a.cfg.LockFails >= freeTries {
		wait := firstWait << uint(a.cfg.LockFails-freeTries)
		if wait > maxWait || wait <= 0 {
			wait = maxWait
		}
		a.cfg.LockUntil = now.Add(wait).Unix()
		msg = fmt.Sprintf("Wrong PIN. Try again in %s.", waitText(wait))
	} else if left := freeTries - a.cfg.LockFails; left <= 2 {
		msg = fmt.Sprintf("Wrong PIN. %d %s left before a pause.", left, plural(left, "try", "tries"))
	}
	a.cfg.save()
	return msg
}

func waitText(d time.Duration) string {
	s := int(d.Round(time.Second) / time.Second)
	if s < 1 {
		s = 1
	}
	if s >= 60 && s%60 == 0 {
		return fmt.Sprintf("%d %s", s/60, plural(s/60, "minute", "minutes"))
	}
	return fmt.Sprintf("%d %s", s, plural(s, "second", "seconds"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// onLockMessage handles the shell's "lock" messages:
// {op:"set"|"change"|"disable"|"unlock"|"now"|"reset", pin, old}.
func (a *app) onLockMessage(data json.RawMessage) {
	var m struct {
		Op  string `json:"op"`
		Pin string `json:"pin"`
		Old string `json:"old"`
	}
	_ = json.Unmarshal(data, &m)
	reply := func(ok bool, msg string) {
		b, _ := json.Marshal(map[string]any{"op": m.Op, "ok": ok, "error": msg})
		a.shell.Eval("twincxt.lockResult(" + string(b) + ")")
	}
	switch m.Op {
	case "set":
		if a.cfg.LockEnabled {
			reply(false, "App lock is already on.")
			return
		}
		if !pinRe.MatchString(m.Pin) {
			reply(false, "Use 4 to 8 digits.")
			return
		}
		a.setPin(m.Pin)
		a.cfg.LockEnabled = true
		a.cfg.save()
		debugLog("lock: turned on")
		reply(true, "")
		a.pushState()
		a.updateTray()
	case "change":
		if msg := a.checkPin(m.Old); msg != "" {
			reply(false, msg)
			return
		}
		if !pinRe.MatchString(m.Pin) {
			reply(false, "Use 4 to 8 digits.")
			return
		}
		a.setPin(m.Pin)
		a.cfg.save()
		reply(true, "")
	case "disable":
		if msg := a.checkPin(m.Old); msg != "" {
			reply(false, msg)
			return
		}
		a.clearLock()
		debugLog("lock: turned off")
		reply(true, "")
		a.pushState()
		a.updateTray()
	case "unlock":
		if !a.appLocked {
			return
		}
		if msg := a.checkPin(m.Pin); msg != "" {
			reply(false, msg)
			return
		}
		reply(true, "")
		a.unlockApp()
	case "now":
		a.lockNow()
	case "reset":
		if !a.appLocked {
			return
		}
		debugLog("lock: forgot PIN, signing out of both services")
		a.runAction("reset:wa")
		a.runAction("reset:tg")
		a.clearLock()
		reply(true, "")
		a.unlockApp()
	}
}

func (a *app) setPin(pin string) {
	a.cfg.LockSalt = newSalt()
	a.cfg.LockHash = hashPin(pin, a.cfg.LockSalt)
	a.cfg.LockFails = 0
	a.cfg.LockUntil = 0
}

func (a *app) clearLock() {
	a.cfg.LockEnabled = false
	a.cfg.LockHash = ""
	a.cfg.LockSalt = ""
	a.cfg.LockFails = 0
	a.cfg.LockUntil = 0
	a.cfg.save()
}
