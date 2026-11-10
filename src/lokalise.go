package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	rowAttempts = 2 // попытки вставки одной строки
	// Столько подряд упавших строк — и считаем, что страница сломалась.
	maxConsecutiveRowFailures = 5
	loginTimeout              = 10 * time.Minute
	evalTimeout               = 30 * time.Second
)

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

type pageState struct {
	URL   string   `json:"url"`
	Ready string   `json:"ready"`
	Rows  int      `json:"rows"`
	Login bool     `json:"login"`
	Files []string `json:"files"`
}

func (t *browserTab) call(ctx context.Context, op string, args, out any) error {
	cctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	return t.Call(cctx, op, args, out)
}

// openProject открывает проект в Multilingual и ждёт строки. Если Lokalise просит
// войти — показывает вкладку и ждёт, пока пользователь залогинится в окне.
func openProject(ctx context.Context, tab *browserTab, projectURL string, rep *projectReporter) (pageState, error) {
	parsedURL, err := url.Parse(projectURL)
	if err != nil {
		return pageState{}, err
	}
	query := parsedURL.Query()
	query.Set("view", "multi")
	parsedURL.RawQuery = query.Encode()
	target := parsedURL.String()

	rep.detail("Открываю проект в Lokalise…")
	tab.Navigate(target)
	if err := sleepCtx(ctx, time.Second); err != nil {
		return pageState{}, err
	}

	deadline := time.Now().Add(60 * time.Second)
	askedLogin := false
	lastNav := time.Now()
	var st pageState
	for {
		err := tab.call(ctx, "state", nil, &st)
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		onLogin := err == nil && (st.Login || strings.Contains(st.URL, "/login"))
		switch {
		case err == nil && st.Rows > 0:
			if askedLogin {
				slog.Info("✅ Вход выполнен")
			}
			return st, nil
		case onLogin && !askedLogin:
			askedLogin = true
			deadline = time.Now().Add(loginTimeout)
			slog.Warn("⚠️ Требуется вход. Войдите в Lokalise во вкладке окна приложения.")
			rep.detail("Нужен вход в Lokalise — войдите во вкладке этого потока")
			tab.Show()
		case askedLogin && err == nil && !onLogin && st.Ready == "complete" &&
			!strings.Contains(st.URL, parsedURL.Path) && time.Since(lastNav) > 10*time.Second:
			// Вошли, но Lokalise привёл на дашборд — открываем проект заново.
			tab.Navigate(target)
			lastNav = time.Now()
		}
		if time.Now().After(deadline) {
			if askedLogin {
				return st, errors.New("превышено время ожидания входа в Lokalise")
			}
			return st, fmt.Errorf("не найдены строки Multilingual (адрес %s)", st.URL)
		}
		if err := sleepCtx(ctx, 700*time.Millisecond); err != nil {
			return st, err
		}
	}
}

