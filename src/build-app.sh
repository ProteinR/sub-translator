#!/bin/bash
# Собирает «Loka Translator.app» (Go + встроенный WebKit) в корне проекта.
# Использование: src/build-app.sh [папка-назначения]
set -euo pipefail
cd "$(dirname "$0")"
DEST="${1:-..}"
APP="$DEST/Loka Translator.app"
VERSION=$(sed -n 's/^const AppVersion = "\(.*\)"/\1/p' main.go)

go build -o "$DEST/translator-web" .
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
	<key>LSMinimumSystemVersion</key><string>12.0</string>
	<key>NSHighResolutionCapable</key><true/>
	<key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
PLIST
codesign --force --sign - "$APP" >/dev/null 2>&1 || true
echo "$APP"
