#!/bin/bash

# Цвета для вывода в консоль
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # Без цвета

echo -e "${BLUE}=== Loka Translator Launcher ===${NC}"

# Переходим в директорию скрипта (чтобы можно было запускать откуда угодно)
cd "$(dirname "$0")"

# 1. Попытка обновить проект из Git
echo -e "${BLUE}[1/3] Проверка обновлений (git pull)...${NC}"
# Сохраняем локальные изменения (если есть) перед pull
git stash -q
GIT_PULL_OUTPUT=$(git pull origin main 2>&1)
PULL_EXIT_CODE=$?

# Если ветки main нет, пробуем master
if [ $PULL_EXIT_CODE -ne 0 ]; then
    GIT_PULL_OUTPUT=$(git pull origin master 2>&1)
    PULL_EXIT_CODE=$?
fi

# Возвращаем локальные изменения (игнорируя ошибки, если stash был пуст)
git stash pop -q 2>/dev/null

if [ $PULL_EXIT_CODE -eq 0 ]; then
    # Проверяем, были ли реальные изменения в коде
    if echo "$GIT_PULL_OUTPUT" | grep -q "Already up to date."; then
         echo -e "${GREEN}Обновлений нет. Проект актуален.${NC}"
         NEEDS_BUILD=false
         # Проверяем наличие бинарника, если его нет - все равно надо билдить
         if [ ! -f "translator-web" ]; then
             NEEDS_BUILD=true
         fi
    else
         echo -e "${GREEN}✅ Загружены новые обновления!${NC}"
         NEEDS_BUILD=true
    fi
else
    echo -e "${YELLOW}⚠️ Не удалось получить обновления из Git (возможно, нет интернета). Запускаем локальную версию.${NC}"
    NEEDS_BUILD=false
    if [ ! -f "translator-web" ]; then
         echo -e "${RED}Бинарный файл не найден, попробуем собрать...${NC}"
         NEEDS_BUILD=true
    fi
fi

# 2. Сборка проекта (если нужно)
if [ "$NEEDS_BUILD" = true ]; then
    echo -e "${BLUE}[2/3] Обновляем зависимости и собираем проект...${NC}"
    go mod tidy > /dev/null 2>&1
    go build -o translator-web main.go
    if [ $? -ne 0 ]; then
        echo -e "${RED}❌ Ошибка компиляции проекта. Запуск невозможен.${NC}"
        exit 1
    fi
    echo -e "${GREEN}✅ Проект успешно скомпилирован!${NC}"
else
    echo -e "${BLUE}[2/3] Сборка не требуется.${NC}"
fi

# 3. Подготовка конфигурации
echo -e "${BLUE}[3/4] Проверка файлов конфигурации...${NC}"
# Копируем .env если его нет
if [ ! -f ".env" ]; then
    echo -e "${YELLOW}Файл .env не найден. Копирую из .env-example...${NC}"
    cp .env-example .env
fi

# Копируем промпты если их нет
if [ ! -f "prompt_to_PL.txt" ] && [ -f "prompt_to_pl_example.txt" ]; then
    echo -e "${YELLOW}Создаю prompt_to_PL.txt из примера...${NC}"
    cp prompt_to_pl_example.txt prompt_to_PL.txt
fi

if [ ! -f "prompt_to_EN.txt" ] && [ -f "prompt_to_en_example.txt" ]; then
    echo -e "${YELLOW}Создаю prompt_to_EN.txt из примера...${NC}"
    cp prompt_to_en_example.txt prompt_to_EN.txt
fi

# 4. Запуск приложения
echo -e "${BLUE}[4/4] Запускаем Loka Translator...${NC}"
echo -e "${GREEN}==========================================${NC}"
./translator-web
