#!/bin/bash
# Выпуск новой версии: тесты → версия → universal-сборка → коммит и тег → GitHub Release.
# Установленные приложения увидят релиз и предложат обновиться одной кнопкой.
#
#   scripts/release.sh 3.2.0 "Что нового (видно в баннере обновления)"
set -euo pipefail
VERSION="${1:?Укажите версию, например: scripts/release.sh 3.2.0 \"Что нового\"}"
NOTES="${2:-}"
cd "$(dirname "$0")/.."

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "Версия должна быть вида 3.2.0"; exit 1; }
[ "$(git branch --show-current)" = "main" ] || { echo "Релиз выпускается из main"; exit 1; }
[ -z "$(git status --porcelain --untracked-files=no)" ] || { echo "Есть незакоммиченные изменения"; exit 1; }
git fetch -q origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "main не совпадает с origin/main — сначала pull/push"; exit 1; }
git rev-parse -q --verify "refs/tags/v$VERSION" >/dev/null && { echo "Тег v$VERSION уже есть"; exit 1; }

(cd src && go vet ./... && go test ./...)

sed -i '' "s/^var AppVersion = \".*\"/var AppVersion = \"$VERSION\"/" src/main.go
sed -i '' "s/Текущая версия: \*\*[0-9.]*\*\*/Текущая версия: **$VERSION**/" README.md

DIST=dist
rm -rf "$DIST" && mkdir -p "$DIST"
UNIVERSAL=1 src/build-app.sh "$DIST" >/dev/null
ZIP="$DIST/Loka-Translator-$VERSION.zip"
ditto -c -k --keepParent "$DIST/Loka Translator.app" "$ZIP"

git add src/main.go README.md
git commit -q -m "Release $VERSION"
git tag -a "v$VERSION" -m "Loka Translator $VERSION"
git push -q origin main "v$VERSION"

if [ -n "$NOTES" ]; then
    gh release create "v$VERSION" "$ZIP" --title "Loka Translator $VERSION" --notes "$NOTES"
else
    gh release create "v$VERSION" "$ZIP" --title "Loka Translator $VERSION" --generate-notes
fi
rm -rf "$DIST"
echo "✅ Выпущена версия $VERSION"
