package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Обновления: приложение смотрит последний релиз на GitHub, а по кнопке скачивает
// zip, заменяет свой .app и перезапускается. Релизы выпускает scripts/release.sh.

const (
	releaseRepo    = "ProteinR/sub-translator"
	appBundleName  = "Loka Translator.app"
	appBundleID    = "com.proteinr.loka-translator"
	updateCheckTTL = 30 * time.Minute
)

var releaseAPI = "https://api.github.com/repos/" + releaseRepo + "/releases/latest"

type updateInfo struct {
	Current    string `json:"current"`
	Latest     string `json:"latest,omitempty"`
	Available  bool   `json:"available"`
	Notes      string `json:"notes,omitempty"`
	PageURL    string `json:"pageUrl,omitempty"`
	CanInstall bool   `json:"canInstall"` // false — запущено не из .app (разработка)
	Error      string `json:"error,omitempty"`
	assetURL   string
}

type updater struct {
	mu      sync.Mutex
	checked time.Time
	info    updateInfo
	busy    bool
	client  *http.Client
	quit    func() // закрыть приложение после установки
}

func newUpdater(quit func()) *updater {
	return &updater{client: &http.Client{Timeout: 5 * time.Minute}, quit: quit}
}

// Check возвращает сведения о последнем релизе (с кэшем, force — без него).
func (u *updater) Check(ctx context.Context, force bool) updateInfo {
	u.mu.Lock()
	if !force && time.Since(u.checked) < updateCheckTTL {
		info := u.info
		u.mu.Unlock()
		return info
	}
	u.mu.Unlock()

	info := updateInfo{Current: AppVersion, CanInstall: currentBundle() != ""}
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		info.Error = err.Error()
		slog.Warn("⚠️ Не удалось проверить обновления", "error", err)
	} else {
		info.Latest = strings.TrimPrefix(rel.Tag, "v")
		info.Notes = truncate(strings.TrimSpace(rel.Body), 2000)
		info.PageURL = rel.HTMLURL
		info.assetURL = rel.assetURL()
		info.Available = info.assetURL != "" && versionLess(AppVersion, info.Latest)
	}

	u.mu.Lock()
	u.info, u.checked = info, time.Now()
	u.mu.Unlock()
	return info
}

type ghRelease struct {
	Tag     string `json:"tag_name"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (r ghRelease) assetURL() string {
	for _, a := range r.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
			return a.URL
		}
	}
	return ""
}

func (u *updater) fetchLatest(ctx context.Context) (ghRelease, error) {
	var rel ghRelease
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseAPI, nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "loka-translator/"+AppVersion)
	resp, err := u.client.Do(req)
	if err != nil {
		return rel, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return rel, errors.New("на GitHub ещё нет релизов")
	}
	if resp.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("GitHub ответил %s", resp.Status)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel)
	return rel, err
}

// Install скачивает релиз, подменяет текущий .app и перезапускает приложение.
func (u *updater) Install(ctx context.Context) error {
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return errors.New("обновление уже устанавливается")
	}
	u.busy = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()

	app := currentBundle()
	if app == "" {
		return errors.New("приложение запущено не из Loka Translator.app — обновите через git")
	}
	info := u.Check(ctx, true)
	if info.Error != "" {
		return errors.New(info.Error)
	}
	if !info.Available {
		return errors.New("уже установлена последняя версия")
	}

	// Временная папка рядом с .app: тот же диск, значит замена — простое переименование.
	parent := filepath.Dir(app)
	work, err := os.MkdirTemp(parent, ".loka-update-")
	if err != nil {
		return fmt.Errorf("нет прав на запись в %s: %w", parent, err)
	}
	defer os.RemoveAll(work)

	slog.Info("⬇️ Скачиваю обновление", "version", info.Latest)
	zipPath := filepath.Join(work, "update.zip")
	if err := u.download(ctx, info.assetURL, zipPath); err != nil {
		return err
	}
	unpacked := filepath.Join(work, "unpacked")
	if out, err := exec.CommandContext(ctx, "/usr/bin/ditto", "-x", "-k", zipPath, unpacked).CombinedOutput(); err != nil {
		return fmt.Errorf("архив не распакован: %v %s", err, out)
	}
	newApp := filepath.Join(unpacked, appBundleName)
	if err := checkBundle(newApp, info.Latest); err != nil {
		return err
	}

	backup := app + ".old"
	_ = os.RemoveAll(backup)
	if err := os.Rename(app, backup); err != nil {
		return fmt.Errorf("не удалось заменить приложение: %w", err)
	}
	if err := os.Rename(newApp, app); err != nil {
		_ = os.Rename(backup, app)
		return fmt.Errorf("не удалось установить новую версию: %w", err)
	}
	_ = os.RemoveAll(backup) // запущенный бинарник продолжает работать и после удаления
	slog.Info("✅ Обновление установлено, перезапускаю", "version", info.Latest)

	// Новый экземпляр стартует, когда этот уже закрылся и освободил порт.
	cmd := exec.Command("/bin/sh", "-c", `sleep 1; while /usr/bin/pgrep -qf "$1/Contents/MacOS/"; do sleep 0.3; done; /usr/bin/open "$1"`, "sh", app)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("новая версия установлена, но не запустилась — откройте приложение вручную: %w", err)
	}
	go func() {
		time.Sleep(500 * time.Millisecond) // даём ответу дойти до панели
		u.quit()
	}()
	return nil
}

func (u *updater) download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "loka-translator/"+AppVersion)
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("не удалось скачать обновление: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("не удалось скачать обновление: %s", resp.Status)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 500<<20)); err != nil {
		f.Close()
		return fmt.Errorf("загрузка оборвалась: %w", err)
	}
	return f.Close()
}

// checkBundle проверяет, что в архиве именно наше приложение нужной версии.
func checkBundle(app, version string) error {
	plist := filepath.Join(app, "Contents", "Info.plist")
	read := func(key string) string {
		out, _ := exec.Command("/usr/bin/plutil", "-extract", key, "raw", plist).Output()
		return strings.TrimSpace(string(out))
	}
	if id := read("CFBundleIdentifier"); id != appBundleID {
		return fmt.Errorf("в архиве чужое приложение (%q)", id)
	}
	if v := read("CFBundleShortVersionString"); v != version {
		return fmt.Errorf("в архиве версия %q вместо %q", v, version)
	}
	exe := filepath.Join(app, "Contents", "MacOS", read("CFBundleExecutable"))
	if fi, err := os.Stat(exe); err != nil || fi.Mode()&0o111 == 0 {
		return errors.New("в архиве нет исполняемого файла приложения")
	}
	return nil
}

// currentBundle — путь к запущенному .app или "" при запуске не из бандла.
func currentBundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	macos := filepath.Dir(exe)
	app := filepath.Dir(filepath.Dir(macos))
	if filepath.Base(macos) != "MacOS" || !strings.HasSuffix(app, ".app") {
		return ""
	}
	return app
}

// versionLess сравнивает версии вида 3.1.0 покомпонентно.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}
