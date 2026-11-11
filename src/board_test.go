package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRenderBoard(t *testing.T) {
	start := time.Date(2026, 11, 11, 12, 30, 0, 0, time.Local)
	v := boardView{
		Phase:   phaseRunning,
		Model:   "claude-haiku-5-5",
		CostUSD: 0.123,
		Started: start,
		Now:     start.Add(15 * time.Minute),
		Projects: []projectStatus{
			{URL: "https://app.lokalise.com/project/111.abc/?view=multi", Status: statusPending},
			{URL: "https://x/project/2/?a=1&b=2", Name: "ep<2>.srt", Status: statusRunning, Done: 12, Total: 40},
			{URL: "https://x/project/3/", Name: "ep3.srt", Status: statusDone, Total: 21},
			{URL: "https://x/project/4/", Name: "ep4.srt", Status: statusError, Detail: "bad <tag> & co"},
		},
	}
	got := renderBoard(v)
	for _, want := range []string{
		"⏳ <b>Перевод идёт</b> · 1 из 4 · ошибок: 1",
		"💳 $0.12 · 🕐 12:30\n",
		"⬜ <a href=\"https://app.lokalise.com/project/111.abc/?view=multi\">проект 111.abc</a>",
		"🔄 <a href=\"https://x/project/2/?a=1&amp;b=2\">ep&lt;2&gt;.srt</a> · 12/40",
		"✅ <a href=\"https://x/project/3/\">ep3.srt</a> · 21 ячейка",
		"❌ <a href=\"https://x/project/4/\">ep4.srt</a>\n      └ bad &lt;tag&gt; &amp; co",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	v.Phase = phaseStopped
	got = renderBoard(v)
	if !strings.Contains(got, "⏸ <b>Перевод остановлен</b>") || !strings.Contains(got, "🕐 12:30–12:45") || !strings.Contains(got, "⏸ <a href=\"https://app.lokalise.com") {
		t.Errorf("stopped board:\n%s", got)
	}
}

func TestRenderBoardLongQueue(t *testing.T) {
	v := boardView{Phase: phaseDone, Model: "m", Started: time.Now(), Now: time.Now()}
	for i := 0; i < 200; i++ {
		v.Projects = append(v.Projects, projectStatus{URL: fmt.Sprintf("https://app.lokalise.com/project/%d/", i), Name: strings.Repeat("x", 40), Status: statusDone, Total: 5})
	}
	v.Projects[7].Status = statusError
	v.Projects[7].Detail = "oops"
	got := renderBoard(v)
	if len(got) > boardMaxChars+10 {
		t.Fatalf("too long: %d", len(got))
	}
	if !strings.Contains(got, "✅ ещё готово: 199") || !strings.Contains(got, "❌") {
		t.Errorf("long board:\n%s", got)
	}
}

func TestBoardNilSafe(t *testing.T) {
	var b *runBoard
	b.Changed()
	b.Finish(phaseDone)
}

func TestRenderBoardTokens(t *testing.T) {
	v := boardView{Phase: phaseRunning, Model: "m", Started: time.Now(), Now: time.Now(),
		Usage: usageSnapshot{Requests: 3, InputTokens: 2000, CacheRead: 10500, OutputTokens: 950, MemoryHits: 4}}
	got := renderBoard(v)
	if want := "🔤 Токены: 12.5k вход · 950 выход · 10.5k из кэша · 🧠 4 из памяти\n"; !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
}
