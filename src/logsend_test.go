package main

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildLogArchive(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name, body string, age time.Duration) {
		path := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(-age)
		_ = os.Chtimes(path, mod, mod)
	}
	write("2026-11-11/10-00-00.log", "key sk-ant-secret-12345 used\n", 30*time.Minute)
	write("2026-11-10/09-00-00.log", "old\n", 30*time.Hour)
	write("error_fill_1.html", `<p>row</p><script>var csrf="abc"</script>`, time.Hour)

	red := &redactor{}
	red.Set("sk-ant-secret-12345")

	data, n, err := buildLogArchive(dir, logArchiveOptions{Hours: 2, Snapshots: true}, "info", red, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("files = %d, want 2 (old log must be skipped)", n)
	}
	got := unzip(t, data)
	if !strings.Contains(got["2026-11-11/10-00-00.log"], "sk-a…2345") || strings.Contains(got["2026-11-11/10-00-00.log"], "secret") {
		t.Errorf("key not redacted: %q", got["2026-11-11/10-00-00.log"])
	}
	if s := got["error_fill_1.html"]; strings.Contains(s, "csrf") || !strings.Contains(s, "<p>row</p>") {
		t.Errorf("script not stripped: %q", s)
	}
	if got["info.txt"] != "info" {
		t.Errorf("info.txt = %q", got["info.txt"])
	}

	_, n, _ = buildLogArchive(dir, logArchiveOptions{Hours: 2}, "info", red, now)
	if n != 1 {
		t.Errorf("without snapshots files = %d, want 1", n)
	}
}

func unzip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func TestHandlerDebugGoesOnlyToFile(t *testing.T) {
	var ui, file bytes.Buffer
	log := slog.New(&CustomHandler{ui: &ui, file: &file, mu: &sync.Mutex{}}).With(tabKey, 2)
	log.Debug("строка", "id", "7")
	log.Info("готово")

	if strings.Contains(ui.String(), "строка") {
		t.Errorf("debug leaked to panel: %q", ui.String())
	}
	if !strings.Contains(ui.String(), "[2] готово") {
		t.Errorf("panel line = %q", ui.String())
	}
	if !strings.Contains(file.String(), " · [2] строка id=\"7\"") || !strings.Contains(file.String(), "[2] готово") {
		t.Errorf("file = %q", file.String())
	}
}
