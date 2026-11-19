// Settings file and debug log. Same keys and defaults as the Electron app
// (desktop/main.js) plus the privacy blur switches.

import Foundation

let env = ProcessInfo.processInfo.environment

// For tests: TWINCXT_USERDATA keeps the config and the update cache in another folder.
let dataDir: URL = {
  if let p = env["TWINCXT_USERDATA"], !p.isEmpty { return URL(fileURLWithPath: p, isDirectory: true) }
  let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
  return base.appendingPathComponent("Twincxt", isDirectory: true)
}()

let debugOn = !(env["TWINCXT_DEBUG"] ?? "").isEmpty

func dlog(_ line: String) {
  guard debugOn else { return }
  let f = DateFormatter()
  f.dateFormat = "HH:mm:ss.SSS"
  let text = f.string(from: Date()) + "  " + line + "\n"
  FileHandle.standardError.write(text.data(using: .utf8) ?? Data())
  let url = dataDir.appendingPathComponent("debug.log")
  try? FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
  if let h = try? FileHandle(forWritingTo: url) {
    h.seekToEndOfFile()
    h.write(text.data(using: .utf8) ?? Data())
    h.closeFile()
  } else {
    try? text.data(using: .utf8)?.write(to: url)
  }
}

let BLUR_KEYS = ["blurMessages", "blurPreviews", "blurNames", "blurPhotos", "blurMedia"]

final class Config {
  var active = "wa"
  var theme = "system"
  var zoomWa = 100
  var zoomTg = 100
  var autostart = false
  var startHidden = false
  var closeAction = "tray"
  var notifyWa = true
  var notifyTg = true
  var notifyMode = "toast"
  var telegramVersion = "a"
  var autoUpdate = true
  var skippedVersion = ""
  var blur: [String: Bool] = [:]
  // App lock (AppLock.swift).
  var lockEnabled = false
  var lockHash = "" // PBKDF2-HMAC-SHA256 of the PIN, hex
  var lockSalt = "" // hex
  var lockAfter = 5 // minutes in the background; 0 = only on start
  var lockFails = 0
  var lockUntil: Int64 = 0 // unix seconds; no tries before this
  // Window frame, top-left origin on the primary screen (the Electron app's convention).
  var winX: Double = 0
  var winY: Double = 0
  var winW: Double = 1240
  var winH: Double = 800
  var winMax = false

  private var raw: [String: Any] = [:]
  let file = dataDir.appendingPathComponent("config.json")

  static func load() -> Config {
    let c = Config()
    for k in BLUR_KEYS { c.blur[k] = false }
    if var data = try? Data(contentsOf: c.file) {
      if data.starts(with: [0xEF, 0xBB, 0xBF]) { data = data.dropFirst(3) }
      if let obj = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] {
        c.raw = obj
        c.read(obj)
      }
    }
    return c
  }

  private func read(_ d: [String: Any]) {
    func str(_ k: String, _ allowed: [String], _ def: String) -> String {
      if let v = d[k] as? String, allowed.contains(v) { return v }
      return def
    }
    func bool(_ k: String, _ def: Bool) -> Bool { (d[k] as? NSNumber)?.boolValue ?? def }
    func zoom(_ k: String) -> Int {
      if let v = (d[k] as? NSNumber)?.doubleValue, v >= 50, v <= 200 { return Int(v.rounded()) }
      return 100
    }
    active = str("active", ["wa", "tg"], "wa")
    theme = str("theme", ["light", "dark", "system"], "system")
    zoomWa = zoom("zoomWa")
    zoomTg = zoom("zoomTg")
    autostart = bool("autostart", false)
    startHidden = bool("startHidden", false)
    closeAction = str("closeAction", ["tray", "quit"], "tray")
    notifyWa = bool("notifyWa", true)
    notifyTg = bool("notifyTg", true)
    notifyMode = str("notifyMode", ["toast", "attention", "both"], "toast")
    telegramVersion = str("telegramVersion", ["a", "k"], "a")
    autoUpdate = bool("autoUpdate", true)
    skippedVersion = d["skippedVersion"] as? String ?? ""
    for k in BLUR_KEYS { blur[k] = bool(k, false) }
    lockEnabled = bool("lockEnabled", false)
    lockHash = d["lockHash"] as? String ?? ""
    lockSalt = d["lockSalt"] as? String ?? ""
    lockAfter = (d["lockAfter"] as? NSNumber)?.intValue ?? 5
    if ![0, 1, 5, 15].contains(lockAfter) { lockAfter = 5 }
    lockFails = max(0, (d["lockFails"] as? NSNumber)?.intValue ?? 0)
    lockUntil = (d["lockUntil"] as? NSNumber)?.int64Value ?? 0
    if lockEnabled && lockHash.isEmpty { lockEnabled = false }
    if let w = d["window"] as? [String: Any] {
      func n(_ k: String, _ def: Double) -> Double { (w[k] as? NSNumber)?.doubleValue ?? def }
      winX = n("x", 0)
      winY = n("y", 0)
      winW = n("w", 1240)
      winH = n("h", 800)
      winMax = (w["maximized"] as? NSNumber)?.boolValue ?? false
    }
  }

  func dict() -> [String: Any] {
    var d = raw // keys this version does not know are kept
    d["active"] = active
    d["theme"] = theme
    d["zoomWa"] = zoomWa
    d["zoomTg"] = zoomTg
    d["autostart"] = autostart
    d["startHidden"] = startHidden
    d["closeAction"] = closeAction
    d["notifyWa"] = notifyWa
    d["notifyTg"] = notifyTg
    d["notifyMode"] = notifyMode
    d["telegramVersion"] = telegramVersion
    d["autoUpdate"] = autoUpdate
    d["skippedVersion"] = skippedVersion
    for k in BLUR_KEYS { d[k] = blur[k] ?? false }
    d["lockEnabled"] = lockEnabled
    d["lockHash"] = lockHash
    d["lockSalt"] = lockSalt
    d["lockAfter"] = lockAfter
    d["lockFails"] = lockFails
    d["lockUntil"] = lockUntil
    d["window"] = ["x": winX, "y": winY, "w": winW, "h": winH, "maximized": winMax] as [String: Any]
    return d
  }

  func save() {
    do {
      try FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
      let data = try JSONSerialization.data(withJSONObject: dict(), options: [.prettyPrinted, .sortedKeys])
      try data.write(to: file, options: .atomic)
    } catch {
      dlog("config: \(error.localizedDescription)")
    }
  }
}

// JSON text for a value (object, array, string, number or bool).
func json(_ v: Any) -> String {
  guard let d = try? JSONSerialization.data(withJSONObject: v, options: [.fragmentsAllowed]),
        let s = String(data: d, encoding: .utf8) else { return "null" }
  return s
}

func parseJSON(_ s: String) -> [String: Any]? {
  guard let d = s.data(using: .utf8) else { return nil }
  return (try? JSONSerialization.jsonObject(with: d)) as? [String: Any]
}
