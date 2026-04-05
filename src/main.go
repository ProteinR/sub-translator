package main

import (
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

	currentKeyIndex int
	keyMutex        sync.Mutex
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
	GeminiAPIKey3   string
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
	Projects        []string
}

func loadConfig() FileConfig {
	path := filepath.Join("data", "config.json")
	data, err := os.ReadFile(path)
	cfg := FileConfig{
		TranslateTo:     "PL",
		Model:           "gemini-2.5-flash",
		MaxConcurrency:  1,
		ScrollDelay:     2000,
		EditorLoadDelay: 1500,
		FocusDelay:      300,
		BeforeSaveDelay: 800,
		RowNextDelay:    600,
		Projects:        []string{},
		Prompts:         map[string]string{},
	}
	if err == nil {
		json.Unmarshal(data, &cfg)
	}
	if cfg.Prompts == nil {
		cfg.Prompts = map[string]string{}
	}
	if cfg.Prompts["PL"] == "" {
		cfg.Prompts["PL"] = getDefaultPrompt("PL")
	}
	if cfg.Prompts["EN"] == "" {
		cfg.Prompts["EN"] = getDefaultPrompt("EN")
	}
	return cfg
}

func saveConfig(cfg FileConfig) {
	path := filepath.Join("data", "config.json")
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(path, data, 0644)
}

func toInternalConfig(fc FileConfig) Config {
	return Config{
		GeminiAPIKey:    fc.GeminiAPIKey,
		GeminiAPIKey2:   fc.GeminiAPIKey2,
		GeminiAPIKey3:   fc.GeminiAPIKey3,
		MaxConcurrency:  fc.MaxConcurrency,
		TargetLangID:    targetLangIdByText(fc.TranslateTo),
		TranslateToLang: fc.TranslateTo,
		Model:           fc.Model,
		Prompt:          fc.Prompts[fc.TranslateTo],
		TgBotToken:      fc.TgBotToken,
		ChatId:          fc.ChatId,
		BaseURL:         fc.BaseURL,
		ScrollDelay:     time.Duration(fc.ScrollDelay) * time.Millisecond,
		EditorLoadDelay: time.Duration(fc.EditorLoadDelay) * time.Millisecond,
		FocusDelay:      time.Duration(fc.FocusDelay) * time.Millisecond,
		BeforeSaveDelay: time.Duration(fc.BeforeSaveDelay) * time.Millisecond,
		RowNextDelay:    time.Duration(fc.RowNextDelay) * time.Millisecond,
		Projects:        fc.Projects,
	}
}

