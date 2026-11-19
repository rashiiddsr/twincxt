// Twincxt for macOS: AppKit + WKWebView. The same behaviour as the Electron
// app in desktop/main.js; the interface is the shared ui/shell.html and the
// page script ui/service.js.
//
// Layout: one window. The shell (sidebar, title strip, settings) covers it;
// the WhatsApp and Telegram views sit in the content area on top of it.

import AppKit
import WebKit
import UserNotifications
import AVFoundation
import ServiceManagement

let SIDE_W: CGFloat = 76
let STRIP_H: CGFloat = 38
let MIN_W: CGFloat = 960
let MIN_H: CGFloat = 620
let NAMES = ["wa": "WhatsApp", "tg": "Telegram"]

func trusted(_ url: URL?) -> Bool {
  guard let u = url, u.scheme?.lowercased() == "https", let h = u.host?.lowercased() else { return false }
  return ["whatsapp.com", "whatsapp.net", "telegram.org"].contains { h == $0 || h.hasSuffix("." + $0) }
}

// A Safari user agent matching this system's WebKit: WhatsApp refuses browsers
// it does not recognise.
let USER_AGENT: String = {
  let major = ProcessInfo.processInfo.operatingSystemVersion.majorVersion
  var safari = "\(major).0"
  if major <= 12 { safari = "15.6" } else if major < 26 { safari = "\(major + 3).6" }
  return "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/\(safari) Safari/605.1.15"
}()

// Clicks WhatsApp's own "Move here" so a call (and the camera) returns to the
// main window when you close the call window.
let MOVE_HERE_JS = """
(function(){
  var re=/move here|pindahkan ke sini|pindah ke sini|mover aqu|mover para c|hierher|ici/i;
  var els=document.querySelectorAll('button,[role="button"]');
  for(var i=0;i<els.length;i++){
    var l=(els[i].getAttribute('aria-label')||'')+' '+(els[i].getAttribute('title')||'')+' '+(els[i].textContent||'');
    if(re.test(l)){els[i].click();return true;}
  }
  return false;
})()
"""

let BRIDGE_JS = "window.twincxtBridge={postMessage:function(s){window.webkit.messageHandlers.twincxtService.postMessage(String(s));}};"
let HOST_JS = "window.twincxtHost={postMessage:function(s){window.webkit.messageHandlers.twincxt.postMessage(String(s));}};"

final class Service {
  let id: String
  var view: WKWebView!
  var unread = 0
  var strip: [Int]?
  var ready = false
  var titleObs: NSKeyValueObservation?
  init(_ id: String) { self.id = id }
}

final class Popup {
  let svc: String
  let window: NSWindow
  let view: WKWebView
  var closing = false
  var destroyed = false
  init(svc: String, window: NSWindow, view: WKWebView) {
    self.svc = svc
    self.window = window
    self.view = view
  }
}