func processProject(ctx context.Context, tab *browserTab, projectURL string, config Config, tr translator, rep *projectReporter) (string, error) {
	st, err := openProject(ctx, tab, projectURL, rep)
	if err != nil {
		return "", err
	}
	filename := projectURL
	if len(st.Files) > 0 {
		filename = strings.Join(st.Files, ", ")
	}
	tab.SetTitle(filename)
	rep.update(func(s *projectStatus) { s.Name = filename; s.Detail = "Ищу строки для перевода…" })

	collected, err := scrollAndCollect(ctx, tab, config, filename)
	if err != nil {
		return filename, fmt.Errorf("scroll error: %w", err)
	}
	if len(collected) == 0 {
		slog.Info("ℹ️ Пустых строк не найдено", "url", projectURL)
		rep.detail("Пустых ячеек нет — переводить нечего")
		return filename, nil
	}
	rep.update(func(s *projectStatus) {
		s.Total = len(collected)
		s.Detail = "Перевожу и вставляю · " + targetsOf(collected)
	})
	onCell := func() { rep.update(func(s *projectStatus) { s.Done++ }) }

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
		f, err := fillTranslations(ctx, tab, res.items, config, onCell)
		failed = append(failed, f...)
		if err != nil {
			return filename, err
		}
	}
	if len(failed) > 0 {
		return filename, fmt.Errorf("не удалось вставить %d ячеек: %s", len(failed), strings.Join(firstN(failed, 5), ", "))
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

type scannedTarget struct {
	LangID string `json:"langId"`
	Lang   string `json:"lang"`
	Value  string `json:"value"`
}

type scannedRow struct {
	ID         string          `json:"id"`
	File       string          `json:"file"`
	BaseCount  int             `json:"baseCount"`
	SourceLang string          `json:"sourceLang"`
	SourceID   string          `json:"sourceId"`
	Source     string          `json:"source"`
	Targets    []scannedTarget `json:"targets"`
}

// itemsFromRow проверяет строку Lokalise и возвращает ячейки, которые надо перевести.
func itemsFromRow(row scannedRow, overwrite bool) ([]TranslationItem, error) {
	id := row.ID
	if row.BaseCount != 1 {
		return nil, fmt.Errorf("строка %s: исходный язык не определён", id)
	}
	if strings.TrimSpace(row.SourceLang) == "" {
		return nil, fmt.Errorf("строка %s: отсутствует название исходного языка", id)
	}
	if len(row.Targets) == 0 {
		return nil, fmt.Errorf("строка %s: нет целевых языков; проверьте фильтр языков Lokalise", id)
	}
	var out []TranslationItem
	seenLanguages := map[string]bool{}
	seenNames := map[string]bool{}
	for _, t := range row.Targets {
		if _, err := strconv.ParseUint(t.LangID, 10, 64); err != nil || t.Lang == "" || t.LangID == row.SourceID || seenLanguages[t.LangID] {
			return nil, fmt.Errorf("строка %s: некорректные данные целевого языка", id)
		}
		if seenNames[t.Lang] {
			return nil, fmt.Errorf("строка %s: два целевых языка с одинаковым названием %q", id, t.Lang)
		}
		seenLanguages[t.LangID] = true
		seenNames[t.Lang] = true
		if strings.TrimSpace(row.Source) != "" && (strings.TrimSpace(t.Value) == "" || overwrite) {
			out = append(out, TranslationItem{ID: id, Original: row.Source, Filename: row.File, SourceLang: row.SourceLang, TargetLang: t.Lang, TargetID: t.LangID})
		}
	}
	return out, nil
}

type scrollResult struct {
	Top   float64 `json:"top"`
	Moved bool    `json:"moved"`
	Max   float64 `json:"max"`
}

func scrollAndCollect(ctx context.Context, tab *browserTab, config Config, filename string) ([]TranslationItem, error) {
	var results []TranslationItem
	seen := make(map[string]bool)

	noNewElementsCount := 0
	const maxNoNewRetries = 5

	modeLog := "пустых"
	if config.OverwriteFilled {
		modeLog = "всех"
	}
	slog.Info(fmt.Sprintf("🔍 Начинаю поиск %s строк", modeLog), "file", filename)
	if err := tab.call(ctx, "top", nil, nil); err != nil {
		return nil, err
	}

	for noNewElementsCount < maxNoNewRetries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var rows []scannedRow
		if err := tab.call(ctx, "scan", nil, &rows); err != nil {
			return nil, err
		}
		newAddedThisStep := 0
		for _, row := range rows {
			if row.ID == "" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			newAddedThisStep++
			items, err := itemsFromRow(row, config.OverwriteFilled)
			if err != nil {
				return nil, err
			}
			results = append(results, items...)
		}

		var sr scrollResult
		if err := tab.call(ctx, "scroll", map[string]int{"dy": 800}, &sr); err != nil {
			return nil, err
		}
		if newAddedThisStep > 0 {
			noNewElementsCount = 0
		} else {
			noNewElementsCount++
		}
		if !sr.Moved && newAddedThisStep == 0 {
			// Дошли до конца: ждём один раз подгрузку и выходим быстрее, чем через 5 шагов.
			noNewElementsCount += 2
		}
		if err := sleepCtx(ctx, config.ScrollDelay); err != nil {
			return nil, err
		}
	}

	if err := tab.call(ctx, "top", nil, nil); err != nil {
		return nil, err
	}
	if len(seen) == 0 {
		return nil, errors.New("не найдено строк для обработки")
	}
	slog.Info("✅ Сбор данных завершен", "file", filename, "checked", len(seen), "collected", len(results))
	return results, nil
}

