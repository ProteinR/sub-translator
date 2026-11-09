package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"

	"github.com/playwright-community/playwright-go"
	"gopkg.in/telebot.v4"
)

func (a *app) runTranslation(parent context.Context) {
	// Свой cancel: при исчерпании бюджета останавливаем весь прогон, а не каждый
	// проект по отдельности с потоком одинаковых ошибок в Telegram.
	ctx, stopRun := context.WithCancelCause(parent)
	defer stopRun(nil)

	defer func() {
		if r := recover(); r != nil {
			slog.Error("💥 Паника в процессе перевода", "panic", r, "stack", string(debug.Stack()))
		}
		a.runMu.Lock()
		a.running = false
		if a.cancel != nil {
			a.cancel()
		}
		a.cancel = nil
		a.runMu.Unlock()
		a.broker.BroadcastControl("___PROCESS_STOPPED___")
		slog.Info("🏁 Процесс перевода завершен или остановлен.")
	}()

	slog.Info("🚀 Loka Translator Automation started", "version", AppVersion)
	fileCfg, err := a.cfg.Load()
	if err != nil {
		slog.Error("❌ Не удалось прочитать конфиг", "error", err)
		return
	}
	config := toInternalConfig(fileCfg)
	secrets.Set(config.APIKey, config.TgBotToken)

	if config.APIKey == "" {
		slog.Error("❌ Не задан Anthropic API key (поле в настройках или переменная ANTHROPIC_API_KEY)")
		return
	}
	if len(config.Projects) == 0 {
		slog.Warn("⚠️ Список проектов пуст.")
		return
	}

	notify := newNotifier(config.TgBotToken, config.ChatId)

	usage := newUsageTracker(config.BudgetUSD, func(s usageSnapshot) {
		if data, err := json.Marshal(s); err == nil {
			a.broker.BroadcastControl("___USAGE___|" + string(data))
		}
	})
	usage.notify(usage.Snapshot()) // обнуляем счётчик в UI
	defer func() {
		s := usage.Snapshot()
		slog.Info("💰 Итог прогона", "requests", s.Requests, "in_tokens", s.InputTokens, "out_tokens", s.OutputTokens,
			"cache_read", s.CacheRead, "from_memory", s.MemoryHits, "cost_usd", fmt.Sprintf("%.4f", s.CostUSD))
	}()

	memory, err := openTranslationMemory(filepath.Join("data", "tm.jsonl"))
	if err != nil {
		slog.Error("❌ Не удалось открыть память переводов", "error", err)
		return
	}
	defer memory.Close()
	slog.Info("🧠 Память переводов загружена", "entries", memory.Len())

	tr := &cachedTranslator{
		inner: newAnthropicClient(config.APIKey, config.Model, config.FallbackModel, config.Prompt, usage),
		tm:    memory,
		scope: memoryScope(config.Model, config.Prompt),
		usage: usage,
	}
	if config.FallbackModel != "" {
		slog.Info("🔀 Запасная модель", "model", config.FallbackModel)
	}
	if config.BudgetUSD > 0 {
		slog.Info("💰 Лимит бюджета на прогон", "usd", config.BudgetUSD)
	}

	pw, err := playwright.Run()
	if err != nil {
		slog.Error("could not start playwright", "error", err)
		return
	}
	defer pw.Stop()

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(false)})
	if err != nil {
		slog.Error("could not launch browser", "error", err)
		return
	}
	defer browser.Close()

	slog.Info("🌐 Открываем первый проект для проверки авторизации...")
	bctx, firstPage, err := ensureLogin(ctx, browser, config.Projects[0])
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Warn("⚠️ Остановлено во время авторизации")
		} else {
			slog.Error("Login failed", "error", err)
		}
		return
	}
	defer bctx.Close()
	// Дефолт Playwright — 30с на действие; для одной ячейки это слишком долго.
	bctx.SetDefaultTimeout(15000)
	bctx.SetDefaultNavigationTimeout(60000)

	slog.Info("📋 Найдено проектов", "count", len(config.Projects), "threads", config.MaxConcurrency)

	pagePool := make(chan playwright.Page, config.MaxConcurrency)
	pagePool <- firstPage
	for i := 1; i < config.MaxConcurrency; i++ {
		if p, err := bctx.NewPage(); err == nil {
			pagePool <- p
		} else {
			slog.Warn("⚠️ Не удалось открыть дополнительную вкладку", "error", err)
		}
	}

	modeText := "только пустые"
	if config.OverwriteFilled {
		modeText = "перезапись всех"
	}

	var wg sync.WaitGroup
	var budgetOnce sync.Once
