// Built-in updater (the same flow as desktop/updater.js and updater.go).
//
//   1. Check: the newest GitHub release is read 20 s after start, every 6 hours
//      and when "Check for updates" is pressed.
//   2. Download: with automatic checks on, a newer version downloads in the
//      background. Its SHA-256 must match the checksum GitHub lists for the
//      file (or SHA256SUMS-mac.txt in the release), otherwise it is deleted.
//   3. Install, only when you press "Restart to update": the new Twincxt.app
//      replaces the running one in place, then opens again. Files downloaded by
//      Twincxt itself carry no quarantine flag, so macOS does not block it.
//
// Minimum version: release notes may contain <!-- twincxt-min: 1.3.0 -->;
// older versions then must update (no skipping, services locked until done).

import Foundation
import CryptoKit

let REPO = "rashiiddsr/twincxt"
private let FIRST_CHECK: TimeInterval = 20
private let CHECK_EVERY: TimeInterval = 6 * 60 * 60
private let UA = "Twincxt-Updater"

struct UpdateError: LocalizedError {
  let message: String
  init(_ m: String) { message = m }
  var errorDescription: String? { message }
}

private func numbers(_ s: String) -> [Int] {
  guard let re = try? NSRegularExpression(pattern: "\\d+") else { return [] }
  let ns = s as NSString
  return re.matches(in: s, range: NSRange(location: 0, length: ns.length)).prefix(4).map { Int(ns.substring(with: $0.range)) ?? 0 }
}

// Is version a newer than version b?
func versionNewer(_ a: String, _ b: String) -> Bool {
  let pa = numbers(a)
  let pb = numbers(b.components(separatedBy: "-")[0])
  for i in 0..<4 {
    let x = i < pa.count ? pa[i] : 0
    let y = i < pb.count ? pb[i] : 0
    if x != y { return x > y }
  }
  return false
}

private func cleanNotes(_ body: String) -> String {
  var out: [String] = []
  for l in body.replacingOccurrences(of: "\r", with: "").components(separatedBy: "\n") {
    var line = l.trimmingCharacters(in: .whitespaces)
    if line.isEmpty || line.hasPrefix("<!--") { continue }
    while line.hasPrefix("#") { line.removeFirst() }
    line = line.replacingOccurrences(of: "**", with: "").replacingOccurrences(of: "`", with: "")
    out.append(line.trimmingCharacters(in: .whitespaces))
    if out.count >= 12 { out.append("…"); break }
  }
  return out.joined(separator: "\n")
}

private func sha256(of url: URL) throws -> String {
  let h = try FileHandle(forReadingFrom: url)
  defer { h.closeFile() }
  var hasher = SHA256()
  while true {
    let chunk = h.readData(ofLength: 1 << 20)
    if chunk.isEmpty { break }
    hasher.update(data: chunk)
  }
  return hasher.finalize().map { String(format: "%02x", $0) }.joined()
}

private func shellQuote(_ s: String) -> String {
  "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
}

private func request(_ url: URL, api: Bool) -> URLRequest {
  var r = URLRequest(url: url)
  r.timeoutInterval = 60
  r.setValue(UA, forHTTPHeaderField: "User-Agent")
  if api { r.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept") }
  return r
}

// Downloads one file with progress (callbacks on the main queue).
final class Downloader: NSObject, URLSessionDownloadDelegate {
  private let dest: URL
  private let expected: Int64
  private let progress: (Double) -> Void
  private let done: (Error?) -> Void
  private var failure: Error?
  private var session: URLSession?

  init(url: URL, dest: URL, expected: Int64, progress: @escaping (Double) -> Void, done: @escaping (Error?) -> Void) {
    self.dest = dest
    self.expected = expected
    self.progress = progress
    self.done = done
    super.init()
    let s = URLSession(configuration: .default, delegate: self, delegateQueue: OperationQueue.main)
    session = s
    s.downloadTask(with: request(url, api: false)).resume()
  }

  func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo location: URL) {
    if let http = downloadTask.response as? HTTPURLResponse, http.statusCode != 200 {
      failure = UpdateError("the server answered \(http.statusCode)")
      return
    }
    do {
      try? FileManager.default.removeItem(at: dest)
      try FileManager.default.moveItem(at: location, to: dest)
    } catch {
      failure = error
    }
  }

  func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didWriteData bytesWritten: Int64,
                  totalBytesWritten: Int64, totalBytesExpectedToWrite: Int64) {
    let total = totalBytesExpectedToWrite > 0 ? totalBytesExpectedToWrite : expected
    if total > 0 { progress(min(1, Double(totalBytesWritten) / Double(total))) }
  }

  func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
    session.finishTasksAndInvalidate()
    self.session = nil
    done(error ?? failure)
  }
}

