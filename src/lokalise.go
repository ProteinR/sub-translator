package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	rowAttempts  = 2 // попытки вставки одной строки
	verifyRounds = 2 // повторные вставки после проверки всех ячеек
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
				logOf(ctx).Info("✅ Вход выполнен")
			}
			return st, nil
		case onLogin && !askedLogin:
			askedLogin = true
			deadline = time.Now().Add(loginTimeout)
			logOf(ctx).Warn("⚠️ Требуется вход. Войдите в Lokalise во вкладке окна приложения.")
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
		logOf(ctx).Info("ℹ️ Пустых строк не найдено", "url", projectURL)
		rep.detail("Пустых ячеек нет — переводить нечего")
		return filename, nil
	}
	rep.update(func(s *projectStatus) {
		s.Total = len(collected)
		s.Detail = "Перевожу и вставляю · " + targetsOf(collected)
	})
	onCell := func() { rep.update(func(s *projectStatus) { s.Done++ }) }

	jobs := buildJobs(ctx, collected)

	// Конвейер: пока браузер вставляет пачку N, модель уже переводит пачку N+1.
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := runTranslationPipeline(pctx, tr, jobs)

	// expected — всё, что мы вставляли (кроме ячеек, оказавшихся уже заполненными):
	// после вставки каждая из них сверяется со страницей.
	var expected []TranslationItem
	for res := range results {
		if res.err != nil {
			return filename, fmt.Errorf("translation error: %w", res.err)
		}
		out, err := fillTranslations(ctx, tab, res.items, config, onCell)
		if err != nil {
			return filename, err
		}
		for _, it := range res.items {
			if !out.skipped[it.key()] {
				expected = append(expected, it)
			}
		}
	}
	rep.detail("Проверяю, что все переводы на месте…")
	return filename, verifyAndRefill(ctx, tab, config, expected)
}

// verifyAndRefill проходит по всем строкам и сверяет ячейки с переводами модели.
// Пустые или несовпавшие ячейки вставляются заново (до verifyRounds раз); если и
// после этого что-то не совпало — ошибка со списком ячеек.
func verifyAndRefill(ctx context.Context, tab *browserTab, config Config, expected []TranslationItem) error {
	if len(expected) == 0 {
		return nil
	}
	refill := config
	refill.OverwriteFilled = true // ячейка уже наша: пустая или с неполным текстом
	for round := 0; ; round++ {
		bad, err := verifyCells(ctx, tab, config, expected)
		if err != nil {
			return fmt.Errorf("проверка после вставки: %w", err)
		}
		if len(bad) == 0 {
			logOf(ctx).Info("🔎 Проверка пройдена: все ячейки совпадают с переводом", "cells", len(expected), "repeats", round)
			return nil
		}
		keys := make([]string, len(bad))
		for i, it := range bad {
			keys[i] = it.ID + "/" + it.TargetLang
		}
		if round == verifyRounds {
			saveDebugSnapshot(ctx, tab, "error_verify")
			return fmt.Errorf("не удалось вставить %d ячеек (проверка после %d повторов): %s", len(bad), verifyRounds, strings.Join(firstN(keys, 5), ", "))
		}
		logOf(ctx).Warn("🔎 Проверка: ячейки пустые или не совпадают с переводом — вставляю заново", "cells", len(bad), "repeat", round+1, "of", verifyRounds, "first", strings.Join(firstN(keys, 5), ", "))
		if _, err := fillTranslations(ctx, tab, bad, refill, nil); err != nil {
			return err
		}
	}
}

