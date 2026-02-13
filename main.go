package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/playwright-community/playwright-go"
	"github.com/sirupsen/logrus"
	"gopkg.in/telebot.v4"
)

// ============================================================
// 0. ВЕРСИЯ
// ============================================================
const AppVersion = "1.0.0"

// ============================================================
// 1. КОНФИГУРАЦИЯ
// ============================================================
type Config struct {
	GeminiAPIKey    string
	InputFile       string
	AuthStateFile   string
	MaxConcurrency  int
	TargetLangID    string
	Model           string
	Prompt          string
	TgBotToken      string
	ChatId          string
	BaseURL         string
	ScrollDelay     time.Duration
	EditorLoadDelay time.Duration
	FocusDelay      time.Duration
	BeforeSaveDelay time.Duration
	RowNextDelay    time.Duration
}

func getScriptConfig() Config {
	// Загружаем .env файл, если он есть
	if err := godotenv.Load(); err != nil {
		log.Println("Info: .env file not found, using defaults or environment variables")
	}

	data, err := os.ReadFile("prompt.txt")
	if err != nil {
		log.Fatal(err)
	}
	prompt := string(data)
	return Config{
		GeminiAPIKey:    os.Getenv("GEMINI_API_KEY"),
		InputFile:       getEnv("INPUT_FILE", "projects.txt"),
		AuthStateFile:   getEnv("AUTH_STATE_FILE", "auth.json"),
		MaxConcurrency:  getIntEnv("MAX_CONCURRENCY", 1),
		TargetLangID:    getEnv("TARGET_LANG_ID", "748"),
		Model:           getEnv("MODEL", "gemini-2.5-flash"),
		Prompt:          prompt,
		ScrollDelay:     getDurationEnv("SCROLL_DELAY_MS", 2000),
		EditorLoadDelay: getDurationEnv("EDITOR_LOAD_DELAY_MS", 1500),
		FocusDelay:      getDurationEnv("FOCUS_DELAY_MS", 300),
		BeforeSaveDelay: getDurationEnv("BEFORE_SAVE_DELAY_MS", 800),
		RowNextDelay:    getDurationEnv("ROW_NEXT_DELAY_MS", 600),
		TgBotToken:      getEnv("TG_BOT_TOKEN", ""),
		ChatId:          getEnv("CHAT_ID", ""),
		BaseURL:         getEnv("BASE_URL", "https://app.lokalise.com"),
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func getIntEnv(key string, fallback int) int {
	if value, exists := os.LookupEnv(key); exists {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

func getDurationEnv(key string, fallbackMs int) time.Duration {
	if value, exists := os.LookupEnv(key); exists {
		if ms, err := strconv.Atoi(value); err == nil {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return time.Duration(fallbackMs) * time.Millisecond
}

// Структуры для Gemini API
type GeminiPayload struct {
	Contents []struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"contents"`
}

type TranslationItem struct {
	ID          string `json:"id"`
	Original    string `json:"text"`
	Translation string `json:"translation,omitempty"`
}

type GeminiResponse struct {
	Results []TranslationItem `json:"results"`
}

func main() {
	fmt.Printf("🚀 Lokalise Translator Automation v%s\n", AppVersion)
	config := getScriptConfig()

	// Запуск Playwright
	pw, err := playwright.Run()
	if err != nil {
		log.Fatalf("could not start playwright: %v", err)
	}
	defer pw.Stop()

	// Запуск браузера
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(false),
	})
	if err != nil {
		log.Fatalf("could not launch browser: %v", err)
	}
	defer browser.Close()

	// 1. Проверка авторизации
	if err := ensureLogin(browser, config); err != nil {
		log.Fatalf("Login failed: %v", err)
	}

	// 2. Чтение списка проектов
	projects, err := readProjects(config.InputFile)
	if err != nil {
		log.Fatalf("Could not read projects file: %v", err)
	}
	if len(projects) == 0 {
		fmt.Println("⚠️ Файл с проектами пуст.")
		return
	}

	fmt.Printf("📋 Найдено проектов: %d. Запуск в %d потока(ов)...\n", len(projects), config.MaxConcurrency)

	// 3. Запуск воркеров
	var wg sync.WaitGroup
	sem := make(chan struct{}, config.MaxConcurrency)
	tgBot := newTgBot(config.TgBotToken)

	for _, url := range projects {
		wg.Add(1)
		sem <- struct{}{} // Захват слота

		go func(projectURL string) {
			defer wg.Done()
			defer func() { <-sem }()

			fmt.Printf("🚀 Старт обработки: %s\n", projectURL)
			filename, err := processProject(browser, projectURL, config)

			if err != nil {
				fmt.Printf("❌ Ошибка обработки %s %s: %v\n", filename, projectURL, err)
				messageText := fmt.Sprintf("❌ Ошибка обработки:\n<a href=\"%s\">%s</a>", projectURL, filename)
				notifyTelegram(config, tgBot, messageText)
				return
			}

			// --- УДАЛЕНИЕ ИЗ ФАЙЛА ПРИ УСПЕХЕ ---
			if err := removeURLFromFile(config.InputFile, projectURL); err != nil {
				fmt.Printf("⚠️ Ошибка при удалении из файла %s: %v\n", projectURL, err)
			}

			fmt.Printf("✅ Завершено: %s\n", projectURL)
			messageText := fmt.Sprintf("✅ Завершено:\n<a href=\"%s\">%s</a>", projectURL, filename)
			notifyTelegram(config, tgBot, messageText)
		}(url)
	}

	wg.Wait()
	fmt.Println("🏁 Все проекты обработаны!")
}

var fileMutex sync.Mutex // Глобальный мьютекс для защиты файла

func removeURLFromFile(filePath string, urlToRemove string) error {
	fileMutex.Lock()         // Блокируем доступ для других потоков
	defer fileMutex.Unlock() // Разблокируем в конце

	// 1. Читаем все текущие строки
	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	var newLines []string

	// 2. Формируем новый список строк без удаляемой
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != urlToRemove {
			newLines = append(newLines, line)
		}
	}

	// 3. Записываем обратно (с флагом O_TRUNC, чтобы очистить старое содержимое)
	return os.WriteFile(filePath, []byte(strings.Join(newLines, "\n")+"\n"), 0644)
}

func notifyTelegram(config Config, tgBot *telebot.Bot, messageText string) {
	chatIdInt64, err := strconv.ParseInt(config.ChatId, 10, 64)
	if err != nil {
		fmt.Printf("Ошибка конвертации телеграм ChatId: %v", err)
		return
	}

	_, _ = tgBot.Send(
		telebot.ChatID(chatIdInt64),
		messageText,
		&telebot.SendOptions{
			ParseMode:             telebot.ModeHTML,
			DisableWebPagePreview: true, // Убирает большое окно с превью сайта
		},
	)
}

// ensureLogin проверяет наличие файла куки. Если нет - просит залогиниться и сохраняет.
func ensureLogin(browser playwright.Browser, config Config) error {
	if _, err := os.Stat(config.AuthStateFile); err == nil {
		fmt.Println("🔑 Найден файл авторизации, пропускаем вход.")
		return nil
	}

	fmt.Println("⚠️ Файл авторизации не найден. Требуется вход.")
	context, err := browser.NewContext()
	if err != nil {
		return err
	}
	defer context.Close()

	page, err := context.NewPage()
	if err != nil {
		return err
	}

	// Переходим на страницу входа (или любую страницу проекта, редиректнет на логин)
	if _, err = page.Goto(config.BaseURL + "/signin"); err != nil {
		return err
	}

	err = byId(page, "onetrust-accept-btn-handler").Click()
	if err != nil {
		// panic("could not close accwpt cookies: " + err.Error())
		fmt.Println("could not close accwpt cookies: " + err.Error())
	}

	fmt.Println("⌨️  Пожалуйста, залогиньтесь в браузере. После успешного входа нажмите ENTER в этой консоли...")
	fmt.Scanln()

	// Сохраняем состояние (куки, local storage)
	if _, err := context.StorageState(config.AuthStateFile); err != nil {
		return fmt.Errorf("could not save storage state: %v", err)
	}
	fmt.Println("💾 Авторизация сохранена в", config.AuthStateFile)
	return nil
}

func byId(page playwright.Page, id string) playwright.Locator {
	selector := fmt.Sprintf("[id='%s']", id)
	return page.Locator(selector)
}

func readProjects(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

func processProject(browser playwright.Browser, projectURL string, config Config) (string, error) {
	// Создаем контекст с сохраненными куками
	context, err := browser.NewContext(playwright.BrowserNewContextOptions{
		StorageStatePath: playwright.String(config.AuthStateFile),
	})
	if err != nil {
		return "", fmt.Errorf("could not create context: %v", err)
	}
	defer context.Close()

	page, err := context.NewPage()
	if err != nil {
		return "", fmt.Errorf("could not create page: %v", err)
	}

	if _, err = page.Goto(projectURL); err != nil {
		return "", fmt.Errorf("could not goto url: %v", err)
	}

	filename, err := page.Locator("button[id='1'] strong").InnerText()
	if err != nil {
		return "", fmt.Errorf("could not get filename: %v", err)
	}

	// 1. Сбор пустых строк
	translationMap, err := scrollAndCollect(page, config, filename)
	if err != nil {
		return filename, fmt.Errorf("scroll error: %v", err)
	}
	if len(translationMap) == 0 {
		fmt.Printf("ℹ️ [%s] Пустых строк не найдено.\n", projectURL)
		return filename, nil
	}

	// 2. Перевод через Gemini
	translatedItems, err := translateWithGemini(translationMap, config)
	//translatedItems, err := mockTranslateWithGemini(translationMap, config)
	if err != nil {
		return filename, fmt.Errorf("gemini error: %v", err)
	}

	// 3. Вставка переводов
	err = fillTranslations(page, translatedItems, config)

	return filename, err
}

func scrollAndCollect(page playwright.Page, config Config, filename string) ([]TranslationItem, error) {
	var results []TranslationItem
	seen := make(map[string]bool)

	noNewElementsCount := 0
	maxNoNewRetries := 5
	totalScrolled := 0.0

	fmt.Printf("\n🔍 Начинаю поиск пустых строк в файле %s...", filename)

	for noNewElementsCount < maxNoNewRetries {
		newAddedThisStep := 0
		foundEmptyInThisStep := 0

		rows, err := page.Locator(".row-key[data-id]").All()
		if err != nil {
			break
		}

		for _, row := range rows {
			id, _ := row.GetAttribute("data-id")
			if id == "" || seen[id] {
				continue
			}

			// Помечаем как увиденный
			seen[id] = true
			newAddedThisStep++

			// Проверка на пустоту
			targetCell := row.Locator(fmt.Sprintf(".cell-trans[data-lang-id='%s']", config.TargetLangID))
			isEmpty, _ := targetCell.Locator(".empty").Count()
			cellText, _ := targetCell.InnerText()

			if isEmpty > 0 || strings.TrimSpace(cellText) == "" || strings.TrimSpace(cellText) == "Empty" {
				originalText, err := row.Locator(".base-cell-trans .highlight").First().InnerText()
				if err != nil || originalText == "" {
					originalText, _ = row.Locator(".base-cell-trans").InnerText()
				}

				results = append(results, TranslationItem{
					ID:       id,
					Original: strings.TrimSpace(originalText),
				})
				foundEmptyInThisStep++
			}
		}

		if newAddedThisStep > 0 {
			noNewElementsCount = 0
		} else {
			noNewElementsCount++
		}

		scrollStep := 800.0
		page.Mouse().Wheel(0, scrollStep)
		totalScrolled += scrollStep
		time.Sleep(config.ScrollDelay)
	}

	// Возвращаем курсор в начало
	_ = page.Mouse().Wheel(0, -totalScrolled)

	// КРАСИВЫЙ ФИНАЛЬНЫЙ ВЫВОД
	fmt.Println("")
	fmt.Println("--------------------------------------------------")
	fmt.Printf("✅ Сбор данных для файла %s завершен!\n", filename)
	fmt.Printf("📦 Всего проверено строк: %d\n", len(seen))
	fmt.Printf("📥 Собрано для перевода (пустых): %d\n", len(results))
	fmt.Println("--------------------------------------------------\n")

	return results, nil
}

func mockTranslateWithGemini(tmap []TranslationItem, config Config) ([]TranslationItem, error) {
	return []TranslationItem{
		{ID: "798330850", Translation: "mock polish translation"},
	}, nil
}

func translateWithGemini(tmap []TranslationItem, config Config) ([]TranslationItem, error) {
	fmt.Println("⏳ Запрос к Gemini...")

	var payloadItems []TranslationItem
	for _, v := range tmap {
		payloadItems = append(payloadItems, v)
	}

	// ВАШ ОРИГИНАЛЬНЫЙ ПРОМПТ
	prompt := fmt.Sprintf(`%s

IMPORTANT: Respond ONLY with a valid JSON object. 
Do NOT repeat the translation twice in the output string.
Structure: {"results": [{"id": "ID_HERE", "translation": "POLISH_TEXT_HERE"}, ...]}

Data to translate: %s`, config.Prompt, func() string { b, _ := json.Marshal(payloadItems); return string(b) }())

	geminiReq := GeminiPayload{}
	geminiReq.Contents = append(geminiReq.Contents, struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}{})
	geminiReq.Contents[0].Parts = append(geminiReq.Contents[0].Parts, struct {
		Text string `json:"text"`
	}{Text: prompt})

	jsonPayload, _ := json.Marshal(geminiReq)
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1/models/%s:generateContent?key=%s", config.Model, config.GeminiAPIKey)

	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// --- ВЫВОД RAW ОТВЕТА В КОНСОЛЬ ---
	// fmt.Printf("\n[RAW LLM RESPONSE]:\n%s\n\n", string(body))

	// Извлекаем JSON из ответа (убираем возможные Markdown обертки)
	respStr := string(body)
	start := strings.Index(respStr, "{")
	end := strings.LastIndex(respStr, "}")
	if start == -1 || end == -1 {
		return nil, fmt.Errorf("invalid response format")
	}

	// Парсим структуру Gemini Candidate
	var rawMap map[string]interface{}
	json.Unmarshal(body, &rawMap)

	// В Go структура Gemini вложена: candidates[0].content.parts[0].text
	// Для простоты примера вытащим текст через простое сопоставление или доп. структуру
	candidates, ok := rawMap["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response: %s", string(body))
	}
	candidate := candidates[0].(map[string]interface{})
	content := candidate["content"].(map[string]interface{})
	parts := content["parts"].([]interface{})
	actualJSON := parts[0].(map[string]interface{})["text"].(string)

	// Применяем очистку
	cleanJSON := sanitizeJSON(actualJSON)

	var finalResp GeminiResponse
	err = json.Unmarshal([]byte(cleanJSON), &finalResp)
	if err != nil {
		// Выводим текст, который не удалось распарсить, для удобства дебага
		return nil, fmt.Errorf("Не удалось распарсить ответ от gemini: %w \nТекст после очистки: %s", err, cleanJSON)
	}

	return finalResp.Results, nil
}

func sanitizeJSON(input string) string {
	// Убираем пробелы и переносы строк в начале и конце
	input = strings.TrimSpace(input)

	// Если ответ обернут в блоки кода Markdown
	if strings.HasPrefix(input, "```") {
		// Убираем открывающий блок (поддерживаем ```json и просто ```)
		input = strings.TrimPrefix(input, "```json")
		input = strings.TrimPrefix(input, "```")

		// Убираем закрывающий блок
		input = strings.TrimSuffix(input, "```")

		// Повторно чистим пробелы
		input = strings.TrimSpace(input)
	}

	// На всякий случай: если перед JSON есть какой-то текст,
	// находим первое вхождение { и последнее }
	start := strings.Index(input, "{")
	end := strings.LastIndex(input, "}")
	if start != -1 && end != -1 && end > start {
		input = input[start : end+1]
	}

	return input
}

func fillTranslations(page playwright.Page, items []TranslationItem, config Config) error {
	fmt.Println("✍️ Вставка переводов...")
	for _, item := range items {
		// fmt.Printf("[%d/%d] ID: %s | Вставка...\n", i+1, len(items), item.ID)

		selector := fmt.Sprintf(".row-key[data-id='%s']", item.ID)

		// Скроллим к строке
		row := page.Locator(selector)
		err := row.ScrollIntoViewIfNeeded()
		if err != nil {
			return errors.New("could not scroll to row: " + err.Error())
		}
		err = row.Locator("text=Empty").Click()
		if err != nil {
			return errors.New("could not click cell: " + err.Error())
		}

		time.Sleep(config.EditorLoadDelay)

		err = page.Keyboard().Type(item.Translation)
		if err != nil {
			return errors.New("could not type translation: " + err.Error())
		}

		time.Sleep(config.BeforeSaveDelay)

		// Пытаемся нажать кнопку Save
		saveBtn := page.Locator("button.save.btn-primary")
		err = saveBtn.Click()
		if err != nil {
			return errors.New("could not click save btn: " + err.Error())
		}

		editorSelector := ".ace_text-input, textarea:not([style*='display: none']), [contenteditable='true']"
		// Ждем закрытия редактора
		for j := 0; j < 10; j++ {
			if visible, _ := page.IsVisible(editorSelector); !visible {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		time.Sleep(config.RowNextDelay)
	}
	return nil
}

func newTgBot(token string) *telebot.Bot {
	pref := telebot.Settings{
		Token:  token,
		Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
	}
	botSdk, err := telebot.NewBot(pref)
	if err != nil {
		logrus.Errorf("Ошибка создания бота: %s", err.Error())
		panic(err)
	}
	return botSdk
}