final class Updater {
  static let blank: [String: Any] = ["status": "idle", "version": "", "progress": 0, "notes": "", "error": "", "required": false]

  var state: [String: Any] = Updater.blank
  private var rel: [String: Any]?
  private var asset: [String: Any]?
  private var file: URL?
  private var busy = false
  private var announced = ""
  private var downloader: Downloader?
  private var lastProgress = Date.distantPast
  private let dir = dataDir.appendingPathComponent("updates", isDirectory: true)
  private let current: String

  private let getConfig: () -> Config
  private let saveConfig: () -> Void
  private let onState: ([String: Any]) -> Void
  private let onRequired: () -> Void
  private let notify: (String, String) -> Void
  private let quit: () -> Void

  init(current: String, getConfig: @escaping () -> Config, saveConfig: @escaping () -> Void,
       onState: @escaping ([String: Any]) -> Void, onRequired: @escaping () -> Void,
       notify: @escaping (String, String) -> Void, quit: @escaping () -> Void) {
    self.current = current
    self.getConfig = getConfig
    self.saveConfig = saveConfig
    self.onState = onState
    self.onRequired = onRequired
    self.notify = notify
    self.quit = quit
  }

  func start() {
    DispatchQueue.main.asyncAfter(deadline: .now() + FIRST_CHECK) { [weak self] in self?.check(false) }
    scheduleNext()
  }

  private func scheduleNext() {
    DispatchQueue.main.asyncAfter(deadline: .now() + CHECK_EVERY) { [weak self] in
      self?.check(false)
      self?.scheduleNext()
    }
  }

  private func set(_ patch: [String: Any], replace: Bool = false) {
    var s = replace ? Updater.blank : state
    for (k, v) in patch { s[k] = v }
    state = s
    onState(s)
  }

  private func requiredBy(_ rel: [String: Any]) -> Bool {
    let body = rel["body"] as? String ?? ""
    guard let re = try? NSRegularExpression(pattern: "<!--\\s*twincxt-min:\\s*v?([0-9][0-9.]*)\\s*-->"),
          let m = re.firstMatch(in: body, range: NSRange(location: 0, length: (body as NSString).length)) else { return false }
    return versionNewer((body as NSString).substring(with: m.range(at: 1)), current)
  }

  // The universal .zip: Twincxt-<version>-mac.zip
  private func pickAsset(_ rel: [String: Any]) -> [String: Any]? {
    let assets = rel["assets"] as? [[String: Any]] ?? []
    return assets.first { (($0["name"] as? String) ?? "").lowercased().hasSuffix("-mac.zip") }
  }

  private func tagVersion(_ rel: [String: Any]) -> String {
    var v = rel["tag_name"] as? String ?? ""
    if v.hasPrefix("v") { v.removeFirst() }
    return v
  }

  func check(_ manual: Bool) {
    if busy { return }
    busy = true
    if manual { set(["status": "checking"], replace: true) }
    let url = URL(string: "https://api.github.com/repos/\(REPO)/releases/latest")!
    URLSession.shared.dataTask(with: request(url, api: true)) { data, resp, err in
      DispatchQueue.main.async { self.checked(manual, data, resp, err) }
    }.resume()
  }

