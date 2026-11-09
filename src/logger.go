package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ============================================================
// Маскировка секретов в логах
// ============================================================

type redactor struct {
	mu      sync.RWMutex
	secrets []string
}

var secrets = &redactor{}

// Set заменяет список секретов. Короткие строки игнорируются, чтобы не
// замазать случайные совпадения.
func (r *redactor) Set(values ...string) {
	var list []string
	for _, v := range values {
		if v = strings.TrimSpace(v); len(v) >= 8 {
			list = append(list, v)
		}
	}
	r.mu.Lock()
	r.secrets = list
	r.mu.Unlock()
}

func (r *redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, secret := range r.secrets {
		s = strings.ReplaceAll(s, secret, maskSecret(secret))
	}
	return s
}

func maskSecret(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// ============================================================
// SSE-брокер для логов в браузер
// ============================================================

type SSEBroker struct {
	clients map[chan []byte]bool
	mu      sync.Mutex
}

func NewSSEBroker() *SSEBroker {
	return &SSEBroker{clients: make(map[chan []byte]bool)}
}

func (b *SSEBroker) AddClient(c chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[c] = true
}

func (b *SSEBroker) RemoveClient(c chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, c)
	close(c)
}

func (b *SSEBroker) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Каждая строка — отдельное SSE-событие; копируем, т.к. p может переиспользоваться.
	for _, line := range bytes.Split(bytes.TrimRight(p, "\n"), []byte("\n")) {
		msg := append([]byte(nil), line...)
		for c := range b.clients {
			select {
			case c <- msg:
			default: // клиент не успевает читать — пропускаем
			}
		}
	}
	return len(p), nil
}

func (b *SSEBroker) BroadcastControl(msg string) {
	_, _ = b.Write([]byte(msg))
}

// ============================================================
// slog handler: время + сообщение + атрибуты, с маскировкой секретов
// ============================================================

type CustomHandler struct {
	out   io.Writer
	level slog.Level
	attrs []slog.Attr
	mu    *sync.Mutex
}

func (h *CustomHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *CustomHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Time.Format("15:04:05"))
	sb.WriteByte(' ')
	sb.WriteString(r.Message)
	write := func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindString {
			fmt.Fprintf(&sb, " %s=%q", a.Key, a.Value.String())
		} else {
			fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value.Any())
		}
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	sb.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.out.Write([]byte(secrets.Redact(sb.String())))
	return err
}

func (h *CustomHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *CustomHandler) WithGroup(string) slog.Handler { return h }

func setupLogger(broker *SSEBroker) *os.File {
	now := time.Now()
	dir := filepath.Join("data", "logs", now.Format("2006-01-02"))
	_ = os.MkdirAll(dir, 0o755)

	writers := []io.Writer{os.Stdout, broker}
	file, err := os.OpenFile(filepath.Join(dir, now.Format("15-04-05")+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		writers = append(writers, file)
	} else {
		file = nil
	}

	slog.SetDefault(slog.New(&CustomHandler{
		out:   io.MultiWriter(writers...),
		level: slog.LevelInfo,
		mu:    &sync.Mutex{},
	}))
	return file
}
