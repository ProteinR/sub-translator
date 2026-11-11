package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakePage — страница Lokalise в памяти: виртуальный список строк с ячейками.
// lose[key] — сколько раз «сохранение» этой ячейки молча потеряется.
type fakePage struct {
	rows  []scannedRow
	pos   int
	lose  map[string]int
	fills int
}

const fakeWindow = 3 // сколько строк «видно» за раз

func (f *fakePage) eval(_ context.Context, _ int, body, argsJSON string) (string, error) {
	i := strings.LastIndex(body, `window.__loka["`)
	op := body[i+len(`window.__loka["`):]
	op = op[:strings.Index(op, `"`)]
	switch op {
	case "top":
		f.pos = 0
		return "true", nil
	case "scan":
		end := min(f.pos+fakeWindow, len(f.rows))
		data, _ := json.Marshal(f.rows[f.pos:end])
		return string(data), nil
	case "scroll":
		old := f.pos
		f.pos = min(f.pos+2, max(len(f.rows)-fakeWindow, 0))
		return fmt.Sprintf(`{"top":%d,"moved":%v,"max":%d}`, f.pos, f.pos != old, len(f.rows)), nil
	case "has":
		return "true", nil
	case "fill":
		var a struct {
			ID, LangID, Text string
			Overwrite        bool
			Attempt          int
		}
		_ = json.Unmarshal([]byte(argsJSON), &a)
		f.fills++
		for ri := range f.rows {
			if f.rows[ri].ID != a.ID {
				continue
			}
			for ti := range f.rows[ri].Targets {
				t := &f.rows[ri].Targets[ti]
				if t.LangID != a.LangID {
					continue
				}
				if a.Attempt == 0 && !a.Overwrite && t.Value != "" {
					return `{"status":"skipped"}`, nil
				}
				if k := a.ID + "/" + a.LangID; f.lose[k] > 0 {
					f.lose[k]--
					return `{"status":"ok","kind":"cm"}`, nil // «сохранилось», но значения нет
				}
				t.Value = a.Text
				return `{"status":"ok","kind":"cm"}`, nil
			}
		}
		return `{"status":"missing"}`, nil
	case "escape", "html":
		return `""`, nil
	case "pageErrors":
		return "[]", nil
	}
	return "", fmt.Errorf("unknown op %s", op)
}

func (f *fakePage) newTab(int, string)   {}
func (f *fakePage) navigate(int, string) {}
func (f *fakePage) setTitle(int, string) {}
func (f *fakePage) selectTab(int)        {}
func (f *fakePage) closeTab(int)         {}
func (f *fakePage) activate()            {}
func (f *fakePage) setBusy(bool)         {}

func fakeProject(n int) (*fakePage, []TranslationItem) {
	page := &fakePage{lose: map[string]int{}}
	var items []TranslationItem
	for i := 0; i < n; i++ {
		id := fmt.Sprint(100 + i)
		page.rows = append(page.rows, scannedRow{ID: id, BaseCount: 1, SourceLang: "English", SourceID: "1", Source: "Hello " + id,
			Targets: []scannedTarget{{LangID: "2", Lang: "German"}, {LangID: "3", Lang: "French"}}})
		items = append(items,
			TranslationItem{ID: id, TargetID: "2", TargetLang: "German", Original: "Hello " + id, Translation: "Hallo " + id},
			TranslationItem{ID: id, TargetID: "3", TargetLang: "French", Original: "Hello " + id, Translation: "Bonjour " + id})
	}
	return page, items
}

func TestVerifyRefillsLostCells(t *testing.T) {
	page, items := fakeProject(8)
	page.lose["103/2"] = 1 // первая вставка потеряется, повтор после проверки — нет
	page.lose["106/3"] = 2
	tab := &browserTab{id: 1, eng: page}
	ctx := context.Background()

	if _, err := fillTranslations(ctx, tab, items, Config{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifyAndRefill(ctx, tab, Config{}, items); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if page.fills != len(items)+3 {
		t.Errorf("fills = %d, want %d (3 re-fills)", page.fills, len(items)+3)
	}
	bad, _ := verifyCells(ctx, tab, Config{}, items)
	if len(bad) != 0 {
		t.Errorf("still bad: %v", bad)
	}
}

func TestVerifyGivesUpAfterRounds(t *testing.T) {
	page, items := fakeProject(5)
	page.lose["102/3"] = 100 // не сохраняется никогда
	tab := &browserTab{id: 1, eng: page}
	ctx := context.Background()

	if _, err := fillTranslations(ctx, tab, items, Config{}, nil); err != nil {
		t.Fatal(err)
	}
	err := verifyAndRefill(ctx, tab, Config{}, items)
	if err == nil || !strings.Contains(err.Error(), "не удалось вставить 1 ячеек") || !strings.Contains(err.Error(), "102/French") {
		t.Fatalf("err = %v", err)
	}
}

func TestVerifySkipsPrefilledCells(t *testing.T) {
	page, items := fakeProject(3)
	page.rows[1].Targets[0].Value = "Человек уже перевёл"
	tab := &browserTab{id: 1, eng: page}
	ctx := context.Background()

	out, err := fillTranslations(ctx, tab, items, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.skipped[items[2].key()] {
		t.Fatalf("prefilled cell not reported as skipped: %v", out.skipped)
	}
	var expected []TranslationItem
	for _, it := range items {
		if !out.skipped[it.key()] {
			expected = append(expected, it)
		}
	}
	if err := verifyAndRefill(ctx, tab, Config{}, expected); err != nil {
		t.Fatal(err)
	}
	if page.rows[1].Targets[0].Value != "Человек уже перевёл" {
		t.Error("prefilled cell overwritten")
	}
}
