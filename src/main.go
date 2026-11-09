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

const AppVersion = "2.2.0"

const defaultPort = "8080"

func main() {
	if exePath, err := os.Executable(); err == nil {
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

	go openBrowser(uiURL)

	if err := srv.ListenAndServe(); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}