struct ViewInfo {
  let svc: String
  let popup: Bool
}

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate, WKNavigationDelegate, WKUIDelegate,
  WKScriptMessageHandler, WKDownloadDelegate, UNUserNotificationCenterDelegate {

  let cfg = Config.load()
  var window: NSWindow!
  var root: RootView!
  var shell: ShellWebView!
  var shellReady = false
  var quitting = false
  var settingsOpen = false
  var locked = false // a required update must be installed first
  var hideAfterFullScreen = false
  var placingLights = false
  var services: [String: Service] = ["wa": Service("wa"), "tg": Service("tg")]
  var popups: [Popup] = []
  var viewInfo: [ObjectIdentifier: ViewInfo] = [:]
  var downloads: [ObjectIdentifier: URL] = [:]
  var updater: Updater!
  var keyMonitor: Any?
  var notificationsOK = false

  let version = (Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String) ?? "0.0.0"
  lazy var uiDir: URL = {
    if let p = env["TWINCXT_UI"], !p.isEmpty { return URL(fileURLWithPath: p, isDirectory: true) }
    return Bundle.main.resourceURL!.appendingPathComponent("ui", isDirectory: true)
  }()
  lazy var serviceSource: String = (try? String(contentsOf: uiDir.appendingPathComponent("service.js"), encoding: .utf8)) ?? ""

  func serviceURL(_ id: String) -> URL {
    URL(string: id == "wa" ? "https://web.whatsapp.com/" : "https://web.telegram.org/\(cfg.telegramVersion)/")!
  }

  func isDark() -> Bool {
    if cfg.theme == "dark" { return true }
    if cfg.theme == "light" { return false }
    return NSApp.effectiveAppearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
  }

  // ---- start ---------------------------------------------------------------------

  func applicationDidFinishLaunching(_ notification: Notification) {
    dlog("start \(version), macOS \(ProcessInfo.processInfo.operatingSystemVersionString), data \(dataDir.path)")
    let ev = NSAppleEventManager.shared().currentAppleEvent
    let atLogin = (ev?.eventID == AEEventID(kAEOpenApplication)
      && ev?.paramDescriptor(forKeyword: AEKeyword(keyAEPropData))?.enumCodeValue == OSType(keyAELaunchedAsLogInItem))
      || CommandLine.arguments.contains("--autostart")
    NSWindow.allowsAutomaticWindowTabbing = false
    applyTheme()
    cfg.autostart = autostartEnabled()
    buildMenu()
    setupNotifications()
    createWindow(show: !(atLogin && cfg.startHidden))
    installKeyMonitor()

    updater = Updater(
      current: version,
      getConfig: { [unowned self] in self.cfg },
      saveConfig: { [unowned self] in self.cfg.save() },
      onState: { [weak self] st in self?.shellEval("twincxt.update(" + json(st) + ")") },
      onRequired: { [weak self] in self?.lockForUpdate() },
      notify: { [weak self] t, b in self?.notifyPlain(t, b) },
      quit: { [weak self] in self?.quit() })
    if env["TWINCXT_SMOKE"] == nil { updater.start() }

    if let s = env["TWINCXT_SMOKE"], let secs = Double(s) {
      DispatchQueue.main.asyncAfter(deadline: .now() + secs) { [weak self] in self?.smokeFinish() }
    }
    if env["TWINCXT_TEST_POPUP"] != nil { testPopup() }
  }

  func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
    showWindow()
    return true
  }

  func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

  func applicationWillTerminate(_ notification: Notification) {
    quitting = true
    saveBounds()
    cfg.save()
  }

  func quit() {
    quitting = true
    NSApp.terminate(nil)
  }

  // ---- window --------------------------------------------------------------------

  func createWindow(show: Bool) {
    var rect = NSRect(x: 0, y: 0, width: max(MIN_W, CGFloat(cfg.winW)), height: max(MIN_H, CGFloat(cfg.winH)))
    var placed = false
    if let primary = NSScreen.screens.first {
      let r = NSRect(x: CGFloat(cfg.winX), y: primary.frame.maxY - CGFloat(cfg.winY) - rect.height, width: rect.width, height: rect.height)
      if cfg.winW >= Double(MIN_W / 2), NSScreen.screens.contains(where: { $0.visibleFrame.intersects(r) }) {
        rect = r
        placed = true
      }
    }
    let w = NSWindow(contentRect: rect, styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
                     backing: .buffered, defer: false)
    window = w
    w.setFrame(rect, display: false)
    if !placed { w.center() }
    w.title = "Twincxt"
    w.titleVisibility = .hidden
    w.titlebarAppearsTransparent = true
    w.minSize = NSSize(width: MIN_W, height: MIN_H)
    w.isReleasedWhenClosed = false
    w.tabbingMode = .disallowed
    w.collectionBehavior.insert(.fullScreenPrimary)
    w.backgroundColor = NSColor(name: nil) { a in
      a.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
        ? NSColor(srgbRed: 0x1A / 255.0, green: 0x1A / 255.0, blue: 0x1A / 255.0, alpha: 1)
        : NSColor(srgbRed: 0xEB / 255.0, green: 0xEB / 255.0, blue: 0xEB / 255.0, alpha: 1)
    }
    w.delegate = self

    let r = RootView(frame: NSRect(origin: .zero, size: rect.size))
    r.autoresizingMask = [.width, .height]
    root = r
    w.contentView = r

    let sc = WKWebViewConfiguration()
    let u = WKUserContentController()
    u.addUserScript(WKUserScript(source: HOST_JS, injectionTime: .atDocumentStart, forMainFrameOnly: true))
    u.add(ScriptProxy(self), name: "twincxt")
    sc.userContentController = u
    let sh = ShellWebView(frame: r.bounds, configuration: sc)
    sh.navigationDelegate = self
    sh.setValue(false, forKey: "drawsBackground")
    sh.allowsBackForwardNavigationGestures = false
    shell = sh
    r.addSubview(sh)
    sh.loadFileURL(uiDir.appendingPathComponent("shell.html"), allowingReadAccessTo: uiDir)

    for id in ["wa", "tg"] { createService(services[id]!) }
    r.onResize = { [weak self] in self?.layout() }
    layout()
    placeTrafficLights()
    if let container = w.standardWindowButton(.closeButton)?.superview?.superview {
      container.postsFrameChangedNotifications = true
      NotificationCenter.default.addObserver(forName: NSView.frameDidChangeNotification, object: container, queue: .main) { [weak self] _ in
        self?.placeTrafficLights()
      }
    }

    if show {
      w.makeKeyAndOrderFront(nil)
      if cfg.winMax && !w.isZoomed { w.zoom(nil) }
      NSApp.activate(ignoringOtherApps: true)
      placeTrafficLights()
    }
  }

  // The traffic lights sit vertically centred in the 38 px title strip, inside
  // the sidebar's top corner (the shell hides its logo there on macOS).
  func placeTrafficLights() {
    guard !placingLights, let w = window, !w.styleMask.contains(.fullScreen),
          let close = w.standardWindowButton(.closeButton),
          let mini = w.standardWindowButton(.miniaturizeButton),
          let zoom = w.standardWindowButton(.zoomButton),
          let titlebar = close.superview, let container = titlebar.superview else { return }
    placingLights = true
    defer { placingLights = false }
    var f = container.frame
    let wantY = w.frame.height - STRIP_H
    if f.height != STRIP_H || f.origin.y != wantY {
      f.size.height = STRIP_H
      f.origin.y = wantY
      container.frame = f
    }
    if titlebar.frame != container.bounds { titlebar.frame = container.bounds }
    let space = mini.frame.minX - close.frame.minX
    let step = space > 0 ? space : 20
    let groupW = 2 * step + zoom.frame.width
    let x0 = ((SIDE_W - groupW) / 2).rounded()
    let y = ((STRIP_H - close.frame.height) / 2).rounded()
    for (i, b) in [close, mini, zoom].enumerated() {
      let o = NSPoint(x: x0 + CGFloat(i) * step, y: y)
      if b.frame.origin != o { b.setFrameOrigin(o) }
    }
  }

  // The visible service is on top; the other stays running underneath the
  // shell so switching is instant and calls keep working.
  func layout() {
    guard let root = root, let shell = shell else { return }
    let b = root.bounds
    shell.frame = b
    let content = NSRect(x: SIDE_W, y: STRIP_H, width: max(0, b.width - SIDE_W), height: max(0, b.height - STRIP_H))
    var under: [NSView] = []
    var over: [NSView] = []
    for id in ["wa", "tg"] {
      guard let v = services[id]?.view else { continue }
      if v.frame != content { v.frame = content }
      if !settingsOpen && id == cfg.active { over.append(v) } else { under.append(v) }
    }
    let order = under + [shell] + over
    if root.subviews != order { root.subviews = order }
  }

  func focusActive() {
    guard let w = window else { return }
    if settingsOpen { w.makeFirstResponder(shell) } else if let v = services[cfg.active]?.view { w.makeFirstResponder(v) }
  }

  func showWindow() {
    guard let w = window else { return }
    if w.isMiniaturized { w.deminiaturize(nil) }
    w.makeKeyAndOrderFront(nil)
    NSApp.activate(ignoringOtherApps: true)
    placeTrafficLights()
    focusActive()
  }

  func saveBounds() {
    guard let w = window, !w.isMiniaturized, !w.styleMask.contains(.fullScreen) else { return }
    if w.isZoomed {
      cfg.winMax = true
    } else {
      let f = w.frame
      let top = NSScreen.screens.first?.frame.maxY ?? f.maxY
      cfg.winX = Double(f.minX)
      cfg.winY = Double(top - f.maxY)
      cfg.winW = Double(f.width)
      cfg.winH = Double(f.height)
      cfg.winMax = false
    }
    cfg.save()
  }

  // ---- window delegate -------------------------------------------------------------

  func windowShouldClose(_ sender: NSWindow) -> Bool {
    if sender === window {
      saveBounds()
      if quitting { return true }
      if cfg.closeAction == "tray" {
        if sender.styleMask.contains(.fullScreen) {
          hideAfterFullScreen = true
          sender.toggleFullScreen(nil)
        } else {
          sender.orderOut(nil)
        }
      } else {
        quit()
      }
      return false
    }
    guard let p = popups.first(where: { $0.window === sender }) else { return true }
    if p.destroyed || p.closing || p.svc != "wa" || quitting { return true }
    // Closing the call window hands the call back to the main window through
    // WhatsApp's own "Move here". Until WhatsApp has done so the window stays
    // open for the page but invisible to you, so the camera keeps running.
    p.closing = true
    sender.alphaValue = 0
    sender.ignoresMouseEvents = true
    sender.isExcludedFromWindowsMenu = true
    dlog("popup: closed by you, asking WhatsApp to move the call back")
    services["wa"]?.view.evaluateJavaScript(MOVE_HERE_JS) { [weak self] result, _ in
      if (result as? Bool) != true { self?.destroyPopup(p) }
    }
    DispatchQueue.main.asyncAfter(deadline: .now() + 3) { [weak self] in self?.destroyPopup(p) }
    return false
  }

  func windowWillClose(_ notification: Notification) {
    guard let w = notification.object as? NSWindow, let i = popups.firstIndex(where: { $0.window === w }) else { return }
    let p = popups.remove(at: i)
    p.destroyed = true
    viewInfo.removeValue(forKey: ObjectIdentifier(p.view))
    p.view.stopLoading()
    p.view.uiDelegate = nil
    p.view.navigationDelegate = nil
    w.contentView = nil
    dlog("popup: closed (\(popups.count) open)")
  }

  func windowDidResize(_ notification: Notification) {
    guard (notification.object as? NSWindow) === window else { return }
    placeTrafficLights()
    pushState()
  }

  func windowDidEndLiveResize(_ notification: Notification) {
    guard (notification.object as? NSWindow) === window else { return }
    placeTrafficLights()
  }

  func windowDidBecomeKey(_ notification: Notification) {
    guard (notification.object as? NSWindow) === window else { return }
    placeTrafficLights()
  }

  func windowDidExitFullScreen(_ notification: Notification) {
    guard (notification.object as? NSWindow) === window else { return }
    placeTrafficLights()
    if hideAfterFullScreen {
      hideAfterFullScreen = false
      window.orderOut(nil)
    }
  }

  func windowDidEnterFullScreen(_ notification: Notification) {
    guard (notification.object as? NSWindow) === window else { return }
    pushState()
  }

  // ---- shell -------------------------------------------------------------------------

  func shellEval(_ js: String) {
    if shellReady { shell?.evaluateJavaScript(js, completionHandler: nil) }
  }

  func pushState() {
    var st: [String: Any] = [
      "platform": "mac",
      "version": version,
      "active": cfg.active,
      "theme": cfg.theme,
      "zoom": ["wa": cfg.zoomWa, "tg": cfg.zoomTg],
      "autostart": cfg.autostart,
      "startHidden": cfg.startHidden,
      "closeAction": cfg.closeAction,
      "notifyWa": cfg.notifyWa,
      "notifyTg": cfg.notifyTg,
      "notifyMode": cfg.notifyMode,
      "telegramVersion": cfg.telegramVersion,
      "autoUpdate": cfg.autoUpdate,
      "maximized": window?.isZoomed ?? false,
      "settingsOpen": settingsOpen,
    ]
    for k in BLUR_KEYS { st[k] = cfg.blur[k] ?? false }
    shellEval("twincxt.state(" + json(st) + ")")
  }

  func pushUnread(_ s: Service) {
    shellEval("twincxt.unread(\(json(s.id)),\(s.unread))")
    let total = services.values.reduce(0) { $0 + $1.unread }
    NSApp.dockTile.badgeLabel = total > 0 ? String(total) : nil
  }

  func applyStrip() {
    guard let s = services[cfg.active] else { return }
    guard !settingsOpen, let c = s.strip, c.count == 3 else { shellEval("twincxt.strip('', '')"); return }
    let dark = (0.299 * Double(c[0]) + 0.587 * Double(c[1]) + 0.114 * Double(c[2])) / 255 < 0.5
    shellEval("twincxt.strip(\(json("rgb(\(c[0]),\(c[1]),\(c[2]))")),\(json(dark ? "#D1D7DB" : "#3B4A54")))")
  }

  func selectService(_ id: String, _ source: String) {
    guard services[id] != nil else { return }
    if locked {
      settingsOpen = true
      layout()
      pushState()
      return
    }
    dlog("select \(id) by \(source)")
    cfg.active = id
    settingsOpen = false
    layout()
    pushState()
    applyStrip()
    focusActive()
    cfg.save()
  }

  func toggleSettings(_ on: Bool?) {
    settingsOpen = locked || (on ?? !settingsOpen)
    layout()
    pushState()
    applyStrip()
    focusActive()
  }

  func lockForUpdate() {
    if locked { return }
    locked = true
    dlog("update: below the minimum version, services locked")
    settingsOpen = true
    layout()
    pushState()
    applyStrip()
    showWindow()
  }

  func handleShell(_ raw: String) {
    guard let m = parseJSON(raw), let type = m["type"] as? String else { return }
    if type != "regions" { dlog("shell: " + raw) }
    let data = m["data"]
    switch type {
    case "ready":
      shellReady = true
      pushState()
      for s in services.values {
        pushUnread(s)
        shellEval("twincxt.loading(\(json(s.id)),\(!s.ready))")
      }
      applyStrip()
      if let u = updater { shellEval("twincxt.update(" + json(u.state) + ")") }
    case "select":
      if let id = data as? String { selectService(id, "sidebar") }
    case "settings":
      toggleSettings((data as? NSNumber)?.boolValue ?? false)
    case "zoom":
      if let d = data as? [String: Any], let id = d["svc"] as? String {
        setZoom(id, (d["value"] as? NSNumber)?.intValue ?? 100)
      }
    case "set":
      if let d = data as? [String: Any], let key = d["key"] as? String { setOption(key, d["value"]) }
    case "action":
      if let a = data as? String { runAction(a) }
    case "update":
      switch data as? String {
      case "check": updater.check(true)
      case "install": updater.install()
      case "skip": updater.skip()
      default: break
      }
    case "window":
      switch data as? String {
      case "min": window.performMiniaturize(nil)
      case "max": window.performZoom(nil)
      case "close": window.performClose(nil)
      default: break // "drag": done natively from the drag regions
      }
    case "regions":
      if let d = data as? [String: Any] {
        func rects(_ a: Any?) -> [CGRect] {
          (a as? [[Any]] ?? []).compactMap { r in
            let n = r.compactMap { ($0 as? NSNumber)?.doubleValue }
            return n.count == 4 ? CGRect(x: n[0], y: n[1], width: n[2], height: n[3]) : nil
          }
        }
        shell.dragRects = rects(d["drag"])
        shell.noDragRects = rects(d["nodrag"])
        dlog("shell: regions drag=\(shell.dragRects.count) nodrag=\(shell.noDragRects.count)")
      }
    default:
      break
    }
  }

  func setZoom(_ id: String, _ value: Int) {
    let z = (50...200).contains(value) ? value : 100
    if id == "wa" { cfg.zoomWa = z } else { cfg.zoomTg = z }
    services[id]?.view.pageZoom = CGFloat(z) / 100
    cfg.save()
  }

  func setOption(_ key: String, _ value: Any?) {
    let b = (value as? NSNumber)?.boolValue ?? false
    let s = value as? String ?? ""
    switch key {
    case "theme":
      if ["light", "dark", "system"].contains(s) { cfg.theme = s; applyTheme() }
    case "autostart":
      setAutostart(b)
    case "startHidden": cfg.startHidden = b
    case "closeAction": cfg.closeAction = s == "quit" ? "quit" : "tray"
    case "notifyWa": cfg.notifyWa = b
    case "notifyTg": cfg.notifyTg = b
    case "notifyMode":
      if ["toast", "attention", "both"].contains(s) { cfg.notifyMode = s }
    case "autoUpdate":
      cfg.autoUpdate = b
      if b { updater.check(false) }
    case "telegramVersion":
      if (s == "a" || s == "k") && s != cfg.telegramVersion {
        cfg.telegramVersion = s
        services["tg"]?.view.load(URLRequest(url: serviceURL("tg")))
      }
    default:
      if BLUR_KEYS.contains(key) {
        cfg.blur[key] = b
        for sv in services.values { applyPrivacy(sv.view) }
      }
    }
    cfg.save()
    pushState()
  }

  func runAction(_ action: String) {
    let parts = action.split(separator: ":").map(String.init)
    guard parts.count == 2, let s = services[parts[1]], let v = s.view else { return }
    let id = s.id
    if parts[0] == "reload" { v.load(URLRequest(url: serviceURL(id))) }
    if parts[0] == "reset" {
      // Sign out: delete everything the site stored, then start again.
      v.load(URLRequest(url: URL(string: "about:blank")!))
      let store = WKWebsiteDataStore.default()
      let types = WKWebsiteDataStore.allWebsiteDataTypes()
      let domains = id == "wa" ? ["whatsapp.com", "whatsapp.net"] : ["telegram.org"]
      store.fetchDataRecords(ofTypes: types) { [weak self] records in
        let mine = records.filter { r in
          let n = r.displayName.lowercased()
          return domains.contains { n == $0 || n.hasSuffix("." + $0) }
        }
        dlog("reset \(id): removing \(mine.count) data records")
        store.removeData(ofTypes: types, for: mine) {
          guard let self = self else { return }
          s.unread = 0
          self.pushUnread(s)
          v.load(URLRequest(url: self.serviceURL(id)))
        }
      }
    }
  }

  func privacyJS() -> String {
    var p: [String: Any] = [:]
    for k in BLUR_KEYS { p[k] = cfg.blur[k] ?? false }
    return "window.twincxtPrivacy && window.twincxtPrivacy(" + json(p) + ")"
  }

  func applyPrivacy(_ v: WKWebView?) {
    v?.evaluateJavaScript(privacyJS(), completionHandler: nil)
  }

  func applyTheme() {
    switch cfg.theme {
    case "light": NSApp.appearance = NSAppearance(named: .aqua)
    case "dark": NSApp.appearance = NSAppearance(named: .darkAqua)
    default: NSApp.appearance = nil
    }
  }

  // ---- services ------------------------------------------------------------------------

  func serviceUCC(_ svc: String, popup: Bool) -> WKUserContentController {
    let u = WKUserContentController()
    let code = serviceSource.replacingOccurrences(of: "__SVC__", with: svc)
      .replacingOccurrences(of: "__POPUP__", with: popup ? "true" : "false")
    u.addUserScript(WKUserScript(source: BRIDGE_JS, injectionTime: .atDocumentStart, forMainFrameOnly: false))
    u.addUserScript(WKUserScript(source: code, injectionTime: .atDocumentStart, forMainFrameOnly: false))
    u.add(ScriptProxy(self), name: "twincxtService")
    return u
  }

  func configure(_ v: WKWebView) {
    v.customUserAgent = USER_AGENT
    v.navigationDelegate = self
    v.uiDelegate = self
    v.allowsBackForwardNavigationGestures = false
    v.allowsMagnification = false
    if #available(macOS 13.3, *) { v.isInspectable = debugOn }
  }

  func createService(_ s: Service) {
    let c = WKWebViewConfiguration()
    c.websiteDataStore = .default() // logins survive restarts
    c.userContentController = serviceUCC(s.id, popup: false)
    c.preferences.javaScriptCanOpenWindowsAutomatically = true
    if #available(macOS 12.3, *) { c.preferences.isElementFullscreenEnabled = true }
    c.mediaTypesRequiringUserActionForPlayback = []
    let v = WKWebView(frame: .zero, configuration: c)
    configure(v)
    v.setValue(false, forKey: "drawsBackground")
    v.pageZoom = CGFloat(s.id == "wa" ? cfg.zoomWa : cfg.zoomTg) / 100
    s.view = v
    viewInfo[ObjectIdentifier(v)] = ViewInfo(svc: s.id, popup: false)
    s.titleObs = v.observe(\.title, options: [.new]) { [weak self] _, _ in
      DispatchQueue.main.async { self?.titleChanged(s) }
    }
    root.addSubview(v)
    v.load(URLRequest(url: serviceURL(s.id)))
  }

  // Unread count from the page title: "(3) WhatsApp".
  func titleChanged(_ s: Service) {
    let title = s.view.title ?? ""
    var n = 0
    if let re = try? NSRegularExpression(pattern: "\\((\\d+)\\)"),
       let m = re.firstMatch(in: title, range: NSRange(location: 0, length: (title as NSString).length)) {
      n = Int((title as NSString).substring(with: m.range(at: 1))) ?? 0
    }
    if n != s.unread {
      s.unread = n
      pushUnread(s)
    }
  }

  func service(of v: WKWebView) -> Service? {
    guard let i = viewInfo[ObjectIdentifier(v)], !i.popup else { return nil }
    return services[i.svc]
  }

  func userContentController(_ u: WKUserContentController, didReceive m: WKScriptMessage) {
    guard let raw = m.body as? String else { return }
    if m.name == "twincxt" { handleShell(raw); return }
    // A call window's page may talk before it is registered: treat an unknown
    // page as a WhatsApp call window.
    let info = m.webView.flatMap { viewInfo[ObjectIdentifier($0)] } ?? ViewInfo(svc: "wa", popup: true)
    guard let msg = parseJSON(raw), let type = msg["type"] as? String, let s = services[info.svc] else { return }
    dlog("\(info.svc)\(info.popup ? " popup" : ""): \(raw.prefix(300))")
    switch type {
    case "frame" where !info.popup:
      let c = ["r", "g", "b"].map { (msg[$0] as? NSNumber)?.intValue ?? 0 }
      if s.strip != c {
        s.strip = c
        if s.id == cfg.active { applyStrip() }
      }
    case "notify":
      notifyMessage(s, msg["title"] as? String ?? "", msg["body"] as? String ?? "")
    default:
      break // popupBlocked: logged above
    }
  }

  // ---- navigation ------------------------------------------------------------------------

  func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
               decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
    let url = navigationAction.request.url
    if webView === shell {
      decisionHandler(url?.isFileURL == true ? .allow : .cancel)
      if let u = url, !u.isFileURL, ["http", "https"].contains(u.scheme ?? "") { NSWorkspace.shared.open(u) }
      return
    }
    if navigationAction.shouldPerformDownload {
      decisionHandler(.download)
      return
    }
    let scheme = url?.scheme?.lowercased() ?? ""
    let mainFrame = navigationAction.targetFrame?.isMainFrame ?? true
    if ["mailto", "tel"].contains(scheme), let u = url {
      NSWorkspace.shared.open(u)
      decisionHandler(.cancel)
      return
    }
    if mainFrame, ["http", "https"].contains(scheme), !trusted(url), let u = url {
      NSWorkspace.shared.open(u)
      decisionHandler(.cancel)
      // A blank window that only led to another site is not needed.
      if let p = popups.first(where: { $0.view === webView }) {
        let cur = webView.url?.absoluteString ?? ""
        if cur.isEmpty || cur.hasPrefix("about:") { destroyPopup(p) }
      }
      return
    }
    decisionHandler(.allow)
  }

  func webView(_ webView: WKWebView, decidePolicyFor navigationResponse: WKNavigationResponse,
               decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void) {
    var attachment = false
    if let http = navigationResponse.response as? HTTPURLResponse,
       let cd = http.value(forHTTPHeaderField: "Content-Disposition") {
      attachment = cd.lowercased().hasPrefix("attachment")
    }
    decisionHandler(attachment || !navigationResponse.canShowMIMEType ? .download : .allow)
  }

  func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
    download.delegate = self
  }

  func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
    download.delegate = self
  }

  func webView(_ webView: WKWebView, didStartProvisionalNavigation navigation: WKNavigation!) {
    guard let s = service(of: webView), trusted(webView.url) else { return }
    s.ready = false
    shellEval("twincxt.loading(\(json(s.id)),true)")
  }

  func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
    guard let s = service(of: webView) else { return }
    dlog("\(s.id): loaded \(webView.url?.absoluteString ?? "")")
    s.ready = true
    shellEval("twincxt.loading(\(json(s.id)),false)")
    webView.pageZoom = CGFloat(s.id == "wa" ? cfg.zoomWa : cfg.zoomTg) / 100
    applyPrivacy(webView)
  }

  func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
    loadFailed(webView, error)
  }

  func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
    loadFailed(webView, error)
  }

  func loadFailed(_ webView: WKWebView, _ error: Error) {
    guard let s = service(of: webView) else { return }
    dlog("\(s.id): load failed: \(error.localizedDescription)")
    s.ready = true
    shellEval("twincxt.loading(\(json(s.id)),false)")
  }

  func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
    dlog("page process ended, reloading")
    if webView === shell {
      shellReady = false
      webView.loadFileURL(uiDir.appendingPathComponent("shell.html"), allowingReadAccessTo: uiDir)
    } else if let s = service(of: webView) {
      webView.load(URLRequest(url: serviceURL(s.id)))
    } else {
      webView.reload()
    }
  }

  // ---- popups (WhatsApp's call window) and new windows --------------------------------------

  func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
               for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
    let url = navigationAction.request.url
    let s = url?.absoluteString ?? ""
    if trusted(url) || s.isEmpty || s.hasPrefix("about:") {
      let svc = viewInfo[ObjectIdentifier(webView)]?.svc ?? "wa"
      return openPopup(configuration, svc: svc, features: windowFeatures)
    }
    if let u = url, ["http", "https", "mailto", "tel"].contains(u.scheme?.lowercased() ?? "") {
      NSWorkspace.shared.open(u)
    }
    return nil
  }

  func openPopup(_ configuration: WKWebViewConfiguration, svc: String, features: WKWindowFeatures) -> WKWebView {
    configuration.userContentController = serviceUCC(svc, popup: true)
    var w = features.width?.doubleValue ?? 0
    var h = features.height?.doubleValue ?? 0
    if w < 200 { w = 960 }
    if h < 200 { h = 640 }
    let win = NSWindow(contentRect: NSRect(x: 0, y: 0, width: w, height: h),
                       styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
    win.isReleasedWhenClosed = false
    win.title = NAMES[svc] ?? "Twincxt"
    win.minSize = NSSize(width: 360, height: 300)
    win.tabbingMode = .disallowed
    win.delegate = self
    let v = WKWebView(frame: NSRect(x: 0, y: 0, width: w, height: h), configuration: configuration)
    configure(v)
    v.autoresizingMask = [.width, .height]
    win.contentView = v
    popups.append(Popup(svc: svc, window: win, view: v))
    viewInfo[ObjectIdentifier(v)] = ViewInfo(svc: svc, popup: true)
    win.center()
    win.makeKeyAndOrderFront(nil)
    dlog("popup: opened for \(svc)")
    return v
  }

  func destroyPopup(_ p: Popup) {
    if p.destroyed { return }
    p.destroyed = true
    p.window.close()
  }

  func webViewDidClose(_ webView: WKWebView) {
    if let p = popups.first(where: { $0.view === webView }) { destroyPopup(p) }
  }

  // ---- page dialogs ------------------------------------------------------------------------

  func alertWindow(_ v: WKWebView) -> NSWindow? { v.window ?? window }

  func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping () -> Void) {
    let a = NSAlert()
    a.messageText = message
    a.addButton(withTitle: "OK")
    if let w = alertWindow(webView) { a.beginSheetModal(for: w) { _ in completionHandler() } } else { a.runModal(); completionHandler() }
  }

  func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (Bool) -> Void) {
    let a = NSAlert()
    a.messageText = message
    a.addButton(withTitle: "OK")
    a.addButton(withTitle: "Cancel")
    if let w = alertWindow(webView) {
      a.beginSheetModal(for: w) { r in completionHandler(r == .alertFirstButtonReturn) }
    } else {
      completionHandler(a.runModal() == .alertFirstButtonReturn)
    }
  }

  func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String, defaultText: String?,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping (String?) -> Void) {
    let a = NSAlert()
    a.messageText = prompt
    a.addButton(withTitle: "OK")
    a.addButton(withTitle: "Cancel")
    let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 260, height: 24))
    field.stringValue = defaultText ?? ""
    a.accessoryView = field
    if let w = alertWindow(webView) {
      a.beginSheetModal(for: w) { r in completionHandler(r == .alertFirstButtonReturn ? field.stringValue : nil) }
    } else {
      completionHandler(a.runModal() == .alertFirstButtonReturn ? field.stringValue : nil)
    }
  }

  func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters,
               initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping ([URL]?) -> Void) {
    let p = NSOpenPanel()
    p.canChooseFiles = true
    p.canChooseDirectories = parameters.allowsDirectories
    p.allowsMultipleSelection = parameters.allowsMultipleSelection
    if let w = alertWindow(webView) {
      p.beginSheetModal(for: w) { r in completionHandler(r == .OK ? p.urls : nil) }
    } else {
      completionHandler(p.runModal() == .OK ? p.urls : nil)
    }
  }

  // ---- camera and microphone ---------------------------------------------------------------

  func webView(_ webView: WKWebView, requestMediaCapturePermissionFor origin: WKSecurityOrigin,
               initiatedByFrame frame: WKFrameInfo, type: WKMediaCaptureType,
               decisionHandler: @escaping (WKPermissionDecision) -> Void) {
    let host = origin.host.lowercased()
    guard origin.protocol == "https", host == "web.whatsapp.com" || host == "web.telegram.org" else {
      dlog("media: denied for \(origin.protocol)://\(host)")
      decisionHandler(.deny)
      return
    }
    var kinds: [AVMediaType] = []
    switch type {
    case .camera: kinds = [.video]
    case .microphone: kinds = [.audio]
    default: kinds = [.video, .audio]
    }
    func ask(_ i: Int) {
      if i >= kinds.count { decisionHandler(.grant); return }
      AVCaptureDevice.requestAccess(for: kinds[i]) { ok in
        DispatchQueue.main.async {
          if ok { ask(i + 1) } else { dlog("media: system access refused"); decisionHandler(.deny) }
        }
      }
    }
    ask(0)
  }

  // ---- downloads ------------------------------------------------------------------------------

  func download(_ download: WKDownload, decideDestinationUsing response: URLResponse, suggestedFilename: String,
                completionHandler: @escaping (URL?) -> Void) {
    let dir = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask)[0]
    var name = suggestedFilename.replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: ":", with: "_")
    if name.isEmpty { name = "download" }
    let ext = (name as NSString).pathExtension
    var base = (name as NSString).deletingPathExtension
    if base.isEmpty { base = "download" }
    func file(_ n: String) -> URL { dir.appendingPathComponent(ext.isEmpty ? n : n + "." + ext) }
    var target = file(base)
    var i = 1
    while FileManager.default.fileExists(atPath: target.path) {
      target = file("\(base) (\(i))")
      i += 1
    }
    downloads[ObjectIdentifier(download)] = target
    dlog("download: \(target.path)")
    completionHandler(target)
  }

  func downloadDidFinish(_ download: WKDownload) {
    guard let target = downloads.removeValue(forKey: ObjectIdentifier(download)) else { return }
    post(title: "Download complete", body: target.lastPathComponent, info: ["reveal": target.path])
  }

  func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
    downloads.removeValue(forKey: ObjectIdentifier(download))
    dlog("download failed: \(error.localizedDescription)")
  }

  // ---- notifications ----------------------------------------------------------------------------

  func setupNotifications() {
    guard Bundle.main.bundleIdentifier != nil else { return }
    let c = UNUserNotificationCenter.current()
    c.delegate = self
    notificationsOK = true
    if env["TWINCXT_SMOKE"] != nil { return }
    c.requestAuthorization(options: [.alert, .sound, .badge]) { ok, err in
      dlog("notifications: allowed=\(ok) \(err?.localizedDescription ?? "")")
    }
  }

  func post(title: String, body: String, info: [String: String]) {
    guard notificationsOK else { return }
    let c = UNMutableNotificationContent()
    c.title = title
    c.body = body
    c.userInfo = info
    let req = UNNotificationRequest(identifier: UUID().uuidString, content: c, trigger: nil)
    UNUserNotificationCenter.current().add(req) { err in
      if let err = err { dlog("notification: \(err.localizedDescription)") }
    }
  }

  func notifyMessage(_ s: Service, _ title: String, _ body: String) {
    if (s.id == "wa" && !cfg.notifyWa) || (s.id == "tg" && !cfg.notifyTg) { return }
    let focused = window.isVisible && window.isKeyWindow && NSApp.isActive && !window.isMiniaturized
    if focused && s.id == cfg.active && !settingsOpen { return } // already looking at it
    if cfg.notifyMode != "attention" {
      post(title: title.isEmpty ? (NAMES[s.id] ?? "Twincxt") : title, body: body, info: ["svc": s.id])
    }
    if cfg.notifyMode != "toast" && !focused {
      NSApp.requestUserAttention(.informationalRequest) // bounces the Dock icon once
    }
  }

  func notifyPlain(_ title: String, _ body: String) {
    post(title: title, body: body, info: ["settings": "1"])
  }

  nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                          withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
    completionHandler([.banner, .list])
  }

  nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                          withCompletionHandler completionHandler: @escaping () -> Void) {
    let info = response.notification.request.content.userInfo
    let svc = info["svc"] as? String
    let reveal = info["reveal"] as? String
    let settings = info["settings"] != nil
    DispatchQueue.main.async {
      guard let me = NSApp.delegate as? AppDelegate else { return }
      if let path = reveal {
        NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
      } else if let svc = svc {
        me.selectService(svc, "notification")
        me.showWindow()
      } else if settings {
        me.toggleSettings(true)
        me.showWindow()
      }
    }
    completionHandler()
  }

  // ---- open at login ---------------------------------------------------------------------------------

  func autostartEnabled() -> Bool {
    if #available(macOS 13.0, *) {
      let st = SMAppService.mainApp.status
      return st == .enabled || st == .requiresApproval
    }
    return false
  }

  func setAutostart(_ on: Bool) {
    if #available(macOS 13.0, *) {
      do {
        if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
      } catch {
        dlog("autostart: \(error.localizedDescription)")
      }
      if on && SMAppService.mainApp.status == .requiresApproval { SMAppService.openSystemSettingsLoginItems() }
    }
    cfg.autostart = autostartEnabled()
  }

  // ---- menu bar and shortcuts --------------------------------------------------------------------------

  func buildMenu() {
    func item(_ title: String, _ action: Selector?, _ key: String = "", _ mods: NSEvent.ModifierFlags = .command,
              _ target: AnyObject? = nil) -> NSMenuItem {
      let i = NSMenuItem(title: title, action: action, keyEquivalent: key)
      i.keyEquivalentModifierMask = mods
      i.target = target
      return i
    }
    func sub(_ title: String, _ items: [NSMenuItem]) -> (NSMenuItem, NSMenu) {
      let m = NSMenu(title: title)
      for i in items { m.addItem(i) }
      let top = NSMenuItem(title: title, action: nil, keyEquivalent: "")
      top.submenu = m
      return (top, m)
    }
    let sep = { NSMenuItem.separator() }
    let main = NSMenu()

    let servicesMenu = NSMenu(title: "Services")
    let servicesItem = NSMenuItem(title: "Services", action: nil, keyEquivalent: "")
    servicesItem.submenu = servicesMenu
    NSApp.servicesMenu = servicesMenu

    main.addItem(sub("Twincxt", [
      item("About Twincxt", #selector(NSApplication.orderFrontStandardAboutPanel(_:))),
      item("Check for Updates…", #selector(menuCheckUpdates(_:)), "", .command, self),
      sep(),
      item("Settings…", #selector(menuSettings(_:)), ",", .command, self),
      sep(),
      servicesItem,
      sep(),
      item("Hide Twincxt", #selector(NSApplication.hide(_:)), "h"),
      item("Hide Others", #selector(NSApplication.hideOtherApplications(_:)), "h", [.command, .option]),
      item("Show All", #selector(NSApplication.unhideAllApplications(_:))),
      sep(),
      item("Quit Twincxt", #selector(menuQuit(_:)), "q", .command, self),
    ]).0)

    main.addItem(sub("Edit", [
      item("Undo", Selector(("undo:")), "z"),
      item("Redo", Selector(("redo:")), "z", [.command, .shift]),
      sep(),
      item("Cut", #selector(NSText.cut(_:)), "x"),
      item("Copy", #selector(NSText.copy(_:)), "c"),
      item("Paste", #selector(NSText.paste(_:)), "v"),
      item("Paste and Match Style", #selector(NSTextView.pasteAsPlainText(_:)), "v", [.command, .option, .shift]),
      item("Select All", #selector(NSText.selectAll(_:)), "a"),
    ]).0)

    main.addItem(sub("View", [
      item("WhatsApp", #selector(menuWhatsApp(_:)), "1", .command, self),
      item("Telegram", #selector(menuTelegram(_:)), "2", .command, self),
      item("Switch Service", #selector(menuSwitch(_:)), "\t", .control, self),
      sep(),
      item("Zoom In", #selector(menuZoomIn(_:)), "=", .command, self),
      item("Zoom Out", #selector(menuZoomOut(_:)), "-", .command, self),
      item("Actual Size", #selector(menuZoomReset(_:)), "0", .command, self),
      sep(),
      item("Reload", #selector(menuReload(_:)), "r", .command, self),
      item("Toggle Full Screen", #selector(NSWindow.toggleFullScreen(_:)), "f", [.command, .control]),
    ]).0)

    let (windowItem, windowMenu) = sub("Window", [
      item("Minimize", #selector(NSWindow.performMiniaturize(_:)), "m"),
      item("Zoom", #selector(NSWindow.performZoom(_:))),
      item("Close Window", #selector(menuClose(_:)), "w", .command, self),
      sep(),
      item("Bring All to Front", #selector(NSApplication.arrangeInFront(_:))),
    ])
    main.addItem(windowItem)
    NSApp.windowsMenu = windowMenu

    let (helpItem, helpMenu) = sub("Help", [
      item("Release Notes", #selector(menuReleaseNotes(_:)), "", .command, self),
    ])
    main.addItem(helpItem)
    NSApp.helpMenu = helpMenu

    NSApp.mainMenu = main
  }

  @objc func menuCheckUpdates(_ sender: Any?) {
    toggleSettings(true)
    showWindow()
    updater.check(true)
  }

  @objc func menuSettings(_ sender: Any?) {
    toggleSettings(true)
    showWindow()
  }

  @objc func menuQuit(_ sender: Any?) { quit() }
  @objc func menuWhatsApp(_ sender: Any?) { selectService("wa", "menu") }
  @objc func menuTelegram(_ sender: Any?) { selectService("tg", "menu") }
  @objc func menuSwitch(_ sender: Any?) { selectService(cfg.active == "wa" ? "tg" : "wa", "menu") }
  @objc func menuZoomIn(_ sender: Any?) { zoomBy(1) }
  @objc func menuZoomOut(_ sender: Any?) { zoomBy(-1) }
  @objc func menuZoomReset(_ sender: Any?) { zoomBy(0) }
  @objc func menuReload(_ sender: Any?) { runAction("reload:" + cfg.active) }

  @objc func menuClose(_ sender: Any?) {
    if let k = NSApp.keyWindow, k !== window { k.performClose(nil) } else { window.performClose(nil) }
  }

  @objc func menuReleaseNotes(_ sender: Any?) {
    NSWorkspace.shared.open(URL(string: "https://github.com/\(REPO)/releases")!)
  }

  func zoomBy(_ dir: Int) {
    let steps = [50, 67, 75, 80, 90, 100, 110, 125, 150, 175, 200]
    let id = cfg.active
    let cur = id == "wa" ? cfg.zoomWa : cfg.zoomTg
    let i = steps.firstIndex(of: cur) ?? 5
    setZoom(id, dir == 0 ? 100 : steps[max(0, min(steps.count - 1, i + dir))])
    pushState()
  }

  // Shortcuts that must work while a page has focus (the page could otherwise
  // take them): ⌃Tab switches, ⌘1 / ⌘2 select, ⌘, toggles settings.
  func installKeyMonitor() {
    keyMonitor = NSEvent.addLocalMonitorForEvents(matching: .keyDown) { [weak self] e in
      guard let self = self, e.window === self.window else { return e }
      let flags = e.modifierFlags.intersection(.deviceIndependentFlagsMask)
      if e.keyCode == 48 && flags.contains(.control) { // Tab
        self.selectService(self.cfg.active == "wa" ? "tg" : "wa", "Ctrl+Tab")
        return nil
      }
      if flags == .command {
        switch e.charactersIgnoringModifiers {
        case "1": self.selectService("wa", "shortcut"); return nil
        case "2": self.selectService("tg", "shortcut"); return nil
        case ",": self.toggleSettings(nil); return nil
        default: break
        }
      }
      return e
    }
  }

  // ---- test hooks -----------------------------------------------------------------------------------------

  func capture(_ path: String) {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
    p.arguments = ["-x", "-o", "-l\(window.windowNumber)", path]
    do {
      try p.run()
      p.waitUntilExit()
      dlog("smoke: screenshot \(path) exit=\(p.terminationStatus)")
    } catch {
      dlog("smoke: screencapture failed: \(error.localizedDescription)")
    }
  }

  func smokeReport() {
    guard let w = window, let frameView = w.contentView?.superview else { return }
    dlog("smoke: window \(w.frame) visible=\(w.isVisible) key=\(w.isKeyWindow) active=\(NSApp.isActive)")
    for b in [NSWindow.ButtonType.closeButton, .miniaturizeButton, .zoomButton] {
      if let btn = w.standardWindowButton(b) {
        let r = btn.convert(btn.bounds, to: nil)
        dlog("smoke: button \(b.rawValue) left=\(r.minX) top=\(w.frame.height - r.maxY) size=\(r.size)")
      }
    }
    let width = frameView.bounds.height > 0 ? frameView.bounds.width : w.frame.width
    let points: [(String, CGFloat, CGFloat)] = [
      ("corner", 60, 30), ("strip", 300, 19), ("strip-zoom", width - 40, 19),
      ("sidebar", 38, 200), ("content", 500, 300),
    ]
    for (name, x, top) in points {
      let p = NSPoint(x: x, y: frameView.isFlipped ? top : frameView.bounds.height - top)
      let hit = frameView.hitTest(p)
      let inShell = hit.map { $0 === shell || $0.isDescendant(of: shell) } ?? false
      let inService = services.values.contains { s in hit.map { $0 === s.view || $0.isDescendant(of: s.view) } ?? false }
      let drag = shell.inDragRegion(NSPoint(x: x, y: w.frame.height - top))
      dlog("smoke: hit \(name) (\(x),\(top)) -> \(hit.map { String(describing: type(of: $0)) } ?? "nil") shell=\(inShell) service=\(inService) drag=\(drag)")
    }
    for s in services.values {
      dlog("smoke: \(s.id) url=\(s.view.url?.absoluteString ?? "") title=\(s.view.title ?? "") ready=\(s.ready) unread=\(s.unread)")
    }
    dlog("smoke: shellReady=\(shellReady) regions=\(shell.dragRects) nodrag=\(shell.noDragRects.count)")
  }

  func smokeFinish() {
    smokeReport()
    guard let path = env["TWINCXT_SMOKE_SHOT"], !path.isEmpty, window.isVisible else { quit(); return }
    capture(path)
    toggleSettings(true)
    DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in
      guard let self = self else { return }
      let p = (path as NSString).deletingPathExtension + "-settings.png"
      self.capture(p)
      self.selectService("tg", "smoke")
      DispatchQueue.main.asyncAfter(deadline: .now() + 2) {
        self.capture((path as NSString).deletingPathExtension + "-telegram.png")
        self.quit()
      }
    }
  }

  // TWINCXT_TEST_POPUP=1: opens a call-like window from a "Move to new window"
  // button, then closes it the way the window's close button would.
  func testPopup() {
    DispatchQueue.main.asyncAfter(deadline: .now() + 12) { [weak self] in
      guard let self = self, let v = self.services["wa"]?.view else { return }
      let js = """
      (function(){
        var b=document.createElement('button');b.setAttribute('aria-label','Move to new window');
        b.onclick=function(){var w=window.open('','twincxt-test','width=520,height=360');
          w.document.title='Popup click';w.document.body.innerHTML='<h1>POPUP OK</h1>';window.__tw=w;};
        var m=document.createElement('button');m.textContent='Move here';
        m.onclick=function(){window.__tw&&window.__tw.close();};
        document.body.append(b,m);b.click();return true;
      })()
      """
      v.evaluateJavaScript(js) { r, e in dlog("test: popup script \(String(describing: r)) \(e?.localizedDescription ?? "")") }
      DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
        dlog("test: popups open = \(self.popups.count)")
        for p in self.popups { p.window.performClose(nil) }
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { dlog("test: popups after close = \(self.popups.count)") }
      }
    }
  }
}
