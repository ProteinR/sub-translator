package main

import (
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"gopkg.in/telebot.v4"
)

// Табло прогона в Telegram: одно сообщение со списком проектов, которое
// редактируется по ходу работы (⬜ → 🔄 → ✅/❌), вместо отдельного сообщения
// на каждый проект.

const (
	boardEvery    = 3 * time.Second // не чаще: Telegram ограничивает правки в группах
	boardMaxChars = 3900            // лимит сообщения 4096 после разбора разметки
)

type boardPhase int

const (
	phaseRunning boardPhase = iota
	phaseDone
	phaseStopped
	phaseBudget
)

type boardView struct {
	Phase    boardPhase
	Projects []projectStatus // в порядке очереди
	Model    string
	CostUSD  float64
	Usage    usageSnapshot
	Started  time.Time
	Now      time.Time
}

// renderBoard собирает текст табло (parse_mode HTML). Всё, что пришло извне —
// названия файлов, ссылки, тексты ошибок, — экранируется: один «<» и Telegram
// отвергнет всё сообщение.
func renderBoard(v boardView) string {
	done, failed := 0, 0
	for _, p := range v.Projects {
		switch p.Status {
		case statusDone:
			done++
		case statusError:
			failed++
		}
	}
	total := len(v.Projects)
	progress := fmt.Sprintf("%d из %d", done, total)
	if failed > 0 {
		progress += fmt.Sprintf(" · ошибок: %d", failed)
	}

	var sb strings.Builder
	switch v.Phase {
	case phaseRunning:
		fmt.Fprintf(&sb, "⏳ <b>Перевод идёт</b> · %s\n", progress)
	case phaseDone:
		icon := "🏁"
		if failed > 0 {
			icon = "⚠️"
		}
		fmt.Fprintf(&sb, "%s <b>Перевод завершён</b> · %s\n", icon, progress)
	case phaseStopped:
		fmt.Fprintf(&sb, "⏸ <b>Перевод остановлен</b> · %s\n", progress)
	case phaseBudget:
		fmt.Fprintf(&sb, "🛑 <b>Лимит расходов исчерпан</b> · %s\n", progress)
	}
	period := v.Started.Format("15:04")
	if v.Phase != phaseRunning {
		period += "–" + v.Now.Format("15:04")
	}
	fmt.Fprintf(&sb, "💳 <b>$%.2f</b> · 🕐 %s\n", v.CostUSD, period)
	sb.WriteByte('\n')

	tech := boardTech(v)
	lines := make([]string, 0, len(v.Projects))
	for _, p := range v.Projects {
		lines = append(lines, boardLine(p, v.Phase))
	}
	body := strings.Join(lines, "\n")
	if sb.Len()+len(body)+len(tech) > boardMaxChars {
		// Длинная очередь: готовые схлопываются в одну строку.
		lines = lines[:0]
		for _, p := range v.Projects {
			if p.Status != statusDone {
				lines = append(lines, boardLine(p, v.Phase))
			}
		}
		lines = append(lines, fmt.Sprintf("✅ ещё готово: %d", done))
		body = strings.Join(lines, "\n")
		if sb.Len()+len(body)+len(tech) > boardMaxChars {
			body = truncate(body, boardMaxChars-sb.Len()-len(tech))
			body = body[:strings.LastIndex(body, "\n")+1] + "…"
		}
	}
	sb.WriteString(body)
	sb.WriteString(tech)
	return sb.String()
}

// boardTech — техническая строка в самом низу, после пустых строк: модель и
// токены под спойлером (раскрываются нажатием). Сверху — то, что нужно сразу.
func boardTech(v boardView) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n\n\n🔤 <tg-spoiler>%s", html.EscapeString(v.Model))
	if u := v.Usage; u.Requests > 0 || u.MemoryHits > 0 {
		fmt.Fprintf(&sb, " · %s вход · %s выход", fmtTokens(u.InputTokens+u.CacheRead+u.CacheWrite), fmtTokens(u.OutputTokens))
		if u.CacheRead > 0 {
			fmt.Fprintf(&sb, " · %s из кэша", fmtTokens(u.CacheRead))
		}
		if u.MemoryHits > 0 {
			fmt.Fprintf(&sb, " · 🧠 %d из памяти", u.MemoryHits)
		}
	}
	sb.WriteString("</tg-spoiler>")
	return sb.String()
}

