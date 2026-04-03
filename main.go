package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "embed"

	"github.com/joho/godotenv"
	"github.com/playwright-community/playwright-go"
	"gopkg.in/telebot.v4"
)

//go:embed index.html
var indexHTML []byte

// ============================================================
// 0. ВЕРСИЯ И СОСТОЯНИЕ
// ============================================================
const AppVersion = "1.2.0-WebUI"

var (
	appCancel context.CancelFunc
	isRunning bool
	runMutex  sync.Mutex
	broker    *SSEBroker
)

// ============================================================
// SSE BROKER ДЛЯ ЛОГОВ
// ============================================================
type SSEBroker struct {
	clients map[chan []byte]bool
	mu      sync.Mutex
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{
		clients: make(map[chan []byte]bool),
	}
}

func (b *SSEBroker) AddClient(c chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[c] = true
}

func (b *SSEBroker) RemoveClient(c chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, c)
	close(c)
}

func (b *SSEBroker) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Очищаем от ANSI escape кодов если нужно, но браузер может их не понимать
	// Пока отправляем как есть, но уберем \n в конце для SSE
	msg := bytes.TrimRight(p, "\n")

	for c := range b.clients {
		select {
		case c <- msg:
		default:
			// Клиент не успевает читать, пропускаем
		}
	}
	return len(p), nil
}

func (b *SSEBroker) BroadcastControl(msg string) {
	b.Write([]byte(msg))
}

// ============================================================
// 1. КОНФИГУРАЦИЯ
// ============================================================
type Config struct {
	GeminiAPIKey    string
	GeminiAPIKey2   string
	InputFile       string
	AuthStateFile   string
	MaxConcurrency  int
	TargetLangID    string
	TranslateToLang string
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
	godotenv.Load() // Пытаемся загрузить актуальный .env

	translateToLangText := getEnv("TRANSLATE_TO", "PL")
	data, err := os.ReadFile(fmt.Sprintf("prompt_to_%s.txt", translateToLangText))
	prompt := ""
	if err == nil {
		prompt = string(data)
	}

	return Config{
		GeminiAPIKey:    os.Getenv("GEMINI_API_KEY"),
		GeminiAPIKey2:   os.Getenv("GEMINI_API_KEY_2"),
		InputFile:       getEnv("INPUT_FILE", "projects.txt"),
		AuthStateFile:   getEnv("AUTH_STATE_FILE", "auth.json"),
		MaxConcurrency:  getIntEnv("MAX_CONCURRENCY", 1),
		TargetLangID:    targetLangIdByText(translateToLangText),
		TranslateToLang: translateToLangText,
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

func targetLangIdByText(text string) string {
	if text == "" || text == "PL" {
		return "748"
	}
	return "640"
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

func setupLogger() *os.File {
	now := time.Now()
	dirName := filepath.Join("logs", now.Format("2006-01-02"))
	os.MkdirAll(dirName, 0755)

	fileName := filepath.Join(dirName, fmt.Sprintf("%s.log", now.Format("15-04-05")))
	file, _ := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)

	broker = NewSSEBroker()

	var multiWriter io.Writer
	if file != nil {
		multiWriter = io.MultiWriter(os.Stdout, file, broker)
	} else {
		multiWriter = io.MultiWriter(os.Stdout, broker)
	}

	handler := slog.NewTextHandler(multiWriter, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format("15:04:05"))
			}
			return a
		},
	})

	slog.SetDefault(slog.New(handler))
	return file
}

// ============================================================
// HTTP API STRUCTURES
// ============================================================
type APIData struct {
	EnvVars   map[string]string `json:"envVars"`
	Projects  string            `json:"projects"`
	Prompt    string            `json:"prompt"`
	IsRunning bool              `json:"isRunning"`
	Version   string            `json:"version"`
}

// ============================================================
// MAIN HTTP SERVER START
// ============================================================
func main() {
	exePath, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exePath)
		if !strings.Contains(exeDir, "go-build") && !strings.Contains(exeDir, "Temp") && !strings.Contains(exeDir, "tmp") {
			os.Chdir(exeDir)
		}
	}

	logFile := setupLogger()
	if logFile != nil {
		defer logFile.Close()
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(indexHTML)
	})

	http.HandleFunc("/api/data", handleGetData)
	http.HandleFunc("/api/save", handleSaveData)
	http.HandleFunc("/api/start", handleStart)
	http.HandleFunc("/api/stop", handleStop)
	http.HandleFunc("/api/logs", handleLogs)
	http.HandleFunc("/api/prompt", handleGetPrompt)

	port := ":8080"
	slog.Info("🌐 Starting Web UI on http://localhost" + port)

	go openBrowser("http://localhost" + port)

	if err := http.ListenAndServe(port, nil); err != nil {
		slog.Error("Server failed", "error", err)
	}
}

