#!/bin/bash
# Builds Twincxt.app for macOS (Apple Silicon and Intel in one binary), then the
# .dmg and the .zip the updater uses. Needs only the Xcode command line tools.
#
#   macos/build.sh            -> build/mac/Twincxt.app, Twincxt-<ver>-mac.dmg/.zip
set -euo pipefail

cd "$(dirname "$0")/.."
ver="$(tr -d '[:space:]' < VERSION)"
out="build/mac"
app="$out/Twincxt.app"

rm -rf "$out"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"

sdk="$(xcrun --sdk macosx --show-sdk-path)"
for arch in arm64 x86_64; do
  echo "== compiling $arch"
  xcrun --sdk macosx swiftc -O -swift-version 5 -sdk "$sdk" -target "$arch-apple-macos12.0" \
    macos/Sources/*.swift -o "$out/Twincxt-$arch"
done
lipo -create -output "$app/Contents/MacOS/Twincxt" "$out/Twincxt-arm64" "$out/Twincxt-x86_64"
rm -f "$out/Twincxt-arm64" "$out/Twincxt-x86_64"

sed "s/@VERSION@/$ver/g" macos/Info.plist > "$app/Contents/Info.plist"
printf 'APPL????' > "$app/Contents/PkgInfo"
cp -R ui "$app/Contents/Resources/ui"

# Icon: macOS .icns from the 1024 px artwork.
src="desktop/build/icon.png"
[ -f "$src" ] || src="ui/icons/app.png"
iconset="$out/Twincxt.iconset"
mkdir -p "$iconset"
for s in 16 32 128 256 512; do
  sips -z $s $s "$src" --out "$iconset/icon_${s}x${s}.png" > /dev/null
  sips -z $((s * 2)) $((s * 2)) "$src" --out "$iconset/icon_${s}x${s}@2x.png" > /dev/null
done
iconutil -c icns "$iconset" -o "$app/Contents/Resources/Twincxt.icns"
rm -rf "$iconset"

# No Apple Developer ID: an ad-hoc signature, so Apple Silicon runs it.
codesign --force --deep --sign - "$app"
codesign --verify --deep --strict "$app"

echo "== packaging $ver"
stage="$out/dmg"
mkdir -p "$stage"
cp -R "$app" "$stage/"
ln -s /Applications "$stage/Applications"
dmg="$out/Twincxt-$ver-mac.dmg"
for i in 1 2 3 4 5; do
  hdiutil create -volname "Twincxt $ver" -srcfolder "$stage" -ov -format UDZO "$dmg" && break
  echo "hdiutil failed, trying again ($i)"; sleep 5
done
[ -f "$dmg" ]
rm -rf "$stage"

ditto -c -k --keepParent "$app" "$out/Twincxt-$ver-mac.zip"
du -sh "$app" "$out"/Twincxt-*