// verifyCells возвращает ячейки, значение которых на странице не совпадает с
// переводом (пустые, обрезанные, чужой текст) или которые не нашлись.
func verifyCells(ctx context.Context, tab *browserTab, config Config, items []TranslationItem) ([]TranslationItem, error) {
	want := map[string]map[string]TranslationItem{} // id → langId → ячейка
	for _, it := range items {
		if want[it.ID] == nil {
			want[it.ID] = map[string]TranslationItem{}
		}
		want[it.ID][it.TargetID] = it
	}
	var bad []TranslationItem
	_, err := walkRows(ctx, tab, config, func(row scannedRow) (bool, error) {
		cells, ok := want[row.ID]
		if !ok {
			return false, nil
		}
		for _, t := range row.Targets {
			it, ok := cells[t.LangID]
			if !ok {
				continue
			}
			if normalizeText(t.Value) != normalizeText(it.Translation) {
				reason := "другой текст"
				if strings.TrimSpace(t.Value) == "" {
					reason = "пусто"
				}
				logOf(ctx).Debug("проверка: ячейка не совпала", "id", it.ID, "lang", it.TargetLang, "reason", reason,
					"page", truncate(t.Value, 60), "want", truncate(it.Translation, 60))
				bad = append(bad, it)
			}
			delete(cells, t.LangID)
		}
		if len(cells) == 0 {
			delete(want, row.ID)
		}
		return len(want) == 0, nil
	})
	if err != nil {
		return nil, err
	}
	for id, cells := range want { // строка или язык не попались при прокрутке
		for _, it := range cells {
			logOf(ctx).Debug("проверка: ячейка не найдена на странице", "id", id, "lang", it.TargetLang)
			bad = append(bad, it)
		}
	}
	return bad, nil
}

