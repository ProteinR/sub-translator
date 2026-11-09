package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/playwright-community/playwright-go"
)

const (
	authStatePath  = "data/auth.json"
	editorSelector = ".ace_text-input, textarea:not([style*='display: none']), [contenteditable='true']"
	rowAttempts    = 2 // попытки вставки одной строки
	// Столько подряд упавших строк — и считаем, что страница сломалась.
	maxConsecutiveRowFailures = 5
)

func ensureLogin(ctx context.Context, browser playwright.Browser, checkURL string) (playwright.BrowserContext, playwright.Page, error) {
	var opts playwright.BrowserNewContextOptions
	if _, err := os.Stat(authStatePath); err == nil {
		slog.Info("🔑 Найден файл авторизации, проверяем...")
		opts.StorageStatePath = playwright.String(authStatePath)
	}

	bctx, err := browser.NewContext(opts)
	if err != nil {
		return nil, nil, err
	}
	page, err := bctx.NewPage()
	if err != nil {
		bctx.Close()
		return nil, nil, err
	}
	if _, err = page.Goto(checkURL); err != nil {
		bctx.Close()
		return nil, nil, err
	}

	loginHeader := page.Locator("h1", playwright.PageLocatorOptions{HasText: "Log in"})
	if err := loginHeader.WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(5000)}); err != nil {
		slog.Info("✅ Куки валидны, вход не требуется.")
		return bctx, page, nil
	}

	slog.Warn("⚠️ Требуется вход. Пожалуйста, залогиньтесь в открывшемся окне браузера!")
	// Раньше тут был Click с дефолтным таймаутом 30с, если баннера нет — минус полминуты.
	_ = page.Locator("[id='onetrust-accept-btn-handler']").Click(playwright.LocatorClickOptions{Timeout: playwright.Float(2000)})

	deadline := time.Now().Add(3 * time.Minute)
	for {
		if count, _ := loginHeader.Count(); count == 0 {
			break
		}
		if time.Now().After(deadline) {
			bctx.Close()
			return nil, nil, errors.New("превышено время ожидания авторизации")
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			bctx.Close()
			return nil, nil, err
		}
	}

	if _, err := bctx.StorageState(authStatePath); err != nil {
		bctx.Close()
		return nil, nil, fmt.Errorf("could not save storage state: %w", err)
	}
	_ = os.Chmod(authStatePath, 0o600)
	slog.Info("💾 Авторизация сохранена")
	return bctx, page, nil
}

type translationJob struct {
	group string // документ + исходный язык: внутри группы тянем контекст
	items []TranslationItem
}

type jobResult struct {
	job   translationJob
	items []TranslationItem
	err   error
}

// Сколько предыдущих строк отдавать модели как контекст.
const (
	contextRows     = 3
	contextMaxRunes = 400
)

func processProject(ctx context.Context, page playwright.Page, projectURL string, config Config, tr translator) (string, error) {
	parsedURL, err := url.Parse(projectURL)
	if err != nil {
		return "", err
	}
	query := parsedURL.Query()
	query.Set("view", "multi")
	parsedURL.RawQuery = query.Encode()
	if _, err := page.Goto(parsedURL.String()); err != nil {
		return "", err
	}
	if err := page.Locator(".row-key[data-id]").First().WaitFor(playwright.LocatorWaitForOptions{Timeout: playwright.Float(60000)}); err != nil {
		return "", fmt.Errorf("не найдены строки Multilingual: %w", err)
	}
	filename := projectURL
	if names, err := page.Locator(".project-document__file-name").AllTextContents(); err == nil && len(names) > 0 {
		filename = strings.Join(names, ", ")
	}

	collected, err := scrollAndCollect(ctx, page, config, filename)
	if err != nil {
		return filename, fmt.Errorf("scroll error: %w", err)
	}
	if len(collected) == 0 {
		slog.Info("ℹ️ Пустых строк не найдено", "url", projectURL)
		return filename, nil
	}

	jobs := buildJobs(collected)

	// Конвейер: пока браузер вставляет пачку N, модель уже переводит пачку N+1.
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := runTranslationPipeline(pctx, tr, jobs)

	var failed []string
	for res := range results {
		if res.err != nil {
			return filename, fmt.Errorf("translation error: %w", res.err)
		}
		f, err := fillTranslations(ctx, page, res.items, config)
		failed = append(failed, f...)
		if err != nil {
			return filename, err
		}
	}
	if len(failed) > 0 {
		return filename, fmt.Errorf("не удалось вставить %d строк: %s", len(failed), strings.Join(failed, ", "))
	}
	return filename, nil
}

