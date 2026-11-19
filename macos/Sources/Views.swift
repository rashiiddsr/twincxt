// The window's views: a flipped root view, and the shell web view that lets you
// drag the window by the title strip (WKWebView has no -webkit-app-region).

import AppKit
import WebKit

final class RootView: NSView {
  var onResize: (() -> Void)?
  override var isFlipped: Bool { true }
  override func setFrameSize(_ newSize: NSSize) {
    super.setFrameSize(newSize)
    onResize?()
  }
}

// What a double-click on a title bar does (System Settings → Desktop & Dock).
func titleBarDoubleClick(_ w: NSWindow?) {
  guard let w = w else { return }
  let d = UserDefaults.standard
  let action = d.string(forKey: "AppleActionOnDoubleClick")
    ?? (d.bool(forKey: "AppleMiniaturizeOnDoubleClick") ? "Minimize" : "Maximize")
  switch action {
  case "Minimize": w.performMiniaturize(nil)
  case "None": break
  default: w.performZoom(nil)
  }
}

final class ShellWebView: WKWebView {
  // CSS pixels from the top-left, sent by the shell ("regions" message).
  var dragRects: [CGRect] = []
  var noDragRects: [CGRect] = []

  private func pagePoint(_ windowPoint: NSPoint) -> CGPoint {
    let p = convert(windowPoint, from: nil)
    let y = isFlipped ? p.y : bounds.height - p.y
    let z = pageZoom > 0 ? pageZoom : 1
    return CGPoint(x: p.x / z, y: y / z)
  }

  func inDragRegion(_ windowPoint: NSPoint) -> Bool {
    let p = pagePoint(windowPoint)
    return dragRects.contains { $0.contains(p) } && !noDragRects.contains { $0.contains(p) }
  }

  override func hitTest(_ point: NSPoint) -> NSView? {
    // point is in the superview's coordinates.
    if let sv = superview, frame.contains(point) {
      let wp = sv.convert(point, to: nil)
      if inDragRegion(wp) { return self }
    }
    return super.hitTest(point)
  }

  override func mouseDown(with event: NSEvent) {
    if inDragRegion(event.locationInWindow) {
      if event.clickCount >= 2 {
        titleBarDoubleClick(window)
      } else {
        window?.performDrag(with: event)
      }
      return
    }
    super.mouseDown(with: event)
  }

  override func acceptsFirstMouse(for event: NSEvent?) -> Bool {
    if let e = event, inDragRegion(e.locationInWindow) { return true }
    return super.acceptsFirstMouse(for: event)
  }

  override var mouseDownCanMoveWindow: Bool { false }
}

// WKUserContentController keeps its handlers alive; this breaks the cycle.
final class ScriptProxy: NSObject, WKScriptMessageHandler {
  weak var target: WKScriptMessageHandler?
  init(_ t: WKScriptMessageHandler) { target = t }
  func userContentController(_ u: WKUserContentController, didReceive m: WKScriptMessage) {
    target?.userContentController(u, didReceive: m)
  }
}