// buildJobs группирует ячейки по документу и исходному языку (все целевые языки
// строки — в одном запросе), сохраняя порядок строк, и режет на пачки.
func buildJobs(ctx context.Context, items []TranslationItem) []translationJob {
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
		logOf(ctx).Info("🌍 Определено направление", "source", group[0].SourceLang, "targets", targetsOf(group), "file", group[0].Filename, "cells", len(group))
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

// describeTargets — для журнала: что с каждой целевой ячейкой строки
// («de:пусто», «fr:заполнено»), чтобы было видно, почему её не взяли в перевод.
func describeTargets(row scannedRow, overwrite bool) string {
	if strings.TrimSpace(row.Source) == "" {
		return "нет исходного текста — пропуск"
	}
	parts := make([]string, 0, len(row.Targets))
	for _, t := range row.Targets {
		state := "пусто"
		if strings.TrimSpace(t.Value) != "" {
			state = "заполнено"
			if overwrite {
				state += ",перезапишу"
			}
		}
		parts = append(parts, t.Lang+":"+state)
	}
	return strings.Join(parts, " ")
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
	emptySource := 0

	modeLog := "пустых"
	if config.OverwriteFilled {
		modeLog = "всех"
	}
	logOf(ctx).Info(fmt.Sprintf("🔍 Начинаю поиск %s строк", modeLog), "file", filename)

	seen, err := walkRows(ctx, tab, config, func(row scannedRow) (bool, error) {
		items, err := itemsFromRow(row, config.OverwriteFilled)
		if err != nil {
			logOf(ctx).Debug("строка не прошла проверку", "id", row.ID, "base", row.BaseCount, "source_lang", row.SourceLang, "targets", len(row.Targets))
			return false, err
		}
		if strings.TrimSpace(row.Source) == "" {
			emptySource++
		}
		logOf(ctx).Debug("строка", "id", row.ID, "file", row.File, "source_lang", row.SourceLang,
			"source", truncate(row.Source, 60), "cells", describeTargets(row, config.OverwriteFilled))
		results = append(results, items...)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	if seen == 0 {
		return nil, errors.New("не найдено строк для обработки")
	}
	logOf(ctx).Info("✅ Сбор данных завершен", "file", filename, "checked", seen, "collected", len(results))
	if emptySource > 0 {
		logOf(ctx).Info("ℹ️ Строки без исходного текста пропущены", "rows", emptySource)
	}
	return results, nil
}

// walkRows листает виртуализированный список сверху вниз и отдаёт fn каждую
// строку один раз. fn возвращает true, чтобы закончить раньше. В конце список
// возвращается наверх. Возвращает число просмотренных строк.
func walkRows(ctx context.Context, tab *browserTab, config Config, fn func(scannedRow) (bool, error)) (int, error) {
	seen := make(map[string]bool)
	noNewElementsCount := 0
	const maxNoNewRetries = 5

	if err := tab.call(ctx, "top", nil, nil); err != nil {
		return 0, err
	}
	for noNewElementsCount < maxNoNewRetries {
		if err := ctx.Err(); err != nil {
			return len(seen), err
		}
		var rows []scannedRow
		if err := tab.call(ctx, "scan", nil, &rows); err != nil {
			return len(seen), err
		}
		newAddedThisStep := 0
		for _, row := range rows {
			if row.ID == "" || seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			newAddedThisStep++
			stop, err := fn(row)
			if err != nil {
				return len(seen), err
			}
			if stop {
				return len(seen), tab.call(ctx, "top", nil, nil)
			}
		}

		var sr scrollResult
		if err := tab.call(ctx, "scroll", map[string]int{"dy": 800}, &sr); err != nil {
			return len(seen), err
		}
		logOf(ctx).Debug("шаг прокрутки", "visible", len(rows), "new", newAddedThisStep, "top", int(sr.Top), "max", int(sr.Max), "moved", sr.Moved, "idle", noNewElementsCount)
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
			return len(seen), err
		}
	}
	return len(seen), tab.call(ctx, "top", nil, nil)
}

type fillOutcome struct {
	failed  []string        // id/lang ячеек, которые не вставились
	skipped map[string]bool // key() ячеек, оказавшихся уже заполненными
}

// fillTranslations вставляет пачку. Упавшие ячейки не валят весь проект: их
// подберёт проверка после вставки (verifyAndRefill). Ошибка возвращается только
// при отмене или если страница явно сломалась.
func fillTranslations(ctx context.Context, tab *browserTab, items []TranslationItem, config Config, onCell func()) (fillOutcome, error) {
	logOf(ctx).Info("✍️ Вставка переводов...", "cells", len(items), "langs", targetsOf(items))

	out := fillOutcome{skipped: map[string]bool{}}
	var failed []string
	consecutive := 0
	for _, item := range items {
		var err error
		for attempt := 0; attempt < rowAttempts; attempt++ {
			if err = ctx.Err(); err != nil {
				out.failed = failed
				return out, err
			}
			var status string
			if status, err = fillRow(ctx, tab, item, config, attempt); err == nil {
				logOf(ctx).Debug("ячейка", "id", item.ID, "lang", item.TargetLang, "status", status, "attempt", attempt+1, "text", truncate(item.Translation, 60))
				if strings.HasPrefix(status, "skipped") {
					out.skipped[item.key()] = true
				}
				break
			}
			if errors.Is(err, context.Canceled) {
				out.failed = failed
				return out, err
			}
			logOf(ctx).Warn("⚠️ Строка не вставилась", "id", item.ID, "lang", item.TargetLang, "attempt", attempt+1, "error", err)
			_ = tab.call(ctx, "escape", nil, nil) // закрываем возможный зависший редактор
		}
		if onCell != nil {
			onCell()
		}
		if err != nil {
			logPageErrors(ctx, tab)
			saveDebugSnapshot(ctx, tab, "error_fill")
			failed = append(failed, item.ID+"/"+item.TargetLang)
			consecutive++
			if consecutive >= maxConsecutiveRowFailures {
				out.failed = failed
				return out, fmt.Errorf("%d строк подряд не вставились, похоже страница сломалась; последняя ошибка: %w", consecutive, err)
			}
			continue
		}
		consecutive = 0
	}
	out.failed = failed
	return out, nil
}

type fillResult struct {
	Status string `json:"status"`
	Kind   string `json:"kind"`
}

// fillRow возвращает статус из lokalise.js: ok (вставлено), skipped (ячейка уже
// заполнена — не трогаем).
func fillRow(ctx context.Context, tab *browserTab, item TranslationItem, config Config, attempt int) (string, error) {
	if err := findRow(ctx, tab, item.ID); err != nil {
		return "", err
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
		return "", err
	}
	if res.Status == "missing" {
		return "", fmt.Errorf("строка %s пропала из DOM", item.ID)
	}
	status := res.Status
	if res.Kind != "" {
		status += "/" + res.Kind
	}
	return status, sleepCtx(ctx, config.RowNextDelay)
}

// logPageErrors пишет в журнал ошибки JavaScript самой страницы Lokalise,
// накопленные с прошлого вызова: часто они и объясняют, почему не сработал редактор.
func logPageErrors(ctx context.Context, tab *browserTab) {
	var errs []string
	if err := tab.call(ctx, "pageErrors", nil, &errs); err != nil || len(errs) == 0 {
		return
	}
	for _, e := range errs {
		logOf(ctx).Debug("ошибка на странице Lokalise", "error", truncate(e, 500))
	}
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
	logOf(ctx).Debug("строка не найдена после двух проходов прокрутки", "id", id)
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
	dir := logsDir
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.html", prefix, time.Now().Format("20060102_150405")))
	if err := os.WriteFile(path, []byte(html), 0o600); err != nil {
		return ""
	}
	logOf(ctx).Info("🧾 Снимок страницы сохранён", "path", path)
	return path
}
