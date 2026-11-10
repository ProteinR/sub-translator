#!/bin/bash
# Собирает «Loka Translator.app» (Go + встроенный WebKit) в корне проекта.
# Использование: src/build-app.sh [папка-назначения]
# UNIVERSAL=1 — один бинарник для Apple Silicon и Intel (для релизов).
set -euo pipefail
cd "$(dirname "$0")"
DEST="${1:-..}"
APP="$DEST/Loka Translator.app"
# Чтобы приложение запускалось и на старых macOS, а не только на той, где собрано.
export MACOSX_DEPLOYMENT_TARGET=13.0
export CGO_CFLAGS="-O2 -g -mmacosx-version-min=13.0" CGO_LDFLAGS="-mmacosx-version-min=13.0"
VERSION="${APP_VERSION:-$(sed -n 's/^var AppVersion = "\(.*\)"/\1/p' main.go)}"
LDFLAGS="-X main.AppVersion=$VERSION"

if [ "${UNIVERSAL:-0}" = "1" ]; then
    TMP=$(mktemp -d)
    trap 'rm -rf "$TMP"' EXIT
    CGO_ENABLED=1 GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP/arm64" .
    CGO_ENABLED=1 GOARCH=amd64 go build -trimpath -ldflags "$LDFLAGS" -o "$TMP/amd64" .
    lipo -create -output "$DEST/translator-web" "$TMP/arm64" "$TMP/amd64"
else
    go build -ldflags "$LDFLAGS" -o "$DEST/translator-web" .
fi
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
mv "$DEST/translator-web" "$APP/Contents/MacOS/loka-translator"
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>Loka Translator</string>
	<key>CFBundleDisplayName</key><string>Loka Translator</string>
	<key>CFBundleIdentifier</key><string>com.proteinr.loka-translator</string>
	<key>CFBundleExecutable</key><string>loka-translator</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleShortVersionString</key><string>$VERSION</string>
	<key>CFBundleVersion</key><string>$VERSION</string>
	<key>LSMinimumSystemVersion</key><string>13.0</string>
	<key>NSHighResolutionCapable</key><true/>
	<key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
PLIST
codesign --force --sign - "$APP" >/dev/null 2>&1 || true
echo "$APP"
