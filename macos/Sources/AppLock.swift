// App lock: a PIN that hides WhatsApp and Telegram behind a lock screen
// (the same rules as applock.go on Windows).
//
//   - The PIN (4 to 8 digits) is never stored: only a PBKDF2-HMAC-SHA256 hash
//     (200,000 rounds, random 16-byte salt) in config.json.
//   - Twincxt locks when it starts, when you press the lock button / ⌘L / the
//     menu's Lock, and after it has been in the background for lockAfter
//     minutes (0 = only on start).
//   - Wrong PINs: after 5 in a row, each further try waits 30 s, doubling up
//     to 5 minutes. The count survives restarts.
//   - Forgot the PIN: "Forgot PIN?" signs out of WhatsApp and Telegram (their
//     site data is deleted) and removes the PIN.
//
// This keeps people at your desk out of your chats. It does not encrypt the
// data on disk.

import AppKit
import WebKit
import CommonCrypto
import Security

private let PIN_ROUNDS: UInt32 = 200_000
private let FREE_TRIES = 5
private let FIRST_WAIT: Int64 = 30
private let MAX_WAIT: Int64 = 5 * 60
let LOCK_CHECK_EVERY: TimeInterval = 15

private func hexBytes(_ s: String) -> [UInt8] {
  var out: [UInt8] = []
  var i = s.startIndex
  while i < s.endIndex, let j = s.index(i, offsetBy: 2, limitedBy: s.endIndex) {
    guard let b = UInt8(s[i..<j], radix: 16) else { return [] }
    out.append(b)
    i = j
  }
  return out
}

private func hexString(_ b: [UInt8]) -> String { b.map { String(format: "%02x", $0) }.joined() }

func hashPin(_ pin: String, _ saltHex: String) -> String {
  let salt = hexBytes(saltHex)
  let pwLen = pin.utf8.count
  let keyLen = 32
  var key = [UInt8](repeating: 0, count: keyLen)
  let rc = pin.withCString { pw -> Int32 in
    CCKeyDerivationPBKDF(CCPBKDFAlgorithm(kCCPBKDF2), pw, pwLen, salt, salt.count,
                         CCPseudoRandomAlgorithm(kCCPRFHmacAlgSHA256), PIN_ROUNDS, &key, keyLen)
  }
  return rc == Int32(kCCSuccess) ? hexString(key) : ""
}

func newSalt() -> String {
  var b = [UInt8](repeating: 0, count: 16)
  if SecRandomCopyBytes(kSecRandomDefault, b.count, &b) != errSecSuccess {
    for i in 0..<b.count { b[i] = UInt8.random(in: 0...255) }
  }
  return hexString(b)
}

private func sameText(_ a: String, _ b: String) -> Bool {
  let x = Array(a.utf8), y = Array(b.utf8)
  if x.count != y.count { return false }
  var d: UInt8 = 0
  for i in 0..<x.count { d |= x[i] ^ y[i] }
  return d == 0
}

private func plural(_ n: Int64, _ one: String, _ many: String) -> String { n == 1 ? one : many }

private func waitText(_ seconds: Int64) -> String {
  let s = max(1, seconds)
  if s >= 60 && s % 60 == 0 { return "\(s / 60) \(plural(s / 60, "minute", "minutes"))" }
  return "\(s) \(plural(s, "second", "seconds"))"
}

private func validPin(_ p: String) -> Bool {
  (4...8).contains(p.count) && p.allSatisfy { ("0"..."9").contains($0) }
}

private func lockKind(_ k: String) -> String { k == "password" ? "password" : "pin" }

// Validates a new PIN (4 to 8 digits) or password (6 to 128 characters); "" when fine.
private func checkSecret(_ kind: String, _ v: String) -> String {
  if kind == "password" { return (6...128).contains(v.count) ? "" : "Use at least 6 characters." }
  return validPin(v) ? "" : "Use 4 to 8 digits."
}

// CI check against a known PBKDF2 result.
func lockSelfTest() -> Bool {
  let got = hashPin("1234", "00112233445566778899aabbccddeeff")
  let want = "98cd56f19733a361768b76706415776c6f3e161aef907b7e88c5ea3fe5639ab7"
  print("selftest: pbkdf2 \(got) \(got == want ? "ok" : "MISMATCH, want " + want)")
  let salt = newSalt()
  let saltOK = salt.count == 32 && salt != newSalt()
  print("selftest: salt \(salt) \(saltOK ? "ok" : "BAD")")
  return got == want && saltOK
}

extension AppDelegate {
  // Runs once at startup, before the window is shown.
  func startLock() {
    if cfg.lockEnabled && !cfg.lockHash.isEmpty { appLocked = true }
    let nc = NotificationCenter.default
    nc.addObserver(forName: NSApplication.didResignActiveNotification, object: nil, queue: .main) { [weak self] _ in self?.onActivate(false) }
    nc.addObserver(forName: NSApplication.didHideNotification, object: nil, queue: .main) { [weak self] _ in self?.onActivate(false) }
    nc.addObserver(forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in self?.onActivate(true) }
    nc.addObserver(forName: NSApplication.didUnhideNotification, object: nil, queue: .main) { [weak self] _ in self?.onActivate(true) }
    lockTimer = Timer.scheduledTimer(withTimeInterval: LOCK_CHECK_EVERY, repeats: true) { _ in
      DispatchQueue.main.async { (NSApp.delegate as? AppDelegate)?.onLockTimer() }
    }
  }