  private func checked(_ manual: Bool, _ data: Data?, _ resp: URLResponse?, _ err: Error?) {
    defer { busy = false }
    do {
      if let err = err { throw err }
      guard let http = resp as? HTTPURLResponse else { throw UpdateError("no answer") }
      guard http.statusCode == 200 else { throw UpdateError("GitHub answered \(http.statusCode)") }
      guard let data = data, let rel = (try JSONSerialization.jsonObject(with: data)) as? [String: Any] else {
        throw UpdateError("unreadable answer")
      }
      if (rel["draft"] as? Bool) == true || (rel["prerelease"] as? Bool) == true {
        throw UpdateError("the latest release is not a final release")
      }
      let version = tagVersion(rel)
      let asset = pickAsset(rel)
      if !versionNewer(version, current) || asset == nil {
        dlog("update: \(version) is not newer than \(current) or has no mac zip")
        if manual { set(["status": "latest"], replace: true) }
        return
      }
      let cfg = getConfig()
      let required = requiredBy(rel)
      if !manual && !required && (!cfg.autoUpdate || cfg.skippedVersion == version) { return }
      self.rel = rel
      self.asset = asset
      set(["status": "available", "version": version, "notes": cleanNotes(rel["body"] as? String ?? ""), "required": required], replace: true)
      if required { onRequired() }
      if required || (!manual && cfg.autoUpdate) { download(false) }
    } catch {
      dlog("update: check failed: \(error.localizedDescription)")
      if manual { set(["status": "error", "error": "Can't check for updates. Check your connection and try again."], replace: true) }
    }
  }

  private func expectedSha256(_ rel: [String: Any], _ asset: [String: Any], _ done: @escaping (Result<String, Error>) -> Void) {
    var d = (asset["digest"] as? String ?? "").lowercased()
    if d.hasPrefix("sha256:") { d.removeFirst(7) }
    if d.count == 64, d.allSatisfy({ $0.isHexDigit }) { done(.success(d)); return }
    let name = asset["name"] as? String ?? ""
    let sums = (rel["assets"] as? [[String: Any]] ?? []).filter {
      let n = (($0["name"] as? String) ?? "").lowercased()
      return n.hasPrefix("sha256sums") && n.hasSuffix(".txt")
    }.compactMap { ($0["browser_download_url"] as? String).flatMap(URL.init(string:)) }
    func next(_ i: Int) {
      if i >= sums.count { done(.failure(UpdateError("no checksum published for this file"))); return }
      URLSession.shared.dataTask(with: request(sums[i], api: false)) { data, resp, _ in
        var found: String?
        if let data = data, (resp as? HTTPURLResponse)?.statusCode == 200, let text = String(data: data, encoding: .utf8) {
          for line in text.components(separatedBy: "\n") {
            let f = line.trimmingCharacters(in: .whitespacesAndNewlines).split(whereSeparator: { $0 == " " || $0 == "\t" }).map(String.init)
            if f.count == 2, f[1].replacingOccurrences(of: "*", with: "") == name, f[0].count == 64, f[0].allSatisfy({ $0.isHexDigit }) {
              found = f[0].lowercased()
              break
            }
          }
        }
        DispatchQueue.main.async {
          if let found = found { done(.success(found)) } else { next(i + 1) }
        }
      }.resume()
    }
    next(0)
  }

  private func download(_ thenInstall: Bool) {
    guard let rel = rel, let asset = asset, (state["status"] as? String) != "downloading" else { return }
    let version = tagVersion(rel)
    let base: [String: Any] = ["version": version, "notes": cleanNotes(rel["body"] as? String ?? ""), "required": requiredBy(rel)]
    func with(_ extra: [String: Any]) -> [String: Any] { base.merging(extra) { _, b in b } }
    set(with(["status": "downloading", "progress": 0]), replace: true)
    let fail: (Error) -> Void = { e in
      dlog("update: download failed: \(e.localizedDescription)")
      self.set(with(["status": "error", "error": "Download failed: " + e.localizedDescription]), replace: true)
    }
    let name = (asset["name"] as? String ?? "update.zip").replacingOccurrences(of: "/", with: "_")
    let final = dir.appendingPathComponent(name)
    let finish = {
      self.file = final
      self.set(with(["status": "ready", "progress": 1]), replace: true)
      if thenInstall { self.install(); return }
      if self.announced != version {
        self.announced = version
        self.notify("Twincxt \(version) is ready", "Open Settings to install it.")
      }
    }
    expectedSha256(rel, asset) { result in
      switch result {
      case .failure(let e): fail(e)
      case .success(let want):
        do {
          try FileManager.default.createDirectory(at: self.dir, withIntermediateDirectories: true)
          if let have = try? sha256(of: final), have == want { finish(); return }
          for f in (try? FileManager.default.contentsOfDirectory(at: self.dir, includingPropertiesForKeys: nil)) ?? [] {
            try? FileManager.default.removeItem(at: f)
          }
          guard let src = (asset["browser_download_url"] as? String).flatMap(URL.init(string:)) else {
            throw UpdateError("no download address")
          }
          let partial = final.appendingPathExtension("partial")
          let size = (asset["size"] as? NSNumber)?.int64Value ?? 0
          self.downloader = Downloader(url: src, dest: partial, expected: size, progress: { p in
            if Date().timeIntervalSince(self.lastProgress) > 0.2 {
              self.lastProgress = Date()
              self.set(["progress": p])
            }
          }, done: { err in
            self.downloader = nil
            if let err = err { fail(err); return }
            do {
              guard try sha256(of: partial) == want else {
                try? FileManager.default.removeItem(at: partial)
                throw UpdateError("file doesn't match the release checksum")
              }
              try? FileManager.default.removeItem(at: final)
              try FileManager.default.moveItem(at: partial, to: final)
              finish()
            } catch { fail(error) }
          })
        } catch { fail(error) }
      }
    }
  }