loop:
	for _, projectURL := range config.Projects {
		var page playwright.Page
		select {
		case <-ctx.Done():
			slog.Warn("⚠️ Обработка прервана пользователем.")
			break loop
		case page = <-pagePool:
		}

		wg.Add(1)
		go func(projectURL string, p playwright.Page) {
			defer wg.Done()
			defer func() { pagePool <- p }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("💥 Паника при обработке проекта", "url", projectURL, "panic", r, "stack", string(debug.Stack()))
				}
			}()

			slog.Info(fmt.Sprintf("🚀 Старт обработки (Режим: %s)", modeText), "url", projectURL)
			filename, err := processProject(ctx, p, projectURL, config, tr)
			if err != nil {
				if errors.Is(err, errBudgetExceeded) {
					budgetOnce.Do(func() {
						slog.Error("🛑 "+err.Error()+" — останавливаю прогон", "url", projectURL)
						notify.Send(html.EscapeString("🛑 " + err.Error() + ". Прогон остановлен, проекты остались в очереди."))
						stopRun(err)
					})
					return
				}
				if errors.Is(err, context.Canceled) {
					slog.Warn("⚠️ Остановлено", "url", projectURL)
					return
				}
				slog.Error("❌ Ошибка обработки", "file", filename, "url", projectURL, "error", err)
				notify.Send(projectMessage(false, projectURL, filename, err))
				return
			}

			if err := a.cfg.RemoveProject(projectURL); err != nil {
				slog.Warn("⚠️ Ошибка при удалении из очереди", "url", projectURL, "error", err)
			}
			a.broker.BroadcastControl("___PROJECT_COMPLETED___|" + projectURL)
			slog.Info("✅ Завершено", "url", projectURL)
			notify.Send(projectMessage(true, projectURL, filename, nil))
		}(projectURL, page)
	}
	wg.Wait()
}

// ============================================================
// Telegram
// ============================================================

type notifier struct {
	bot  *telebot.Bot
	chat telebot.ChatID
}

func newNotifier(token, chatID string) *notifier {
	if token == "" || chatID == "" {
		return nil
	}
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		slog.Error("Некорректный Telegram ChatId", "error", err)
		return nil
	}
	bot, err := telebot.NewBot(telebot.Settings{Token: token})
	if err != nil {
		slog.Error("Ошибка создания Telegram-бота", "error", err)
		return nil
	}
	return &notifier{bot: bot, chat: telebot.ChatID(id)}
}

func (n *notifier) Send(text string) {
	if n == nil {
		return
	}
	_, err := n.bot.Send(n.chat, text, &telebot.SendOptions{
		ParseMode:             telebot.ModeHTML,
		DisableWebPagePreview: true,
	})
	if err != nil {
		slog.Warn("⚠️ Не удалось отправить уведомление в Telegram", "error", err)
	}
}

// projectMessage экранирует всё, что пришло извне: символ "<" в тексте ошибки
// раньше ломал HTML-разметку, и Telegram молча отбрасывал сообщение.
func projectMessage(ok bool, projectURL, name string, err error) string {
	link := fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(projectURL), html.EscapeString(name))
	if ok {
		return "✅ Завершено:\n" + link
	}
	return fmt.Sprintf("❌ Ошибка обработки:\n%s\nОшибка: %s", link, html.EscapeString(truncate(secrets.Redact(err.Error()), 1500)))
}
