package main

import (
	_ "embed"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed index.html
var indexHTML []byte

// var, а не const: build-app.sh может подменить версию (APP_VERSION) для проверки обновлений.
var AppVersion = "3.3.0"

const defaultPort = "8080"

func main() {
	if app := currentBundle(); app != "" {
		// Установленное приложение (хоть в /Applications) хранит данные у пользователя.
		if home, err := os.UserHomeDir(); err == nil {
			dir := filepath.Join(home, "Library", "Application Support", "Loka Translator")
			_ = os.MkdirAll(filepath.Join(dir, "data"), 0o700)
			migrateData(filepath.Join(filepath.Dir(app), "data"), filepath.Join(dir, "data"))
			_ = os.Chdir(dir)
		}
	} else if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		if !strings.Contains(exeDir, "go-build") && !strings.Contains(exeDir, "Temp") && !strings.Contains(exeDir, "tmp") {
			_ = os.Chdir(exeDir)
		}
	}

	_ = os.MkdirAll("data", 0o755)

	broker := NewSSEBroker()
	if logFile := setupLogger(broker); logFile != nil {
		defer logFile.Close()
	}

	port := os.Getenv("LOKA_PORT")
	if port == "" {
		port = defaultPort
	}

	a := newApp(newConfigStore(filepath.Join("data", "config.json")), broker, port)

	// Слушаем только loopback: ключи и управление не должны торчать в локальную сеть.
	addr := net.JoinHostPort("127.0.0.1", port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	uiURL := "http://localhost:" + port
	slog.Info("🚀 Loka Translator", "version", AppVersion)
	slog.Info("🌐 Starting Web UI on " + uiURL)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
	go func() {
		if err := srv.Serve(ln); err != nil {
			slog.Error("Server failed", "error", err)
			os.Exit(1)
		}
	}()

	// На macOS — своё окно с вкладками (WebKit); LOKA_BROWSER=1 — панель в обычном браузере.
	if hasNativeWindow && os.Getenv("LOKA_BROWSER") != "1" {
		runNativeWindow(uiURL)
		return
	}
	go openBrowser(uiURL)
	select {}
}

// migrateData один раз переносит настройки и память переводов из папки data
// рядом с .app (так было до версии 3.1) в папку пользователя.
func migrateData(from, to string) {
	if _, err := os.Stat(filepath.Join(to, "config.json")); err == nil {
		return
	}
	for _, name := range []string{"config.json", "tm.jsonl"} {
		data, err := os.ReadFile(filepath.Join(from, name))
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(to, name), data, 0o600)
	}
}