// ============================================================
// HTTP HANDLERS
// ============================================================
func handleGetPrompt(w http.ResponseWriter, r *http.Request) {
	lang := r.URL.Query().Get("lang")
	if lang == "" {
		lang = "PL"
	}
	prompt, _ := os.ReadFile(fmt.Sprintf("prompt_to_%s.txt", lang))
	w.Header().Set("Content-Type", "text/plain")
	w.Write(prompt)
}

func handleGetData(w http.ResponseWriter, r *http.Request) {
	envMap, _ := godotenv.Read()
	if envMap == nil {
		envMap = make(map[string]string)
	}

	projects, _ := os.ReadFile("projects.txt")
	translateTo := envMap["TRANSLATE_TO"]
	if translateTo == "" {
		translateTo = "PL"
	}
	prompt, _ := os.ReadFile(fmt.Sprintf("prompt_to_%s.txt", translateTo))

	runMutex.Lock()
	rState := isRunning
	runMutex.Unlock()

	json.NewEncoder(w).Encode(APIData{
		EnvVars:   envMap,
		Projects:  string(projects),
		Prompt:    string(prompt),
		IsRunning: rState,
		Version:   AppVersion,
	})
}

func handleSaveData(w http.ResponseWriter, r *http.Request) {
	var req APIData
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 1. Update .env
	envMap, _ := godotenv.Read()
	if envMap == nil {
		envMap = make(map[string]string)
	}
	for k, v := range req.EnvVars {
		envMap[k] = v
	}
	godotenv.Write(envMap, ".env") // Это перезапишет файл (без комментариев), но зато просто

	// 2. Update projects.txt
	os.WriteFile("projects.txt", []byte(req.Projects), 0644)

	// 3. Update prompt file
	translateTo := envMap["TRANSLATE_TO"]
	if translateTo == "" {
		translateTo = "PL"
	}
	os.WriteFile(fmt.Sprintf("prompt_to_%s.txt", translateTo), []byte(req.Prompt), 0644)

	w.WriteHeader(http.StatusOK)
}

func handleStart(w http.ResponseWriter, r *http.Request) {
	runMutex.Lock()
	if isRunning {
		runMutex.Unlock()
		http.Error(w, "Already running", http.StatusConflict)
		return
	}
	isRunning = true
	runMutex.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	appCancel = cancel
	broker.BroadcastControl("___PROCESS_STARTED___")

	go runTranslation(ctx)
	w.WriteHeader(http.StatusOK)
}

func handleStop(w http.ResponseWriter, r *http.Request) {
	runMutex.Lock()
	defer runMutex.Unlock()
	if isRunning && appCancel != nil {
		appCancel()
		w.WriteHeader(http.StatusOK)
	} else {
		http.Error(w, "Not running", http.StatusConflict)
	}
}

func handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	clientChan := make(chan []byte, 100)
	broker.AddClient(clientChan)
	defer broker.RemoveClient(clientChan)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-clientChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

func openBrowser(url string) {
	time.Sleep(1 * time.Second) // Даем серверу подняться
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		slog.Warn("Could not open browser automatically", "error", err)
	}
}