// buildJobs группирует ячейки по документу и исходному языку (все целевые языки
// строки — в одном запросе), сохраняя порядок строк, и режет на пачки.
func buildJobs(items []TranslationItem) []translationJob {
	var groups [][]TranslationItem
	var groupKeys []string
	index := map[string]int{}
	for _, item := range items {
		key := item.Filename + "\x00" + item.SourceLang
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, nil)
			groupKeys = append(groupKeys, key)
		}
		groups[i] = append(groups[i], item)
	}

	var jobs []translationJob
	for gi, group := range groups {
		slog.Info("🌍 Определено направление", "source", group[0].SourceLang, "targets", targetsOf(group), "file", group[0].Filename, "cells", len(group))
		for _, batch := range splitBatches(group, maxBatchChars, maxBatchRows) {
			jobs = append(jobs, translationJob{group: groupKeys[gi], items: batch})
		}
	}
	return jobs
}

// runTranslationPipeline переводит пачки по очереди в фоне (буфер 1: модель
// работает над следующей пачкой, пока браузер вставляет текущую) и подкладывает
// в каждый запрос последние строки предыдущей пачки той же группы.
func runTranslationPipeline(ctx context.Context, tr translator, jobs []translationJob) <-chan jobResult {
	out := make(chan jobResult, 1)
	go func() {
		defer close(out)
		lastRows := map[string][]contextRow{}
		for _, job := range jobs {
			items, err := tr.Translate(ctx, translateRequest{Items: job.items, Context: lastRows[job.group]})
			if err == nil {
				lastRows[job.group] = contextFrom(items, contextRows)
			}
			select {
			case out <- jobResult{job: job, items: items, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// contextFrom берёт последние n строк пачки со всеми их переводами.
func contextFrom(items []TranslationItem, n int) []contextRow {
	var rows []contextRow
	for i := len(items) - 1; i >= 0 && len(rows) < n; {
		j := i
		row := contextRow{Text: truncate(items[i].Original, contextMaxRunes), Translations: map[string]string{}}
		for j >= 0 && items[j].ID == items[i].ID {
			row.Translations[items[j].TargetLang] = truncate(items[j].Translation, contextMaxRunes)
			j--
		}
		rows = append(rows, row)
		i = j
	}
	slices.Reverse(rows)
	return rows
}

func scrollAndCollect(ctx context.Context, page playwright.Page, config Config, filename string) ([]TranslationItem, error) {
	var results []TranslationItem
	seen := make(map[string]bool)

	noNewElementsCount := 0
	const maxNoNewRetries = 5

	modeLog := "пустых"
	if config.OverwriteFilled {
		modeLog = "всех"
	}
	slog.Info(fmt.Sprintf("🔍 Начинаю поиск %s строк", modeLog), "file", filename)

	for noNewElementsCount < maxNoNewRetries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		newAddedThisStep := 0
		rows, err := page.Locator(".row-key[data-id]").All()
		if err != nil {
			return nil, err
		}

		for _, row := range rows {
			id, _ := row.GetAttribute("data-id")
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			newAddedThisStep++

			source := row.Locator(".key-translation[data-is-base='1']")
			if count, _ := source.Count(); count != 1 {
				return nil, fmt.Errorf("строка %s: исходный язык не определён", id)
			}
			sourceName, err := source.GetAttribute("data-lang")
			if err != nil || strings.TrimSpace(sourceName) == "" {
				return nil, fmt.Errorf("строка %s: отсутствует название исходного языка", id)
			}
			sourceID, _ := source.GetAttribute("data-lang-id")
			originalText, err := cellValue(source)
			if err != nil {
				return nil, err
			}
			targets, err := row.Locator(".key-translation:not([data-is-base='1'])").All()
			if err != nil {
				return nil, err
			}
			if len(targets) == 0 {
				return nil, fmt.Errorf("строка %s: нет целевых языков; проверьте фильтр языков Lokalise", id)
			}
			rowFile, _ := row.GetAttribute("data-filename")
			seenLanguages := map[string]bool{}
			seenNames := map[string]bool{}
			for _, target := range targets {
				targetID, _ := target.GetAttribute("data-lang-id")
				targetName, _ := target.GetAttribute("data-lang")
				if _, err := strconv.ParseUint(targetID, 10, 64); err != nil || targetName == "" || targetID == sourceID || seenLanguages[targetID] {
					return nil, fmt.Errorf("строка %s: некорректные данные целевого языка", id)
				}
				if seenNames[targetName] {
					return nil, fmt.Errorf("строка %s: два целевых языка с одинаковым названием %q", id, targetName)
				}
				seenLanguages[targetID] = true
				seenNames[targetName] = true
				cellText, err := cellValue(target)
				if err != nil {
					return nil, err
				}
				if strings.TrimSpace(originalText) != "" && (strings.TrimSpace(cellText) == "" || config.OverwriteFilled) {
					results = append(results, TranslationItem{ID: id, Original: originalText, Filename: rowFile, SourceLang: sourceName, TargetLang: targetName, TargetID: targetID})
				}
			}
		}

		if newAddedThisStep > 0 {
			noNewElementsCount = 0
		} else {
			noNewElementsCount++
		}

		_ = page.Mouse().Wheel(0, 800)
		if err := sleepCtx(ctx, config.ScrollDelay); err != nil {
			return nil, err
		}
	}

	if err := scrollToTop(ctx, page); err != nil {
		return nil, err
	}

	if len(seen) == 0 {
		return nil, errors.New("не найдено строк для обработки")
	}
	slog.Info("✅ Сбор данных завершен", "file", filename, "checked", len(seen), "collected", len(results))
	return results, nil
}

func scrollToTop(ctx context.Context, page playwright.Page) error {
	for i := 0; i < 40; i++ {
		_ = page.Mouse().Wheel(0, -3000)
		if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
			return err
		}
	}
	return sleepCtx(ctx, 300*time.Millisecond)
}

// fillTranslations вставляет пачку. Упавшие строки не валят весь проект: они
// возвращаются списком, а проект остаётся в очереди для повторного прогона.
// Ошибка возвращается только при отмене или если страница явно сломалась.
func fillTranslations(ctx context.Context, page playwright.Page, items []TranslationItem, config Config) ([]string, error) {
	slog.Info("✍️ Вставка переводов...", "cells", len(items), "langs", targetsOf(items))
	if err := scrollToTop(ctx, page); err != nil {
		return nil, err
	}

	var failed []string
	consecutive := 0
	for _, item := range items {
		var err error
		for attempt := 0; attempt < rowAttempts; attempt++ {
			if err = ctx.Err(); err != nil {
				return failed, err
			}
			if err = fillRow(ctx, page, item, config, attempt); err == nil {
				break
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return failed, err
			}
			slog.Warn("⚠️ Строка не вставилась", "id", item.ID, "lang", item.TargetLang, "attempt", attempt+1, "error", err)
			_ = page.Keyboard().Press("Escape") // закрываем возможный зависший редактор
		}
		if err != nil {
			failed = append(failed, item.ID+"/"+item.TargetLang)
			consecutive++
			if consecutive >= maxConsecutiveRowFailures {
				return failed, fmt.Errorf("%d строк подряд не вставились, похоже страница сломалась; последняя ошибка: %w", consecutive, err)
			}
			continue
		}
		consecutive = 0
	}
	return failed, nil
}

func fillRow(ctx context.Context, page playwright.Page, item TranslationItem, config Config, attempt int) error {
	row, err := findRow(ctx, page, item.ID)
	if err != nil {
		return err
	}
	if err := row.ScrollIntoViewIfNeeded(); err != nil {
		return fmt.Errorf("could not scroll to row: %w", err)
	}
	targetCell := row.Locator(targetCellSelector(item.TargetID))

	// На повторной попытке ячейка может содержать наш же кривой текст — перезаписываем.
	if attempt == 0 && !config.OverwriteFilled {
		current, err := cellValue(targetCell)
		if err != nil {
			return err
		}
		if strings.TrimSpace(current) != "" {
			return nil
		}
	}

	clickTarget := targetCell.Locator(".highlight").First()
	if err := clickTarget.ScrollIntoViewIfNeeded(); err == nil {
		// Сдвигаем экран чуть вверх, чтобы ячейку не перекрывал sticky-хэдер
		_, _ = page.Evaluate(`window.scrollBy(0, -150)`)
	}
	if err := clickTarget.Click(); err != nil {
		return fmt.Errorf("could not click cell: %w", err)
	}

	editor := targetCell.Locator(editorSelector).First()
	visible := playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateVisible, Timeout: playwright.Float(5000)}
	if err := editor.WaitFor(visible); err != nil {
		_ = clickTarget.Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)})
		visible.Timeout = playwright.Float(3000)
		if err := editor.WaitFor(visible); err != nil {
			path := saveDebugArtifacts(page, "error_editor_open")
			return fmt.Errorf("editor did not appear for row %s (debug: %s): %w", item.ID, path, err)
		}
	}

	// Фокус именно в редакторе, чтобы select-all выделил текст в нём, а не всю страницу.
	if err := editor.Focus(); err != nil {
		_ = editor.Click(playwright.LocatorClickOptions{Force: playwright.Bool(true)})
	}
	if err := sleepCtx(ctx, config.EditorLoadDelay); err != nil {
		return err
	}

	selectAll, deleteWord := "Control+a", ""
	if runtime.GOOS == "darwin" {
		selectAll, deleteWord = "Meta+a", "Meta+Backspace"
	}
	_ = editor.Press(selectAll)
	if deleteWord != "" {
		_ = editor.Press(deleteWord)
	}
	_ = editor.Press("Backspace")

	// Первая попытка — вставка одним событием (быстро даже для длинных скриптов).
	// Если не подтвердилось — посимвольный ввод, как было раньше.
	if attempt == 0 {
		err = page.Keyboard().InsertText(item.Translation)
	} else {
		err = editor.PressSequentially(item.Translation)
	}
	if err != nil {
		return fmt.Errorf("could not type translation: %w", err)
	}

	if err := sleepCtx(ctx, config.BeforeSaveDelay); err != nil {
		return err
	}

	saveBtn := targetCell.Locator("button.save.btn-primary")
	if err := saveBtn.Click(playwright.LocatorClickOptions{Timeout: playwright.Float(5000)}); err != nil {
		path := saveDebugArtifacts(page, "error_click_btn")
		return fmt.Errorf("could not click Save for row %s (debug: %s): %w", item.ID, path, err)
	}

	_ = editor.WaitFor(playwright.LocatorWaitForOptions{State: playwright.WaitForSelectorStateHidden, Timeout: playwright.Float(5000)})

	saved, err := cellValue(targetCell)
	if err != nil {
		return fmt.Errorf("не прочитано сохранённое значение: %w", err)
	}
	if normalizeText(saved) != normalizeText(item.Translation) {
		return fmt.Errorf("не подтверждено сохранение строки %s (%s)", item.ID, item.TargetLang)
	}
	return sleepCtx(ctx, config.RowNextDelay)
}

