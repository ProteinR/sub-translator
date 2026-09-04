package main

import (
	"context"
	"os"
	"testing"

	"github.com/playwright-community/playwright-go"
)

func TestValidateTranslations(t *testing.T) {
	input := []TranslationItem{{ID: "1", Original: "First"}, {ID: "2", Original: "Second"}}
	valid := []TranslationItem{{ID: "2", Translation: "Drugi"}, {ID: "1", Translation: "Pierwszy"}}
	got, err := validateTranslations(input, valid)
	if err != nil || got[0].ID != "1" || got[0].Original != "First" {
		t.Fatalf("ordering/source lost: %v %v", got, err)
	}
	for _, bad := range [][]TranslationItem{nil, valid[:1], {{ID: "3", Translation: "x"}}, {{ID: "1", Translation: "x"}, {ID: "1", Translation: "y"}}, {{ID: "1", Translation: " "}, {ID: "2", Translation: "x"}}} {
		if _, err := validateTranslations(input, bad); err == nil {
			t.Errorf("accepted incomplete/invalid response: %v", bad)
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
