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
	a.resetStatuses(config.Projects)

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

	pagePool, err := a.workerTabs(config.MaxConcurrency)
	if err != nil {
		slog.Error("❌ " + err.Error())
		return
	}
	a.eng.setBusy(true)
	defer a.eng.setBusy(false)

	slog.Info("📋 Найдено проектов", "count", len(config.Projects), "threads", config.MaxConcurrency)

	modeText := "только пустые"
	if config.OverwriteFilled {
		modeText = "перезапись всех"
	}

	var wg sync.WaitGroup
	var budgetOnce sync.Once
loop:
	for _, projectURL := range config.Projects {
		var page *browserTab
		select {
		case <-ctx.Done():
			slog.Warn("⚠️ Обработка прервана пользователем.")
			break loop
		case page = <-pagePool:
		}

		wg.Add(1)
		go func(projectURL string, p *browserTab) {
			defer wg.Done()
			defer func() { pagePool <- p }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("💥 Паника при обработке проекта", "url", projectURL, "panic", r, "stack", string(debug.Stack()))
				}
			}()

			slog.Info(fmt.Sprintf("🚀 Старт обработки (Режим: %s)", modeText), "url", projectURL)
			rep := &projectReporter{st: projectStatus{URL: projectURL, Status: statusRunning}, emit: a.setStatus}
			filename, err := processProject(ctx, p, projectURL, config, tr, rep)
			if err != nil {
				if errors.Is(err, errBudgetExceeded) {
					rep.update(func(s *projectStatus) { s.Status = statusPending; s.Detail = humanizeError(err) })
					budgetOnce.Do(func() {
						slog.Error("🛑 "+err.Error()+" — останавливаю прогон", "url", projectURL)
						notify.Send(html.EscapeString("🛑 " + err.Error() + ". Прогон остановлен, проекты остались в очереди."))
						stopRun(err)
					})
					return
				}
				if errors.Is(err, context.Canceled) {
					slog.Warn("⚠️ Остановлено", "url", projectURL)
					rep.update(func(s *projectStatus) {
						s.Status = statusPending
						s.Detail = "Остановлено — продолжится при следующем запуске"
						if errors.Is(context.Cause(ctx), errBudgetExceeded) {
							s.Detail = humanizeError(errBudgetExceeded)
						}
					})
					return
				}
				slog.Error("❌ Ошибка обработки", "file", filename, "url", projectURL, "error", err)
				rep.update(func(s *projectStatus) { s.Status = statusError; s.Detail = humanizeError(err) })
				notify.Send(projectMessage(false, projectURL, filename, err))
				return
			}

			if err := a.cfg.RemoveProject(projectURL); err != nil {
				slog.Warn("⚠️ Ошибка при удалении из очереди", "url", projectURL, "error", err)
			}
			rep.update(func(s *projectStatus) {
				s.Status = statusDone
				if s.Total > 0 {
					s.Detail = fmt.Sprintf("Готово · %d ячеек", s.Total)
				}
			})
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

// workerTabs возвращает пул вкладок для потоков: вкладки создаются один раз и
// остаются в окне между прогонами (вход в Lokalise общий для всех).
func (a *app) workerTabs(n int) (chan *browserTab, error) {
	if !hasNativeWindow {
		return nil, errNoBrowser
	}
	a.tabsMu.Lock()
	defer a.tabsMu.Unlock()
	for len(a.tabs) < n {
		id := len(a.tabs) + 1
		title := fmt.Sprintf("Поток %d", id)
		a.eng.newTab(id, title)
		a.tabs = append(a.tabs, &browserTab{id: id, eng: a.eng})
	}
	pool := make(chan *browserTab, n)
	for _, t := range a.tabs[:n] {
		pool <- t
	}
	return pool, nil
}
