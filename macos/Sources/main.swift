import AppKit

// CI check of the app lock's PIN hashing: TWINCXT_SELFTEST=1 prints and exits.
if env["TWINCXT_SELFTEST"] != nil {
  exit(lockSelfTest() ? 0 : 1)
}

let application = NSApplication.shared
let appDelegate = AppDelegate()
application.delegate = appDelegate
application.setActivationPolicy(.regular)
application.run()
