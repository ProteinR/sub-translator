#!/bin/bash

# Цвета для вывода в консоль
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # Без цвета

echo -e "${BLUE}=== Loka Translator Launcher ===${NC}"

# Переходим в корневую директорию проекта (на уровень выше, чем папка src)
cd "$(dirname "$0")/.."

# 1. Попытка обновить проект из Git
echo -e "${BLUE}[1/3] Проверка обновлений (git pull)...${NC}"

# Сохраняем локальные изменения (если есть) перед pull
git stash -q

# Выполняем git pull
GIT_PULL_OUTPUT=$(git pull origin main 2>&1)
PULL_EXIT_CODE=$?

# Если ветки main нет, пробуем master
if [ $PULL_EXIT_CODE -ne 0 ]; then
    GIT_PULL_OUTPUT=$(git pull origin master 2>&1)
    PULL_EXIT_CODE=$?
fi

if [ $PULL_EXIT_CODE -eq 0 ]; then
     echo -e "${GREEN}✅ Обновления проверены!${NC}"
     echo -e "$GIT_PULL_OUTPUT"
else
    echo -e "${YELLOW}⚠️ Не удалось получить обновления из Git (возможно, нет интернета или есть конфликты). Продолжаем запуск локальной версии.${NC}"
    echo -e "Git output: $GIT_PULL_OUTPUT"
fi

# Возвращаем локальные изменения
git stash pop -q 2>/dev/null

# 2. Сборка проекта
echo -e "${BLUE}[2/3] Обновляем зависимости и собираем проект...${NC}"
cd src
go mod tidy > /dev/null 2>&1
go build -o ../translator-web main.go
if [ $? -ne 0 ]; then
    echo -e "${RED}❌ Ошибка компиляции проекта. Запуск невозможен.${NC}"
    exit 1
fi
cd ..
echo -e "${GREEN}✅ Проект успешно скомпилирован!${NC}"

# 3. Запуск приложения
echo -e "${BLUE}[3/3] Запускаем Loka Translator...${NC}"
echo -e "${GREEN}==========================================${NC}"
./translator-web