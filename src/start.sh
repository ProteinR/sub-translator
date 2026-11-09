#!/bin/bash

# Цвета для вывода в консоль
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # Без цвета

echo -e "${BLUE}=== Loka Translator Launcher ===${NC}"

# Переходим в корневую директорию проекта (на уровень выше, чем папка src)
cd "$(dirname "$0")/.." || exit 1

# 1. Обновление из Git — только fast-forward и только при чистом рабочем дереве.
# Раньше тут был stash + pull + stash pop: при любом расхождении это заканчивалось
# конфликтами на машине пользователя.
echo -e "${BLUE}[1/3] Проверка обновлений (git pull)...${NC}"

if [ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]; then
    echo -e "${YELLOW}⚠️ Есть локальные изменения в файлах проекта — обновление пропущено, запускаю локальную версию.${NC}"
    echo -e "${YELLOW}   Чтобы сбросить их: git fetch origin && git reset --hard origin/main${NC}"
    git status --short --untracked-files=no
elif GIT_PULL_OUTPUT=$(git pull --ff-only 2>&1); then
    echo -e "${GREEN}✅ Обновления проверены!${NC}"
    echo "$GIT_PULL_OUTPUT"
else
    echo -e "${YELLOW}⚠️ Не удалось получить обновления (нет интернета или история разошлась). Запускаю локальную версию.${NC}"
    echo "$GIT_PULL_OUTPUT"
fi

# 2. Сборка проекта
echo -e "${BLUE}[2/3] Собираем проект...${NC}"
cd src || exit 1
if ! go mod download; then
    echo -e "${RED}❌ Не удалось скачать зависимости Go (см. ошибку выше).${NC}"
    exit 1
fi
# Собираем пакет целиком: код разнесён по нескольким файлам.
if ! go build -o ../translator-web .; then
    echo -e "${RED}❌ Ошибка компиляции проекта. Запуск невозможен.${NC}"
    exit 1
fi
cd ..
echo -e "${GREEN}✅ Проект успешно скомпилирован!${NC}"

# 3. Запуск приложения
echo -e "${BLUE}[3/3] Запускаем Loka Translator...${NC}"
echo -e "${GREEN}==========================================${NC}"
exec ./translator-web