  // Twincxt is "away" when it is not the active app, or its window is hidden or in the Dock.
  var isAway: Bool {
    guard let w = window else { return true }
    return !NSApp.isActive || NSApp.isHidden || !w.isVisible || w.isMiniaturized
  }

  func lockNow() {
    if !cfg.lockEnabled || appLocked { return }
    dlog("lock: locked")
    appLocked = true
    layout()
    pushState()
    applyStrip()
    focusActive()
  }

  func unlockApp() {
    appLocked = false
    awaySince = nil
    dlog("lock: unlocked")
    layout()
    pushState()
    applyStrip()
    focusActive()
  }

  func onActivate(_ active: Bool) {
    if !active || isAway {
      if awaySince == nil { awaySince = Date() }
      return
    }
    checkAway()
    awaySince = nil
  }

  // Locks when Twincxt has been in the background long enough. It runs on a
  // timer too, so the window is already locked when it comes back.
  func checkAway() {
    guard cfg.lockEnabled, !appLocked, cfg.lockAfter > 0, !inCall, let since = awaySince else { return }
    if Date().timeIntervalSince(since) >= TimeInterval(cfg.lockAfter * 60) { lockNow() }
  }

  // A call is going on: a call window is open, or a page uses the camera or
  // the microphone. No automatic lock then.
  var inCall: Bool {
    if !popups.isEmpty { return true }
    var views: [WKWebView] = services.values.compactMap { $0.view }
    views += popups.map { $0.view }
    return views.contains { $0.cameraCaptureState == .active || $0.microphoneCaptureState == .active }
  }

  func onLockTimer() {
    if inCall { awaySince = nil; return }
    if isAway {
      if awaySince == nil { awaySince = Date() }
      checkAway()
    }
  }

  // Verifies a PIN and applies the wrong-PIN delays. Returns "" when the PIN
  // is right, otherwise the message for the lock screen.
  func checkPin(_ pin: String) -> String {
    let now = Int64(Date().timeIntervalSince1970)
    if cfg.lockUntil > now {
      return "Too many tries. Try again in \(waitText(cfg.lockUntil - now))."
    }
    let want = cfg.lockHash
    let got = hashPin(pin, cfg.lockSalt)
    if !want.isEmpty && !got.isEmpty && sameText(want, got) {
      cfg.lockFails = 0
      cfg.lockUntil = 0
      cfg.save()
      return ""
    }
    cfg.lockFails += 1
    let name = cfg.lockType == "password" ? "password" : "PIN"
    var msg = "Wrong \(name)."
    if cfg.lockFails >= FREE_TRIES {
      let shift = cfg.lockFails - FREE_TRIES
      var wait = shift < 20 ? FIRST_WAIT << Int64(shift) : MAX_WAIT
      if wait > MAX_WAIT || wait <= 0 { wait = MAX_WAIT }
      cfg.lockUntil = now + wait
      msg = "Wrong \(name). Try again in \(waitText(wait))."
    } else {
      let left = Int64(FREE_TRIES - cfg.lockFails)
      if left <= 2 { msg = "Wrong \(name). \(left) \(plural(left, "try", "tries")) left before a pause." }
    }
    cfg.save()
    return msg
  }

  // The shell's "lock" messages: {op:"set"|"change"|"disable"|"unlock"|"now"|"reset", pin, old}.
  func onLockMessage(_ data: Any?) {
    let m = data as? [String: Any] ?? [:]
    let op = m["op"] as? String ?? ""
    let pin = m["pin"] as? String ?? ""
    let old = m["old"] as? String ?? ""
    let kind = lockKind(m["kind"] as? String ?? "")
    func reply(_ ok: Bool, _ msg: String) {
      shellEval("twincxt.lockResult(" + json(["op": op, "ok": ok, "error": msg] as [String: Any]) + ")")
    }
    switch op {
    case "set":
      if cfg.lockEnabled { reply(false, "App lock is already on."); return }
      let bad = checkSecret(kind, pin)
      if !bad.isEmpty { reply(false, bad); return }
      setPin(pin)
      cfg.lockType = kind
      cfg.lockEnabled = true
      cfg.save()
      dlog("lock: turned on")
      reply(true, "")
      pushState()
    case "change":
      let msg = checkPin(old)
      if !msg.isEmpty { reply(false, msg); return }
      let bad = checkSecret(kind, pin)
      if !bad.isEmpty { reply(false, bad); return }
      setPin(pin)
      cfg.lockType = kind
      cfg.save()
      reply(true, "")
      pushState()
    case "disable":
      let msg = checkPin(old)
      if !msg.isEmpty { reply(false, msg); return }
      clearLock()
      dlog("lock: turned off")
      reply(true, "")
      pushState()
    case "unlock":
      if !appLocked { return }
      let msg = checkPin(pin)
      if !msg.isEmpty { reply(false, msg); return }
      reply(true, "")
      unlockApp()
    case "now":
      lockNow()
    case "reset":
      if !appLocked { return }
      dlog("lock: forgot PIN, signing out of both services")
      runAction("reset:wa")
      runAction("reset:tg")
      clearLock()
      reply(true, "")
      unlockApp()
    default:
      break
    }
  }

  func setPin(_ pin: String) {
    cfg.lockSalt = newSalt()
    cfg.lockHash = hashPin(pin, cfg.lockSalt)
    cfg.lockFails = 0
    cfg.lockUntil = 0
  }

  func clearLock() {
    cfg.lockEnabled = false
    cfg.lockType = "pin"
    cfg.lockHash = ""
    cfg.lockSalt = ""
    cfg.lockFails = 0
    cfg.lockUntil = 0
    cfg.save()
  }
}
