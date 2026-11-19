#!/bin/bash
# Makes dist/Twincxt-<version>-linux-amd64.deb and
# dist/Twincxt-<version>-linux-x86_64.AppImage from build/twincxt.
# Needs ImageMagick (icon sizes), dpkg-deb, and appimagetool on PATH or in
# $APPIMAGETOOL.
set -euo pipefail
cd "$(dirname "$0")"
VERSION="$(cat ../VERSION)"
APPID=io.github.rashiiddsr.Twincxt
BIN=build/twincxt
ICON=../desktop/build/icon.png
[ -x "$BIN" ] || { echo "build $BIN first (make)"; exit 1; }
rm -rf dist pkg
mkdir -p dist pkg

ui() {  # copy the shared interface to $1
  mkdir -p "$1/icons"
  cp ../ui/shell.html ../ui/service.js "$1/"
  cp ../ui/icons/*.png "$1/icons/"
}

icons() {  # hicolor sizes under $1/share/icons
  for s in 16 24 32 48 64 128 256 512; do
    d="$1/share/icons/hicolor/${s}x${s}/apps"
    mkdir -p "$d"
    convert "$ICON" -resize "${s}x${s}" -strip "$d/twincxt.png"
  done
}

# ---- .deb ----
D=pkg/deb
install -Dm755 "$BIN" "$D/usr/bin/twincxt"
strip --strip-unneeded "$D/usr/bin/twincxt"
ui "$D/usr/share/twincxt/ui"
install -Dm644 "$APPID.desktop" "$D/usr/share/applications/$APPID.desktop"
icons "$D/usr"
mkdir -p "$D/usr/share/doc/twincxt"
printf 'Twincxt %s\nhttps://github.com/rashiiddsr/twincxt\n' "$VERSION" > "$D/usr/share/doc/twincxt/README"
mkdir -p "$D/DEBIAN"
SIZE=$(du -sk --exclude=DEBIAN "$D" | cut -f1)
cat > "$D/DEBIAN/control" <<CONTROL
Package: twincxt
Version: $VERSION
Architecture: amd64
Maintainer: Twincxt <rashiiddsr@users.noreply.github.com>
Installed-Size: $SIZE
Depends: libwebkit2gtk-4.1-0 (>= 2.40), libgtk-3-0, libsoup-3.0-0
Recommends: gstreamer1.0-plugins-good, gstreamer1.0-plugins-bad, gstreamer1.0-libav, gstreamer1.0-nice, gstreamer1.0-pipewire, libayatana-appindicator3-1
Section: net
Priority: optional
Homepage: https://github.com/rashiiddsr/twincxt
Description: WhatsApp and Telegram in one window
 Twincxt keeps WhatsApp Web and Telegram Web side by side in one window,
 with notifications, unread counts and a tray icon. It uses the system's
 WebKitGTK, so the package is small.
CONTROL
cat > "$D/DEBIAN/postinst" <<'POSTINST'
#!/bin/sh
set -e
command -v update-desktop-database >/dev/null 2>&1 && update-desktop-database -q /usr/share/applications || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor || true
exit 0
POSTINST
cp "$D/DEBIAN/postinst" "$D/DEBIAN/postrm"
chmod 755 "$D/DEBIAN/postinst" "$D/DEBIAN/postrm"
find "$D" -type d -exec chmod 755 {} +
DEB="dist/Twincxt-$VERSION-linux-amd64.deb"
dpkg-deb --root-owner-group -Zxz --build "$D" "$DEB"

# ---- AppImage ----
A=pkg/Twincxt.AppDir
install -Dm755 AppRun "$A/AppRun"
install -Dm755 "$D/usr/bin/twincxt" "$A/usr/bin/twincxt"
ui "$A/usr/bin/ui"
install -Dm644 "$APPID.desktop" "$A/$APPID.desktop"
install -Dm644 "$D/usr/share/icons/hicolor/256x256/apps/twincxt.png" "$A/twincxt.png"
install -Dm644 "$D/usr/share/icons/hicolor/256x256/apps/twincxt.png" "$A/usr/share/icons/hicolor/256x256/apps/twincxt.png"
ln -sf twincxt.png "$A/.DirIcon"
APP="dist/Twincxt-$VERSION-linux-x86_64.AppImage"
ARCH=x86_64 "${APPIMAGETOOL:-appimagetool}" --no-appstream "$A" "$APP"
chmod +x "$APP"
ls -la dist