// ============================================================
// MAIN TRANSLATION LOGIC
// ============================================================
func runTranslation(ctx context.Context) {
	defer func() {
		runMutex.Lock()
		isRunning = false
		appCancel = nil
		runMutex.Unlock()
		broker.BroadcastControl("___PROCESS_STOPPED___")
		slog.Info("🏁 Процесс перевода завершен или остановлен.")
	}()

	slog.Info("🚀 Loka Translator Automation started", "version", AppVersion)
	config := getScriptConfig()

	pw, err := playwright.Run()
	if err != nil {
		slog.Error("could not start playwright", "error", err)
		return
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(false),
	})
	if err != nil {
		slog.Error("could not launch browser", "error", err)
		return
	}
	defer browser.Close()

	projects, err := readProjects(config.InputFile)
	if err != nil {
		slog.Error("Could not read projects file", "error", err)
		return
	}
	if len(projects) == 0 {
		slog.Warn("⚠️ Файл с проектами пуст.")
		return
	}

	slog.Info("🌐 Открываем первый проект для проверки авторизации...")
	contextBrowser, firstPage, err := ensureLogin(ctx, browser, config, projects[0])
	if err != nil {
		slog.Error("Login failed", "error", err)
		return
	}
	defer contextBrowser.Close()

	slog.Info("📋 Найдено проектов", "count", len(projects), "threads", config.MaxConcurrency)

	pagePool := make(chan playwright.Page, config.MaxConcurrency)
	pagePool <- firstPage
	for i := 1; i < config.MaxConcurrency; i++ {
		p, err := contextBrowser.NewPage()
		if err == nil {
			pagePool <- p
		}
	}

	var wg sync.WaitGroup
	var tgBot *telebot.Bot
	if config.TgBotToken != "" {
		tgBot = newTgBot(config.TgBotToken)
	}

	for _, url := range projects {
		// Проверяем отмену перед каждым проектом
		select {
		case <-ctx.Done():
			slog.Warn("⚠️ Обработка прервана пользователем.")
			return
		default:
		}

		wg.Add(1)

		// Ограничиваем concurrency: горутина ждет страницу из пула
		page := <-pagePool

		go func(projectURL string, p playwright.Page) {
			defer wg.Done()
			defer func() { pagePool <- p }()

			slog.Info("🚀 Старт обработки", "url", projectURL)

			// Передаем ctx внутрь processProject (упрощенно проверяем отмену внутри долгих функций)
			filename, err := processProject(ctx, p, projectURL, config)

			if err != nil {
				if err == context.Canceled {
					slog.Warn("⚠️ Остановлено", "url", projectURL)
					return
				}
				slog.Error("❌ Ошибка обработки", "file", filename, "url", projectURL, "error", err)
				messageText := fmt.Sprintf("❌ Ошибка обработки:\n<a href=\"%s\">%s</a>\nОшибка: %s", projectURL, filename, err.Error())
				notifyTelegram(config, tgBot, messageText)
				return
			}

			if err := removeURLFromFile(config.InputFile, projectURL); err != nil {
				slog.Warn("⚠️ Ошибка при удалении из файла", "url", projectURL, "error", err)
			}

			slog.Info("✅ Завершено", "url", projectURL)
			messageText := fmt.Sprintf("✅ Завершено:\n<a href=\"%s\">%s</a>", projectURL, filename)
			notifyTelegram(config, tgBot, messageText)

		}(url, page)
	}

	wg.Wait()
}

var fileMutex sync.Mutex

func removeURLFromFile(filePath string, urlToRemove string) error {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	data, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	var newLines []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != urlToRemove {
			newLines = append(newLines, line)
		}
	}

	return os.WriteFile(filePath, []byte(strings.Join(newLines, "\n")+"\n"), 0644)
}

func notifyTelegram(config Config, tgBot *telebot.Bot, messageText string) {
	if tgBot == nil {
		return
	}
	chatIdInt64, err := strconv.ParseInt(config.ChatId, 10, 64)
	if err != nil {
		slog.Error("Ошибка конвертации телеграм ChatId", "error", err)
		return
	}

	_, _ = tgBot.Send(
		telebot.ChatID(chatIdInt64),
		messageText,
		&telebot.SendOptions{
			ParseMode:             telebot.ModeHTML,
			DisableWebPagePreview: true,
		},
	)
}

func ensureLogin(ctx context.Context, browser playwright.Browser, config Config, checkURL string) (playwright.BrowserContext, playwright.Page, error) {
	var ctxOpts playwright.BrowserNewContextOptions

	if _, err := os.Stat(config.AuthStateFile); err == nil {
		slog.Info("🔑 Найден файл авторизации, проверяем...")
		ctxOpts.StorageStatePath = playwright.String(config.AuthStateFile)
	}

	contextBrowser, err := browser.NewContext(ctxOpts)
	if err != nil {
		return nil, nil, err
	}

	page, err := contextBrowser.NewPage()
	if err != nil {
		contextBrowser.Close()
		return nil, nil, err
	}

	if _, err = page.Goto(checkURL); err != nil {
		contextBrowser.Close()
		return nil, nil, err
	}

	// Ждем появления h1 с текстом "Log in" максимум 5 секунд
	err = page.Locator("h1", playwright.PageLocatorOptions{
		HasText: "Log in",
	}).WaitFor(playwright.LocatorWaitForOptions{
		Timeout: playwright.Float(5000),
	})

	if err == nil {
		slog.Warn("⚠️ Требуется вход. Пожалуйста, залогиньтесь в открывшемся окне браузера!")

		err = byId(page, "onetrust-accept-btn-handler").Click()
		if err != nil {
			// игнорируем, если нет куков
		}

		// Асинхронное ожидание логина: ждем, пока страница с логином исчезнет
		loggedIn := false
		for i := 0; i < 180; i++ { // Ожидаем до 3 минут
			select {
			case <-ctx.Done():
				return contextBrowser, page, context.Canceled
			default:
				count, _ := page.Locator("h1", playwright.PageLocatorOptions{HasText: "Log in"}).Count()
				if count == 0 {
					loggedIn = true
					break
				}
				time.Sleep(1 * time.Second)
			}
			if loggedIn {
				break
			}
		}

		if !loggedIn {
			return contextBrowser, page, fmt.Errorf("превышено время ожидания авторизации")
		}

		// Сохраняем состояние
		if _, err := contextBrowser.StorageState(config.AuthStateFile); err != nil {
			return contextBrowser, page, fmt.Errorf("could not save storage state: %v", err)
		}
		slog.Info("💾 Авторизация сохранена")
	} else {
		slog.Info("✅ Куки валидны, вход не требуется.")
	}

	return contextBrowser, page, nil
}

