package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConfigFixesUnknownModelAndDedupesProjects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"model":"gpt-4o","projects":[" a ","a","","b"],"maxConcurrency":0}`), 0o600)

	cfg, err := newConfigStore(path).Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != defaultModel || cfg.MaxConcurrency != 1 || strings.Join(cfg.Projects, ",") != "a,b" {
		t.Fatalf("not normalized: %+v", cfg)
	}
}

func TestConfigCorruptFileIsReportedAndBackedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"anthropicApiKey":"sk-ant-`), 0o600) // обрыв записи

	store := newConfigStore(path)
	if _, err := store.Load(); err == nil {
		t.Fatal("corrupt config must return an error, not silent defaults")
	}
	if err := store.Save(defaultConfig()); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(path + ".broken-*")
	if len(backups) != 1 {
		t.Fatalf("expected backup of corrupt file, got %v", backups)
	}
}

func TestConfigConcurrentRemoveAndSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := newConfigStore(path)
	cfg := defaultConfig()
	for i := 0; i < 50; i++ {
		cfg.Projects = append(cfg.Projects, "p"+string(rune('A'+i)))
	}
	store.Save(cfg)

	var wg sync.WaitGroup
	for _, p := range cfg.Projects {
		wg.Add(1)
		go func(p string) { defer wg.Done(); store.RemoveProject(p) }(p)
	}
	wg.Wait()
	got, err := store.Load()
	if err != nil || len(got.Projects) != 0 {
		t.Fatalf("lost updates: %v %v", got.Projects, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config with secrets must be 0600, got %v", fi.Mode().Perm())
	}
}

func TestGuardBlocksForeignHostAndCSRF(t *testing.T) {
	a := newApp(newConfigStore(filepath.Join(t.TempDir(), "c.json")), NewSSEBroker(), "8080")
	h := a.routes()

	cases := []struct {
		name, method, host, origin string
		header                     bool
		want                       int
	}{
		{"ui get ok", "GET", "localhost:8080", "", false, 200},
		{"dns rebinding", "GET", "evil.example:8080", "", false, 403},
		{"post without header", "POST", "localhost:8080", "", false, 403},
		{"post foreign origin", "POST", "localhost:8080", "https://evil.example", true, 403},
		{"post ok", "POST", "127.0.0.1:8080", "http://127.0.0.1:8080", true, 409}, // stop: not running
	}
	for _, c := range cases {
		path := "/api/data"
		if c.method == "POST" {
			path = "/api/stop"
		}
		req := httptest.NewRequest(c.method, path, nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.header {
			req.Header.Set("X-Loka-Request", "1")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestNormalizeText(t *testing.T) {
	if normalizeText("a  b\r\n c ") != normalizeText("a b\nc") {
		t.Fatal("whitespace normalization failed")
	}
}