func targetLangIdByText(text string) string {
	if text == "" || text == "PL" {
		return "748"
	}
	return "640"
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
	dirName := filepath.Join("data", "logs", now.Format("2006-01-02"))
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
func getDefaultPrompt(lang string) string {
	if lang == "EN" {
		return `Role: Act as a professional translator and English localization expert. Your task is to translate video scripts from Polish to English.

Context: This content covers business/personal development coaching, meditations, sports lessons, and psychology podcasts.

Style & Tone: * Use natural, "living" English.
Prioritize flow and conversational rhythm.
Avoid "Slavicisms" (e.g., wordy constructions like "the fact that", "which is", or excessive passive voice).

Grammar & Localization Rules:
Strict Gender Agreement: Research the speaker's name or context to determine if they are male or female.
Binary Pronouns: Use "he/him" for men and "she/her" for women. Do not use gender-neutral "they" unless the source text specifically implies a group or an unspecified person.
Participles & Sentence Structure: Be careful with Polish adverbial participles (imiesłów) like "robiąc" or "zostawiając". Translate them into clear English structures (e.g., "While doing..." or by using a new clause) to avoid "dangling modifiers." Every sentence must have a clear subject and a finite verb.
Punctuation: Remove periods from titles and headings, following standard English formatting rules.
Direct Address: In coaching and sports lessons, ensure the "You" (Ty/Pan/Pani) sounds motivating and direct, matching the energy of an English-speaking coach.`
	}

	return `Role: Act as a professional translator and Polish localization expert. Your task is to translate video scripts from English to Polish.

Context: This is business/personal development coaching, meditations, sports lessons, psychology podcasts.
Style: Natural "living" language. Focus on flow.
Be aware of this rule in polish grammar: W tym zdaniu jest imiesłów przysłówkowy pozostawiając, ale nie ma czasownika w funkcji orzeczenia. Możliwe, że to tytuł, ale w takim razie zbędna jest kropka.
Check the name of the speaker and make a research: if it's a man or a woman.
Grammar: If the speaker is a woman:
  1. Use feminine verb forms (e.g., "zrobiłam", "powiedziałam").
  2. Use feminatives (e.g., "trenerka", "ekspertka"), where needed.`
}

type FileConfig struct {
	GeminiAPIKey    string            `json:"geminiApiKey"`
	GeminiAPIKey2   string            `json:"geminiApiKey2"`
	GeminiAPIKey3   string            `json:"geminiApiKey3"`
	MaxConcurrency  int               `json:"maxConcurrency"`
	TranslateTo     string            `json:"translateTo"`
	Model           string            `json:"model"`
	TgBotToken      string            `json:"tgBotToken"`
	ChatId          string            `json:"chatId"`
	BaseURL         string            `json:"baseUrl"`
	ScrollDelay     int               `json:"scrollDelay"`
	EditorLoadDelay int               `json:"editorLoadDelay"`
	FocusDelay      int               `json:"focusDelay"`
	BeforeSaveDelay int               `json:"beforeSaveDelay"`
	RowNextDelay    int               `json:"rowNextDelay"`
	Projects        []string          `json:"projects"`
	Prompts         map[string]string `json:"prompts"`
}

type APIData struct {
	Config    FileConfig `json:"config"`
	IsRunning bool       `json:"isRunning"`
	Version   string     `json:"version"`
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

	os.MkdirAll("data", 0755)

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
	cfg := loadConfig()
	prompt, ok := cfg.Prompts[lang]
	if !ok || prompt == "" {
		prompt = getDefaultPrompt(lang)
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(prompt))
}

func handleGetData(w http.ResponseWriter, r *http.Request) {
	cfg := loadConfig()

	runMutex.Lock()
	rState := isRunning
	runMutex.Unlock()

	json.NewEncoder(w).Encode(APIData{
		Config:    cfg,
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

	saveConfig(req.Config)
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
	fileCfg := loadConfig()
	config := toInternalConfig(fileCfg)

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

	projects := config.Projects
	if len(projects) == 0 {
		slog.Warn("⚠️ Список проектов пуст.")
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

			if err := removeProjectFromConfig(projectURL); err != nil {
				slog.Warn("⚠️ Ошибка при удалении из файла", "url", projectURL, "error", err)
			}

			broker.BroadcastControl("___PROJECT_COMPLETED___|" + projectURL)
			slog.Info("✅ Завершено", "url", projectURL)
			messageText := fmt.Sprintf("✅ Завершено:\n<a href=\"%s\">%s</a>", projectURL, filename)
			notifyTelegram(config, tgBot, messageText)

		}(url, page)
	}

	wg.Wait()
}

var fileMutex sync.Mutex

func removeProjectFromConfig(urlToRemove string) error {
	fileMutex.Lock()
	defer fileMutex.Unlock()

	cfg := loadConfig()
	var newProjects []string
	for _, p := range cfg.Projects {
		if p != urlToRemove {
			newProjects = append(newProjects, p)
		}
	}
	cfg.Projects = newProjects
	saveConfig(cfg)
	return nil
}

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

	if _, err := os.Stat(filepath.Join("data", "auth.json")); err == nil {
		slog.Info("🔑 Найден файл авторизации, проверяем...")
		ctxOpts.StorageStatePath = playwright.String(filepath.Join("data", "auth.json"))
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
		if _, err := contextBrowser.StorageState(filepath.Join("data", "auth.json")); err != nil {
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

	var keys []string
	if config.GeminiAPIKey != "" {
		keys = append(keys, config.GeminiAPIKey)
	}
	if config.GeminiAPIKey2 != "" {
		keys = append(keys, config.GeminiAPIKey2)
	}
	if config.GeminiAPIKey3 != "" {
		keys = append(keys, config.GeminiAPIKey3)
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("нет доступных API ключей")
	}

	keyMutex.Lock()
	startIndex := currentKeyIndex % len(keys)
	keyMutex.Unlock()

	var body []byte
	var err error

	for i := 0; i < len(keys); i++ {
		idx := (startIndex + i) % len(keys)
		key := keys[idx]

		actualKeyNumber := 1
		if key == config.GeminiAPIKey2 {
			actualKeyNumber = 2
		} else if key == config.GeminiAPIKey3 {
			actualKeyNumber = 3
		}

		body, err = doCall(key)
		if err == nil {
			if i > 0 {
				keyMutex.Lock()
				currentKeyIndex = idx
				keyMutex.Unlock()
				slog.Info(fmt.Sprintf("🔄 Установлен новый дефолтный API ключ (ключ %d)", actualKeyNumber))
			}
			break
		} else {
			slog.Warn(fmt.Sprintf("⚠️ Ошибка с API ключом %d, пробуем следующий...", actualKeyNumber), "error", err)
		}
	}

	if err != nil {
		return nil, fmt.Errorf("все доступные ключи вернули ошибку: %v", err)
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