func byId(page playwright.Page, id string) playwright.Locator {
	selector := fmt.Sprintf("[id='%s']", id)
	return page.Locator(selector)
}

func readProjects(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		// Создадим пустой если нет
		if os.IsNotExist(err) {
			os.WriteFile(path, []byte(""), 0644)
			return []string{}, nil
		}
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

func processProject(ctx context.Context, page playwright.Page, projectURL string, config Config) (string, error) {
	if _, err := page.Goto(projectURL); err != nil {
		return "", fmt.Errorf("could not goto url: %v", err)
	}

	bilingualBtn := page.Locator(".single-view-btn")
	if err := bilingualBtn.WaitFor(playwright.LocatorWaitForOptions{
		Timeout: playwright.Float(5000),
	}); err == nil {
		classAttr, err := bilingualBtn.GetAttribute("class")
		if err == nil && !strings.Contains(classAttr, "active") {
			slog.Info("🔄 Переключаем вид на 'Bilingual'...")
			if err := bilingualBtn.Click(); err == nil {
				page.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
					State: playwright.LoadStateNetworkidle,
				})
				time.Sleep(2 * time.Second)
			}
		}
	}

	langSelect := page.Locator("#single-lang")
	if count, _ := langSelect.Count(); count > 0 {
		currentVal, err := langSelect.InputValue()
		if err == nil && currentVal != config.TargetLangID {
			slog.Info("🌍 Переключаем язык перевода...", "id", config.TargetLangID)
			_, err = langSelect.SelectOption(playwright.SelectOptionValues{
				Values: playwright.StringSlice(config.TargetLangID),
			})
			if err == nil {
				page.WaitForLoadState(playwright.PageWaitForLoadStateOptions{
					State: playwright.LoadStateNetworkidle,
				})
				time.Sleep(2 * time.Second)
			}
		}
	}

	collapseBtn := page.Locator("button[aria-label='Collapse panel']")
	if count, _ := collapseBtn.Count(); count > 0 {
		if err := collapseBtn.Click(); err == nil {
			time.Sleep(300 * time.Millisecond)
		}
	}

	filename, err := page.Locator("button[id='1'] strong").InnerText()
	if err != nil {
		return "", fmt.Errorf("could not get filename: %v", err)
	}
	filename = strings.TrimSpace(strings.ReplaceAll(filename, "\u00a0", " "))
	filename = strings.TrimPrefix(filename, "Filename: ")
	filename = strings.TrimSpace(filename)

	translationMap, err := scrollAndCollect(ctx, page, config, filename)
	if err != nil {
		return filename, fmt.Errorf("scroll error: %v", err)
	}
	if len(translationMap) == 0 {
		slog.Info("ℹ️ Пустых строк не найдено", "url", projectURL)
		return filename, nil
	}

	translatedItems, err := translateWithGemini(translationMap, config)
	if err != nil {
		return filename, fmt.Errorf("gemini error: %v", err)
	}

	err = fillTranslations(ctx, page, translatedItems, config)

	return filename, err
}

