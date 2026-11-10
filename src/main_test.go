package main

import (
	"slices"
	"testing"
)

func TestCheckTranslation(t *testing.T) {
	ok := []struct{ src, tr string }{
		{"Hello, {name}! Click <b>here</b>.", "Cześć, {name}! Kliknij <b>tutaj</b>."},
		{"Discount 50% for %s", "Zniżka 50% dla %s"},
		{"Line one\n\nLine two", "Linia jeden\n\nLinia druga"},
		{"OK", "OK"}, // короткие строки могут совпадать
	}
	for _, c := range ok {
		if err := checkTranslation(c.src, c.tr); err != nil {
			t.Errorf("%q → %q rejected: %v", c.src, c.tr, err)
		}
	}
	bad := []struct{ src, tr string }{
		{"Hello, {name}", "Cześć"},
		{"Click <b>here</b>", "Kliknij tutaj"},
		{"Para one\n\nPara two", "Akapit jeden akapit dwa"},
		{"This is a fairly long sentence to translate.", "Tak."},
		{"This sentence stays in English and was not translated at all.", "This sentence stays in English and was not translated at all."},
		{"Hi", "  "},
	}
	for _, c := range bad {
		if err := checkTranslation(c.src, c.tr); err == nil {
			t.Errorf("%q → %q accepted", c.src, c.tr)
		}
	}
}

func TestItemsFromRow(t *testing.T) {
	row := scannedRow{ID: "1", File: "a.srt", BaseCount: 1, SourceLang: "English", SourceID: "640", Source: "Hello",
		Targets: []scannedTarget{{LangID: "748", Lang: "Polish"}, {LangID: "597", Lang: "Russian", Value: "Привет"}, {LangID: "769", Lang: "Ukrainian", Value: " "}}}
	items, err := itemsFromRow(row, false)
	if err != nil || len(items) != 2 {
		t.Fatalf("empty cells: got %d, %v", len(items), err)
	}
	if items[0].TargetLang != "Polish" || items[0].TargetID != "748" || items[0].SourceLang != "English" || items[0].Filename != "a.srt" {
		t.Fatalf("wrong metadata: %+v", items[0])
	}
	if items, _ := itemsFromRow(row, true); len(items) != 3 {
		t.Fatalf("overwrite: got %d", len(items))
	}
	empty := row
	empty.Source = "  "
	if items, _ := itemsFromRow(empty, true); len(items) != 0 {
		t.Fatal("empty source must be skipped")
	}

	bad := []func(r *scannedRow){
		func(r *scannedRow) { r.BaseCount = 0 },
		func(r *scannedRow) { r.SourceLang = "" },
		func(r *scannedRow) { r.Targets = nil },
		func(r *scannedRow) { r.Targets = []scannedTarget{{LangID: "x", Lang: "Polish"}} },
		func(r *scannedRow) { r.Targets = []scannedTarget{{LangID: "640", Lang: "English"}} },
		func(r *scannedRow) {
			r.Targets = []scannedTarget{{LangID: "1", Lang: "Polish"}, {LangID: "2", Lang: "Polish"}}
		},
		func(r *scannedRow) {
			r.Targets = []scannedTarget{{LangID: "1", Lang: "Polish"}, {LangID: "1", Lang: "Polish 2"}}
		},
	}
	for i, mod := range bad {
		r := row
		r.Targets = slices.Clone(row.Targets)
		mod(&r)
		if _, err := itemsFromRow(r, false); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