// fillTranslations вставляет пачку. Упавшие строки не валят весь проект: они
// возвращаются списком, а проект остаётся в очереди для повторного прогона.
// Ошибка возвращается только при отмене или если страница явно сломалась.
func fillTranslations(ctx context.Context, tab *browserTab, items []TranslationItem, config Config, onCell func()) ([]string, error) {
	slog.Info("✍️ Вставка переводов...", "cells", len(items), "langs", targetsOf(items))

	var failed []string
	consecutive := 0
	for _, item := range items {
		var err error
		for attempt := 0; attempt < rowAttempts; attempt++ {
			if err = ctx.Err(); err != nil {
				return failed, err
			}
			if err = fillRow(ctx, tab, item, config, attempt); err == nil {
				break
			}
			if errors.Is(err, context.Canceled) {
				return failed, err
			}
			slog.Warn("⚠️ Строка не вставилась", "id", item.ID, "lang", item.TargetLang, "attempt", attempt+1, "error", err)
			_ = tab.call(ctx, "escape", nil, nil) // закрываем возможный зависший редактор
		}
		if onCell != nil {
			onCell()
		}
		if err != nil {
			saveDebugSnapshot(ctx, tab, "error_fill")
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

type fillResult struct {
	Status string `json:"status"`
	Kind   string `json:"kind"`
}

func fillRow(ctx context.Context, tab *browserTab, item TranslationItem, config Config, attempt int) error {
	if err := findRow(ctx, tab, item.ID); err != nil {
		return err
	}
	args := map[string]any{
		"id":          item.ID,
		"langId":      item.TargetID,
		"text":        item.Translation,
		"overwrite":   config.OverwriteFilled,
		"attempt":     attempt,
		"editorDelay": config.EditorLoadDelay.Milliseconds(),
		"saveDelay":   config.BeforeSaveDelay.Milliseconds(),
	}
	var res fillResult
	if err := tab.call(ctx, "fill", args, &res); err != nil {
		return err
	}
	if res.Status == "missing" {
		return fmt.Errorf("строка %s пропала из DOM", item.ID)
	}
	return sleepCtx(ctx, config.RowNextDelay)
}

// findRow ищет строку виртуализированного списка: листает вниз, а если не нашёл —
// возвращается наверх и проходит ещё раз.
func findRow(ctx context.Context, tab *browserTab, id string) error {
	args := map[string]string{"id": id}
	for pass := 0; pass < 2; pass++ {
		for k := 0; k < 200; k++ {
			var found bool
			if err := tab.call(ctx, "has", args, &found); err != nil {
				return err
			}
			if found {
				return nil
			}
			var sr scrollResult
			if err := tab.call(ctx, "scroll", map[string]int{"dy": 800}, &sr); err != nil {
				return err
			}
			if !sr.Moved && k > 0 {
				break
			}
			if err := sleepCtx(ctx, 150*time.Millisecond); err != nil {
				return err
			}
		}
		if err := tab.call(ctx, "top", nil, nil); err != nil {
			return err
		}
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("строка %s не найдена в DOM после прокрутки", id)
}

// normalizeText сравнивает тексты без учёта различий в пробелах и переносах,
// которые Lokalise может нормализовать при сохранении.
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\u00a0", " ")
	return strings.Join(strings.Fields(s), " ")
}

// saveDebugSnapshot сохраняет HTML страницы в data/logs для разбора сбоев.
func saveDebugSnapshot(ctx context.Context, tab *browserTab, prefix string) string {
	var html string
	if err := tab.call(ctx, "html", nil, &html); err != nil {
		return ""
	}
	dir := filepath.Join("data", "logs")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.html", prefix, time.Now().Format("20060102_150405")))
	if err := os.WriteFile(path, []byte(html), 0o600); err != nil {
		return ""
	}
	slog.Info("🧾 Снимок страницы сохранён", "path", path)
	return path
}