func scrollAndCollect(ctx context.Context, page playwright.Page, config Config, filename string) ([]TranslationItem, error) {
	var results []TranslationItem
	seen := make(map[string]bool)

	noNewElementsCount := 0
	maxNoNewRetries := 5
	totalScrolled := 0.0

	slog.Info("🔍 Начинаю поиск пустых строк", "file", filename)

	for noNewElementsCount < maxNoNewRetries {
		select {
		case <-ctx.Done():
			return nil, context.Canceled
		default:
		}

		newAddedThisStep := 0
		rows, err := page.Locator(".row-key[data-id]").All()
		if err != nil {
			break
		}

		for _, row := range rows {
			id, _ := row.GetAttribute("data-id")
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			newAddedThisStep++

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

	steps := int(totalScrolled/800.0) + 5
	for i := 0; i < steps; i++ {
		page.Mouse().Wheel(0, -800)
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1 * time.Second)

	slog.Info("✅ Сбор данных завершен", "file", filename, "checked", len(seen), "collected", len(results))
	return results, nil
}

func translateWithGemini(tmap []TranslationItem, config Config) ([]TranslationItem, error) {
	slog.Info("⏳ Запрос к Gemini...")

	prompt := fmt.Sprintf(`%s

IMPORTANT: Respond ONLY with a valid JSON object. 
Do NOT repeat the translation twice in the output string.
Structure: {"results": [{"id": "ID_HERE", "translation": "TRANSLATED_TEXT_HERE"}, ...]}

Data to translate: %s`, config.Prompt, func() string { b, _ := json.Marshal(tmap); return string(b) }())

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

	doCall := func(key string) ([]byte, error) {
		url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1/models/%s:generateContent?key=%s", config.Model, key)
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonPayload))
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
		}

		return body, nil
	}

	body, err := doCall(config.GeminiAPIKey)
	if err != nil {
		if config.GeminiAPIKey2 != "" {
			slog.Warn("⚠️ Ошибка с основным API ключом, пробуем запасной...", "error", err)
			body, err = doCall(config.GeminiAPIKey2)
			if err != nil {
				return nil, fmt.Errorf("оба ключа вернули ошибку: %v", err)
			}
		} else {
			return nil, err
		}
	}

	respStr := string(body)
	start := strings.Index(respStr, "{")
	end := strings.LastIndex(respStr, "}")
	if start == -1 || end == -1 {
		return nil, fmt.Errorf("invalid response format")
	}

	var rawMap map[string]interface{}
	json.Unmarshal(body, &rawMap)

	candidates, ok := rawMap["candidates"].([]interface{})
	if !ok || len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates in response")
	}
	candidate := candidates[0].(map[string]interface{})
	content := candidate["content"].(map[string]interface{})
	parts := content["parts"].([]interface{})
	actualJSON := parts[0].(map[string]interface{})["text"].(string)

	cleanJSON := sanitizeJSON(actualJSON)

	var finalResp GeminiResponse
	err = json.Unmarshal([]byte(cleanJSON), &finalResp)
	if err != nil {
		return nil, fmt.Errorf("parse err: %w \nClean text: %s", err, cleanJSON)
	}

	return finalResp.Results, nil
}

func sanitizeJSON(input string) string {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "```") {
		input = strings.TrimPrefix(input, "```json")
		input = strings.TrimPrefix(input, "```")
		input = strings.TrimSuffix(input, "```")
		input = strings.TrimSpace(input)
	}
	start := strings.Index(input, "{")
	end := strings.LastIndex(input, "}")
	if start != -1 && end != -1 && end > start {
		input = input[start : end+1]
	}
	return input
}

func fillTranslations(ctx context.Context, page playwright.Page, items []TranslationItem, config Config) error {
	slog.Info("✍️ Вставка переводов...")

	for i := 0; i < 5; i++ {
		page.Mouse().Wheel(0, -2000)
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	for _, item := range items {
		select {
		case <-ctx.Done():
			return context.Canceled
		default:
		}

		selector := fmt.Sprintf(".row-key[data-id='%s']", item.ID)
		found := false
		for k := 0; k < 50; k++ {
			count, _ := page.Locator(selector).Count()
			if count > 0 {
				found = true
				break
			}
			page.Mouse().Wheel(0, 800)
			time.Sleep(200 * time.Millisecond)
		}

		if !found {
			return fmt.Errorf("could not find row %s in DOM after scrolling", item.ID)
		}

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

		saveBtn := page.Locator("button.save.btn-primary")
		err = saveBtn.Click()
		if err != nil {
			return errors.New("could not click save btn: " + err.Error())
		}

		editorSelector := ".ace_text-input, textarea:not([style*='display: none']), [contenteditable='true']"
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
		slog.Error("Ошибка создания бота", "error", err)
		return nil
	}
	return botSdk
}
