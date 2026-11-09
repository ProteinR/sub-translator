package main

import (
	"context"
	"os"
	"testing"

	"github.com/playwright-community/playwright-go"
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

// Opt-in browser regression test; uses a local saved page, never a live project.
func TestMultilingualHTML(t *testing.T) {
	fixture := os.Getenv("LOKALISE_HTML_FIXTURE")
	if fixture == "" {
		t.Skip("set LOKALISE_HTML_FIXTURE to saved Multilingual HTML")
	}
	html, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := playwright.Run()
	if err != nil {
		t.Fatal(err)
	}
	defer pw.Stop()
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	page, err := browser.NewPage(playwright.BrowserNewPageOptions{JavaScriptEnabled: playwright.Bool(false)})
	if err != nil {
		t.Fatal(err)
	}
	// Scripts in the attached HTML are data, not test instructions.
	err = page.Route("**/*", func(route playwright.Route) { _ = route.Abort() })
	if err != nil {
		t.Fatal(err)
	}
	if err := page.SetContent(string(html)); err != nil {
		t.Fatal(err)
	}
	count, _ := page.Locator(".row-key[data-id]").Count()
	if count == 0 {
		t.Fatal("fixture has no rows")
	}

	cfg := Config{}
	items, err := scrollAndCollect(context.Background(), page, cfg, "fixture")
	if err != nil || len(items) != count*3 {
		t.Fatalf("got %d of %d: %v", len(items), count*3, err)
	}
	directions := map[string]string{}
	for _, item := range items {
		if item.SourceLang != "English" || item.Original == "" || item.Filename == "" {
			t.Fatalf("missing metadata: %+v", item)
		}
		directions[item.TargetID] = item.TargetLang
	}
	if directions["748"] != "Polish" || directions["597"] != "Russian" || directions["769"] != "Ukrainian" {
		t.Fatalf("wrong directions: %v", directions)
	}
	cell := page.Locator(".row-key[data-id]").First().Locator(targetCellSelector("748"))
	if _, err := cell.Locator("[data-lokalise-editor-value]").Evaluate("el => el.setAttribute('data-lokalise-editor-value', 'Existing')", nil); err != nil {
		t.Fatal(err)
	}
	items, err = scrollAndCollect(context.Background(), page, cfg, "fixture")
	if err != nil || len(items) != count*3-1 {
		t.Fatalf("filled cell not skipped: %v", err)
	}
	cfg.OverwriteFilled = true
	items, err = scrollAndCollect(context.Background(), page, cfg, "fixture")
	if err != nil || len(items) != count*3 {
		t.Fatalf("overwrite failed: %v", err)
	}
	// A different source and a previously unknown target must work without any configuration.
	_, err = page.Evaluate(`() => {
  document.querySelectorAll(".key-translation[data-is-base='1']").forEach(el => {el.setAttribute('data-lang', 'German'); el.setAttribute('data-lang-id', '999');});
  document.querySelectorAll(".key-translation[data-lang-id='748']").forEach(el => {el.setAttribute('data-lang', 'Spanish'); el.setAttribute('data-lang-id', '888');});
 }`)
	if err != nil {
		t.Fatal(err)
	}
	items, err = scrollAndCollect(context.Background(), page, cfg, "fixture")
	if err != nil || len(items) != count*3 {
		t.Fatalf("dynamic languages failed: %v", err)
	}
	if items[0].SourceLang != "German" || items[0].TargetLang != "Spanish" || items[0].TargetID != "888" {
		t.Fatalf("hardcoded direction: %+v", items[0])
	}
}
