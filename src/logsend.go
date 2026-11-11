package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/telebot.v4"
)

// Отправка журналов разработчику: кнопка «Отправить логи» в панели собирает
// журналы и снимки страниц за последние часы в zip и шлёт его в Telegram-чат
// из настроек (или сохраняет в «Загрузки», если Telegram не настроен).

const (
	defaultLogHours = 2
	maxLogFileBytes = 20 << 20 // от очень длинного журнала берём хвост
	maxTelegramZip  = 45 << 20 // Bot API принимает файлы до 50 МБ
)

type logArchiveOptions struct {
	Hours     int    `json:"hours"`
	Snapshots bool   `json:"snapshots"`
	Comment   string `json:"comment"`
	Mode      string `json:"mode"` // telegram | save
}

var scriptRe = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)

// buildLogArchive упаковывает журналы из dir, изменённые за последние hours часов.
// Ключи из конфига вырезаются ещё раз (на случай строк, записанных до их маскировки),
// а из HTML-снимков убираются скрипты: в них бывают токены сессии Lokalise.
func buildLogArchive(dir string, opts logArchiveOptions, info string, red *redactor, now time.Time) ([]byte, int, error) {
	if opts.Hours <= 0 {
		opts.Hours = defaultLogHours
	}
	since := now.Add(-time.Duration(opts.Hours) * time.Hour)

	type entry struct {
		path, name string
		mod        time.Time
	}
	var files []entry
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".log" && !(ext == ".html" && opts.Snapshots) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.ModTime().Before(since) {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		files = append(files, entry{path, filepath.ToSlash(rel), fi.ModTime()})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, data []byte, mod time.Time) error {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod})
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	if err := add("info.txt", []byte(red.Redact(info)), now); err != nil {
		return nil, 0, err
	}
	for _, f := range files {
		data, err := readTail(f.path, maxLogFileBytes)
		if err != nil {
			continue
		}
		text := string(data)
		if strings.HasSuffix(f.name, ".html") {
			text = scriptRe.ReplaceAllString(text, "<script></script>")
		}
		if err := add(f.name, []byte(red.Redact(text)), f.mod); err != nil {
			return nil, 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), len(files), nil
}

func readTail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > limit {
		if _, err := f.Seek(fi.Size()-limit, io.SeekStart); err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		return append([]byte("… начало файла обрезано …\n"), data...), err
	}
	return io.ReadAll(f)
}

// archiveInfo — шапка архива: версии, настройки без ключей и состояние очереди.
func (a *app) archiveInfo(cfg FileConfig, opts logArchiveOptions, now time.Time) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Loka Translator %s\nmacOS %s (%s)\nСобрано: %s, журналы за %d ч.\nИдёт прогон: %v\n",
		AppVersion, macOSVersion(), runtime.GOARCH, now.Format("2006-01-02 15:04:05 MST"), opts.Hours, a.isRunning())
	if c := strings.TrimSpace(opts.Comment); c != "" {
		fmt.Fprintf(&sb, "\nЧто случилось:\n%s\n", c)
	}
	cfg.AnthropicAPIKey = mask(cfg.AnthropicAPIKey)
	cfg.TgBotToken = mask(cfg.TgBotToken)
	if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		fmt.Fprintf(&sb, "\nНастройки:\n%s\n", data)
	}
	if data, err := json.MarshalIndent(a.statusSnapshot(), "", "  "); err == nil {
		fmt.Fprintf(&sb, "\nОчередь:\n%s\n", data)
	}
	return sb.String()
}

func mask(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "(задан)"
}

func macOSVersion() string {
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return runtime.GOOS
	}
	return strings.TrimSpace(string(out))
}

func (a *app) handleLogsSend(w http.ResponseWriter, r *http.Request) {
	var opts logArchiveOptions
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&opts); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if opts.Hours <= 0 || opts.Hours > 48 {
		opts.Hours = defaultLogHours
	}
	cfg, err := a.cfg.Load()
	if err != nil {
		http.Error(w, "не удалось прочитать настройки: "+err.Error(), http.StatusInternalServerError)
		return
	}
	internal := toInternalConfig(cfg)
	red := &redactor{}
	red.Set(internal.APIKey, internal.TgBotToken)

	now := time.Now()
	slog.Info("📦 Собираю архив журналов", "hours", opts.Hours, "snapshots", opts.Snapshots, "mode", opts.Mode)
	data, n, err := buildLogArchive(logsDir, opts, a.archiveInfo(cfg, opts, now), red, now)
	if err != nil {
		http.Error(w, "не удалось собрать архив: "+err.Error(), http.StatusInternalServerError)
		return
	}
	name := "loka-logs-" + now.Format("2006-01-02_15-04") + ".zip"
	result := map[string]any{"files": n, "bytes": len(data), "name": name}

	switch opts.Mode {
	case "save":
		path, err := saveToDownloads(name, data)
		if err != nil {
			http.Error(w, "не удалось сохранить: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = exec.Command("open", "-R", path).Start()
		slog.Info("📦 Архив журналов сохранён", "path", path)
		result["path"] = path
	default:
		notify := newNotifier(internal.TgBotToken, internal.ChatId)
		if notify == nil {
			http.Error(w, "Telegram не настроен: укажите токен бота и Chat ID в настройках или сохраните архив", http.StatusBadRequest)
			return
		}
		if len(data) > maxTelegramZip {
			http.Error(w, fmt.Sprintf("архив %d МБ — больше лимита Telegram; уменьшите период или сохраните архив", len(data)>>20), http.StatusBadRequest)
			return
		}
		caption := fmt.Sprintf("📦 Логи Loka Translator %s · macOS %s · за %d ч.", AppVersion, macOSVersion(), opts.Hours)
		if c := strings.TrimSpace(opts.Comment); c != "" {
			caption += "\n\n" + c
		}
		if err := notify.SendFile(name, data, truncate(caption, 1000)); err != nil {
			msg := "Telegram не принял архив: " + red.Redact(err.Error())
			if strings.Contains(err.Error(), "chat not found") {
				msg += " — бот не видит чат: добавьте бота в чат и проверьте Chat ID в настройках"
			}
			slog.Warn("⚠️ "+msg, "chat", internal.ChatId)
			http.Error(w, msg, http.StatusBadGateway)
			return
		}
		slog.Info("📦 Архив журналов отправлен в Telegram", "files", n, "kb", len(data)>>10)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func saveToDownloads(name string, data []byte) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Downloads")
	if _, err := os.Stat(dir); err != nil {
		return "", errors.New("нет папки «Загрузки»")
	}
	path := filepath.Join(dir, name)
	return path, os.WriteFile(path, data, 0o600)
}

// SendFile отправляет файл документом в чат уведомлений.
func (n *notifier) SendFile(name string, data []byte, caption string) error {
	doc := &telebot.Document{File: telebot.FromReader(bytes.NewReader(data)), FileName: name, Caption: caption}
	_, err := n.bot.Send(n.chat, doc)
	return err
}