  func install() {
    guard let f = file, FileManager.default.fileExists(atPath: f.path) else {
      file = nil
      download(true)
      return
    }
    do {
      try installMac(f)
    } catch {
      dlog("update: install failed: \(error.localizedDescription)")
      set(["status": "error", "error": error.localizedDescription])
    }
  }

  // Replace the running Twincxt.app in place, after Twincxt has quit.
  private func installMac(_ zip: URL) throws {
    let fm = FileManager.default
    let bundle = Bundle.main.bundleURL.standardizedFileURL
    guard bundle.pathExtension == "app" else { throw UpdateError("Twincxt is not running from an .app bundle.") }
    let parent = bundle.deletingLastPathComponent()
    guard fm.isWritableFile(atPath: parent.path) else {
      throw UpdateError("Can't write to \(parent.path). Install the new version from the .dmg on the releases page.")
    }
    let stage = dir.appendingPathComponent("stage", isDirectory: true)
    try? fm.removeItem(at: stage)
    try fm.createDirectory(at: stage, withIntermediateDirectories: true)
    let unzip = Process()
    unzip.executableURL = URL(fileURLWithPath: "/usr/bin/ditto")
    unzip.arguments = ["-x", "-k", zip.path, stage.path]
    try unzip.run()
    unzip.waitUntilExit()
    guard unzip.terminationStatus == 0 else { throw UpdateError("Couldn't unpack the update.") }
    guard let fresh = try fm.contentsOfDirectory(atPath: stage.path).first(where: { $0.hasSuffix(".app") }) else {
      throw UpdateError("The update contains no app.")
    }
    let pid = ProcessInfo.processInfo.processIdentifier
    let script = dir.appendingPathComponent("install.sh")
    let text = [
      "#!/bin/bash",
      // Wait for this Twincxt to quit, up to 60 s.
      "for i in $(seq 1 120); do kill -0 \(pid) 2>/dev/null || break; sleep 0.5; done",
      "NEW=" + shellQuote(stage.appendingPathComponent(fresh).path),
      "APP=" + shellQuote(bundle.path),
      "OLD=\"$APP.old-$$\"",
      "mv \"$APP\" \"$OLD\" && mv \"$NEW\" \"$APP\" && rm -rf \"$OLD\" || { [ -d \"$OLD\" ] && mv \"$OLD\" \"$APP\"; }",
      "xattr -dr com.apple.quarantine \"$APP\" 2>/dev/null",
      "open \"$APP\"",
      "",
    ].joined(separator: "\n")
    try text.write(to: script, atomically: true, encoding: .utf8)
    try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: script.path)
    // Detached from Twincxt so it keeps running after Twincxt quits.
    let run = Process()
    run.executableURL = URL(fileURLWithPath: "/bin/bash")
    run.arguments = ["-c", "nohup /bin/bash \(shellQuote(script.path)) >/dev/null 2>&1 &"]
    try run.run()
    run.waitUntilExit()
    dlog("update: installer started, quitting")
    set(["status": "installing"])
    DispatchQueue.main.asyncAfter(deadline: .now() + 0.8) { self.quit() }
  }

  func skip() {
    if (state["version"] as? String ?? "").isEmpty || (state["required"] as? Bool) == true { return }
    getConfig().skippedVersion = state["version"] as? String ?? ""
    saveConfig()
    set([:], replace: true)
  }
}