func boardLine(p projectStatus, phase boardPhase) string {
	name := fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(p.URL), html.EscapeString(truncate(boardName(p), 60)))
	switch p.Status {
	case statusRunning:
		if p.Total > 0 {
			return fmt.Sprintf("🔄 %s · %d/%d", name, p.Done, p.Total)
		}
		return "🔄 " + name
	case statusDone:
		if p.Total > 0 {
			return fmt.Sprintf("✅ %s · %d %s", name, p.Total, plural(p.Total, "ячейка", "ячейки", "ячеек"))
		}
		return "✅ " + name + " · нечего переводить"
	case statusError:
		return "❌ " + name + "\n      └ " + html.EscapeString(truncate(p.Detail, 160))
	}
	if phase != phaseRunning {
		return "⏸ " + name // не дошли: остался в очереди
	}
	return "⬜ " + name
}

// boardName — имя файла, если проект уже открыт, иначе номер проекта из ссылки.
func boardName(p projectStatus) string {
	if p.Name != "" {
		return p.Name
	}
	if u, err := url.Parse(p.URL); err == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i, s := range parts {
			if s == "project" && i+1 < len(parts) {
				return "проект " + parts[i+1]
			}
		}
	}
	return p.URL
}

// fmtTokens: 950 → «950», 12 345 → «12.3k», 2 100 000 → «2.10M».
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func plural(n int, one, few, many string) string {
	m10, m100 := n%10, n%100
	switch {
	case m10 == 1 && m100 != 11:
		return one
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		return few
	}
	return many
}

// runBoard держит сообщение табло и правит его не чаще boardEvery.
// nil-безопасен: без Telegram все методы ничего не делают.
type runBoard struct {
	n      *notifier
	msg    *telebot.Message
	view   func(boardPhase) boardView
	dirty  chan struct{}
	stop   chan struct{}
	doneCh chan struct{}

	mu   sync.Mutex
	last string
}

func startRunBoard(n *notifier, view func(boardPhase) boardView) *runBoard {
	if n == nil {
		return nil
	}
	text := renderBoard(view(phaseRunning))
	msg, err := n.bot.Send(n.chat, text, &telebot.SendOptions{ParseMode: telebot.ModeHTML, DisableWebPagePreview: true})
	if err != nil {
		slog.Warn("⚠️ Не удалось отправить табло в Telegram", "error", secrets.Redact(err.Error()))
		return nil
	}
	b := &runBoard{n: n, msg: msg, view: view, last: text,
		dirty: make(chan struct{}, 1), stop: make(chan struct{}), doneCh: make(chan struct{})}
	go b.loop()
	return b
}

// Changed отмечает, что состояние проектов изменилось.
func (b *runBoard) Changed() {
	if b == nil {
		return
	}
	select {
	case b.dirty <- struct{}{}:
	default:
	}
}

func (b *runBoard) loop() {
	defer close(b.doneCh)
	for {
		select {
		case <-b.stop:
			return
		case <-b.dirty:
			b.edit(phaseRunning)
			select { // пауза между правками
			case <-b.stop:
				return
			case <-time.After(boardEvery):
			}
		}
	}
}

// Finish останавливает обновления и записывает итог.
func (b *runBoard) Finish(phase boardPhase) {
	if b == nil {
		return
	}
	close(b.stop)
	<-b.doneCh
	for attempt := 0; attempt < 3; attempt++ {
		var flood telebot.FloodError
		err := b.edit(phase)
		if !errors.As(err, &flood) {
			return
		}
		time.Sleep(time.Duration(min(flood.RetryAfter, 10)) * time.Second)
	}
}

func (b *runBoard) edit(phase boardPhase) error {
	text := renderBoard(b.view(phase))
	b.mu.Lock()
	defer b.mu.Unlock()
	if text == b.last {
		return nil
	}
	_, err := b.n.bot.Edit(b.msg, text, &telebot.SendOptions{ParseMode: telebot.ModeHTML, DisableWebPagePreview: true})
	if err != nil && !errors.Is(err, telebot.ErrMessageNotModified) && !errors.Is(err, telebot.ErrSameMessageContent) {
		slog.Debug("табло не обновилось", "error", secrets.Redact(err.Error()))
		return err
	}
	b.last = text
	return nil
}