// findRow ищет строку виртуализированного списка: листает вниз, а если не нашёл —
// возвращается наверх и проходит ещё раз (раньше строки выше экрана терялись).
func findRow(ctx context.Context, page playwright.Page, id string) (playwright.Locator, error) {
	loc := page.Locator(fmt.Sprintf(".row-key[data-id=%s]", cssString(id)))
	for pass := 0; pass < 2; pass++ {
		for k := 0; k < 60; k++ {
			if count, _ := loc.Count(); count > 0 {
				return loc.First(), nil
			}
			_ = page.Mouse().Wheel(0, 800)
			if err := sleepCtx(ctx, 150*time.Millisecond); err != nil {
				return nil, err
			}
		}
		if err := scrollToTop(ctx, page); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("строка %s не найдена в DOM после прокрутки", id)
}

func cssString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// normalizeText сравнивает тексты без учёта различий в пробелах и переносах,
// которые Lokalise может нормализовать при сохранении.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	return strings.Join(strings.Fields(s), " ")
}

func saveDebugArtifacts(page playwright.Page, prefix string) string {
	dir := filepath.Join("data", "logs")
	_ = os.MkdirAll(dir, 0o755)
	stamp := time.Now().Format("20060102_150405")
	shot := filepath.Join(dir, fmt.Sprintf("%s_%s.png", prefix, stamp))
	if _, err := page.Screenshot(playwright.PageScreenshotOptions{Path: playwright.String(shot)}); err != nil {
		slog.Error("Не удалось сохранить скриншот", "error", err)
	}
	if content, err := page.Content(); err == nil {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s_%s.html", prefix, stamp)), []byte(content), 0o600)
	}
	return shot
}

func targetCellSelector(langID string) string {
	return fmt.Sprintf(".key-translation[data-lang-id=%s]:not([data-is-base='1'])", cssString(langID))
}

func cellValue(cell playwright.Locator) (string, error) {
	value := cell.Locator("[data-lokalise-editor-value]")
	count, err := value.Count()
	if err != nil {
		return "", err
	}
	if count == 1 {
		return value.GetAttribute("data-lokalise-editor-value")
	}
	if count, _ := cell.Locator(".highlight .empty").Count(); count > 0 {
		return "", nil
	}
	return cell.Locator(".highlight").First().InnerText()
}
