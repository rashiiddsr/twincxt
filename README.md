# Twincxt

WhatsApp Web and Telegram Web in one window, for Windows, macOS and Linux.

- **Windows**: native Go + Microsoft Edge WebView2 app (about 4 MB installer).
- **macOS**: native Swift + AppKit + WKWebView app (one universal app for Apple
  Silicon and Intel, about 1 MB; macOS 12 or later).
- **Linux**: Electron app (Chromium). The system's WebKitGTK has no WebRTC on
  Ubuntu, so calls need Chromium's.

All three share the same interface (`ui/shell.html`) and page script
(`ui/service.js`); the look follows each system: Windows 11, macOS
(traffic lights, menu bar, Dock badge) and GNOME/libadwaita.

## Features

- One window, sidebar split in two: WhatsApp on top, Telegram below.
- Both stay signed in and running; unread counts on each half, in the tray / Dock.
- Calls stay in the main window; a separate call window opens only from WhatsApp's
  "Move to new window", and closing it hands the call (and camera) back.
- Notifications: system notifications, and an unread badge on the taskbar / Dock
  icon, or both.
- Shortcuts: `Ctrl+Tab` switches; `Alt+1` / `Alt+2` (macOS `⌘1` / `⌘2`); settings
  `Ctrl+,` (macOS `⌘,`).
- Settings: theme, zoom per service, open at sign-in, start hidden, close to tray,
  notifications, Telegram Web A/K, reload or sign out per service, updates.
- Privacy: blur messages, chat previews, names, profile photos and media until
  you point at them.
- App lock: a PIN (stored as a PBKDF2 hash) on start or after 1, 5 or 15
  minutes away; "Forgot PIN?" signs out of both services and removes it.
- Built-in updater (below).

## Install

Download from [Releases](https://github.com/rashiiddsr/twincxt/releases/latest).

| System | File | Notes |
|---|---|---|
| Windows 10/11 | `Twincxt-Setup-<version>.exe` | Installs per user by default (no admin prompt). Running it over an installed Twincxt offers Update / Repair / Uninstall. |
| macOS 12+ (Apple Silicon and Intel) | `Twincxt-<version>-mac.dmg` | Drag Twincxt to Applications. |
| Ubuntu, Debian, Mint | `Twincxt-<version>-linux-amd64.deb` | Recommended on these systems: `sudo apt install ./Twincxt-<version>-linux-amd64.deb` |
| Other Linux | `Twincxt-<version>-linux-x86_64.AppImage` | `chmod +x` it and run. Needs FUSE 2 (`libfuse2`). |

**macOS, first start.** Twincxt is not signed with an Apple Developer ID, so
macOS asks once: open it from Applications, then go to
*System Settings → Privacy & Security* and click **Open Anyway**. Updates
installed by Twincxt itself do not ask again. If macOS says the app "is damaged",
run `xattr -dr com.apple.quarantine /Applications/Twincxt.app` once.

**Linux AppImage on Ubuntu 24.04+.** Ubuntu restricts the browser sandbox for
programs outside the package manager; use the `.deb` there.

Logins and settings are kept outside the program folder (Windows
`%APPDATA%\Twincxt`, macOS `~/Library/Application Support/Twincxt`, Linux
`~/.config/Twincxt`), so updating or reinstalling never signs you out.

## Updates

Twincxt checks the latest GitHub release when it starts and every 6 hours
(Settings → Updates; can be turned off).

1. A newer version is downloaded in the background. Its SHA-256 must match the
   checksum GitHub lists for the file (or `SHA256SUMS-*.txt` in the release);
   otherwise it is deleted and never run.
2. It is installed only when you press **Restart to update**:
   - Windows: the installer is started as its own process (so it is not closed
     together with Twincxt), Twincxt closes cleanly (logins saved), the installer
     updates in place silently and opens Twincxt again.
   - macOS: the new app replaces the current one in place and opens again.
   - Linux AppImage: the file replaces itself and starts again; `.deb`: installed
     through the system password prompt.

**Minimum version.** A release whose notes contain `<!-- twincxt-min: x.y.z -->`
must be installed by every older version: it cannot be skipped, downloads at once,
and WhatsApp and Telegram stay locked until it is installed.

## Releasing

1. Set the version in `VERSION` and `desktop/package.json`, and pass it to `build.ps1`.
2. Windows (on the PC): `.\build.ps1 -Version x.y.z` → `dist\Twincxt-Setup-x.y.z.exe`
   and `dist\SHA256SUMS-windows.txt`.
3. Push the tag `vx.y.z`: GitHub Actions builds macOS (`.github/workflows/mac.yml`)
   and Linux (`linux.yml`) and attaches them to the release.
4. Upload the Windows files to the same release.

## Build locally

- Windows: `winget install GoLang.Go JRSoftware.InnoSetup`, then `.\build.ps1 -Version x.y.z`.
- macOS: `macos/build.sh` (Xcode command line tools only) → `build/mac/Twincxt.app`,
  `.dmg` and `.zip`.
- Linux: `cd desktop && npm ci && npm run dist:linux`.

Debugging: `TWINCXT_DEBUG=1` writes `debug.log` in the data folder.

## Project files

- `ui/shell.html`, `ui/service.js`, `ui/icons/`: shared interface and page script.
- Windows: `main.go`, `app.go`, `webview2.go`, `winapi.go`, `updater.go`,
  `notify.go`, `config.go`, `scripts.go`, `installer.iss`, `build.ps1`,
  `gen_icon.py`, `gen_resources.py`.
- macOS: `macos/` (`Sources/*.swift`, `Info.plist`, `build.sh`).
- Linux: `desktop/` (`main.js`, `updater.js`, preloads, electron-builder
  config in `package.json`).
- Cloud builds: `.github/workflows/mac.yml`, `.github/workflows/linux.yml`.

## Credits

Twincxt grew out of [whatsapp-web.view](https://github.com/Adytm404/whatsapp-web.view)
by Adytm404, a lightweight Go + WebView2 wrapper for WhatsApp Web on Windows.

Not affiliated with WhatsApp, Meta or Telegram.
