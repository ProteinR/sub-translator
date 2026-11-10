#!/bin/bash
# Пересобирает src/icon/AppIcon.icns из src/icon/AppIcon.svg (нужен rsvg-convert: brew install librsvg).
set -euo pipefail
cd "$(dirname "$0")/../src/icon"
SET=$(mktemp -d)/AppIcon.iconset
mkdir -p "$SET"
for s in 16 32 128 256 512; do
    rsvg-convert -w $s -h $s AppIcon.svg -o "$SET/icon_${s}x${s}.png"
    rsvg-convert -w $((s * 2)) -h $((s * 2)) AppIcon.svg -o "$SET/icon_${s}x${s}@2x.png"
done
iconutil -c icns "$SET" -o AppIcon.icns
rm -rf "$(dirname "$SET")"
echo "src/icon/AppIcon.icns"
