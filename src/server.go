package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

type app struct {
	cfg    *configStore
	broker *SSEBroker
	port   string

	runMu   sync.Mutex
	running bool
	cancel  context.CancelFunc

	statusMu sync.Mutex
	statuses map[string]projectStatus
}

type APIData struct {
	Config    FileConfig `json:"config"`
	IsRunning bool       `json:"isRunning"`
	Version   string     `json:"version"`
	// Только для чтения: UI берёт отсюда текст для кнопки «Сбросить на дефолтный».
	DefaultPrompt string `json:"defaultPrompt,omitempty"`
	// Ключ задан переменной ANTHROPIC_API_KEY — поле в настройках можно не заполнять.
	EnvAPIKey     bool                     `json:"envApiKey"`
	ProjectStatus map[string]projectStatus `json:"projectStatus"`
}

func newApp(cfg *configStore, broker *SSEBroker, port string) *app {
	return &app{cfg: cfg, broker: broker, port: port}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/data", a.handleGetData)
	mux.HandleFunc("POST /api/save", a.handleSaveData)
	mux.HandleFunc("POST /api/start", a.handleStart)
	mux.HandleFunc("POST /api/stop", a.handleStop)
	mux.HandleFunc("GET /api/logs", a.handleLogs)
	return a.guard(mux)
}

// guard закрывает две дыры локального веб-сервера:
//   - DNS rebinding: чужой домен, указывающий на 127.0.0.1, не пройдёт проверку Host;
//   - CSRF: любой сайт в браузере мог POST-ить на localhost. Кастомный заголовок
//     требует CORS-preflight, а CORS мы не разрешаем, поэтому чужая страница его не пошлёт.
func (a *app) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.isLocalHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Loka-Request") != "1" {
				http.Error(w, "missing X-Loka-Request header", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || !a.isLocalHost(u.Host) {
					http.Error(w, "forbidden origin", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) isLocalHost(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port != a.port {
		return false
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func (a *app) isRunning() bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.running
}

func (a *app) handleGetData(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.cfg.Load()
	if err != nil {
		// Отдаём ошибку, а не дефолты: иначе UI автосейвом перетрёт файл пустыми полями.
		slog.Error("❌ Не удалось прочитать конфиг", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(APIData{
		Config:        cfg,
		IsRunning:     a.isRunning(),
		Version:       AppVersion,
		DefaultPrompt: defaultTranslationPrompt,
		EnvAPIKey:     strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) != "",
		ProjectStatus: a.statusSnapshot(),
	})
}

func (a *app) handleSaveData(w http.ResponseWriter, r *http.Request) {
	var req APIData
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.cfg.Save(req.Config); err != nil {
		slog.Error("❌ Не удалось сохранить конфиг", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (a *app) handleStart(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	if a.running {
		a.runMu.Unlock()
		http.Error(w, "Already running", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.running = true
	a.cancel = cancel
	a.runMu.Unlock()

	a.broker.BroadcastControl("___PROCESS_STARTED___")
	go a.runTranslation(ctx)
	w.WriteHeader(http.StatusOK)
}

func (a *app) handleStop(w http.ResponseWriter, r *http.Request) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if !a.running || a.cancel == nil {
		http.Error(w, "Not running", http.StatusConflict)
		return
	}
	a.cancel()
	w.WriteHeader(http.StatusOK)
}

func (a *app) handleLogs(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	clientChan := make(chan []byte, 256)
	a.broker.AddClient(clientChan)
	defer a.broker.RemoveClient(clientChan)

	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case msg := <-clientChan:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

func openBrowser(url string) {
	time.Sleep(1 * time.Second) // даём серверу подняться
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		slog.Warn("Could not open browser automatically", "error", err)
	}
}
