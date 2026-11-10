package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultModel         = "claude-haiku-5-5"
	defaultFallbackModel = "claude-sonnet-5-5"
	noFallbackModel      = "none"
)

const defaultTranslationPrompt = `You are a professional subtitle and script localizer for an online learning platform: business and personal-development coaching, guided meditations, sports and fitness lessons, psychology podcasts. Viewers read your text while listening to the speaker, so it must sound like natural spoken language, not like a translation.

## Style
- Translate meaning and intent, not words. Rephrase freely when a literal version sounds stiff.
- Match the tone: calm and warm in meditations, energetic and direct in sports and coaching, conversational in podcasts.
- Keep lines short and readable on screen; don't make a line much longer than the source.
- Address the viewer like a native coach would: "ty" in Polish, "ты" in Russian and Ukrainian, direct "you" in English, unless the source is clearly formal.
- Keep names, brands and course titles unless there is an established translation.
- No trailing period in titles and headings.
- Never add, drop or soften content. No explanations.

## Speaker gender
Infer it only from the text: gendered verb forms or adjectives in the source (e.g. Polish "zrobiłam" / "zrobiłem"), the speaker's name, or how others refer to them. If unclear, prefer constructions that don't mark gender instead of guessing.

## Target-language rules
Polish:
- Use the speaker's grammatical gender in past-tense verbs and adjectives; use feminatives (trenerka, ekspertka, psycholożka) for a female speaker where natural.
- Every sentence needs a finite verb: never leave an adverbial participle (robiąc, pozostawiając) without a main verb. If the line is a title, drop the period instead.
English:
- Avoid Slavic calques: wordy "the fact that", "which is", heavy passive voice.
- he/she by the speaker's gender; "they" only for groups or an unspecified person.
- Turn adverbial participles into clear clauses ("While doing…"); no dangling modifiers.
Russian / Ukrainian:
- Natural spoken language: no канцелярит, no anglicisms where a native word exists.
- Ukrainian must be real Ukrainian, not Russian with Ukrainian spelling.

## Formatting
Preserve line breaks, paragraph breaks, placeholders, tags and timing markers exactly.`

// FileConfig — то, что лежит в data/config.json и ходит в UI.
type FileConfig struct {
	AnthropicAPIKey   string   `json:"anthropicApiKey"`
	Model             string   `json:"model"`
	FallbackModel     string   `json:"fallbackModel"` // "none" — без запасной модели
	BudgetUSD         float64  `json:"budgetUsd"`     // 0 — без лимита
	MaxConcurrency    int      `json:"maxConcurrency"`
	TranslationPrompt string   `json:"translationPrompt"`
	TgBotToken        string   `json:"tgBotToken"`
	ChatId            string   `json:"chatId"`
	ScrollDelay       int      `json:"scrollDelay"`
	EditorLoadDelay   int      `json:"editorLoadDelay"`
	BeforeSaveDelay   int      `json:"beforeSaveDelay"`
	RowNextDelay      int      `json:"rowNextDelay"`
	Projects          []string `json:"projects"`
	OverwriteFilled   bool     `json:"overwriteFilled"`
}

// Config — рабочий снимок настроек на время одного прогона.
type Config struct {
	APIKey          string
	Model           string
	FallbackModel   string // "" — без запасной модели
	BudgetUSD       float64
	MaxConcurrency  int
	Prompt          string
	TgBotToken      string
	ChatId          string
	ScrollDelay     time.Duration
	EditorLoadDelay time.Duration
	BeforeSaveDelay time.Duration
	RowNextDelay    time.Duration
	Projects        []string
	OverwriteFilled bool
}

func defaultConfig() FileConfig {
	return FileConfig{
		Model:             defaultModel,
		FallbackModel:     defaultFallbackModel,
		MaxConcurrency:    1,
		TranslationPrompt: defaultTranslationPrompt,
		ScrollDelay:       2000,
		EditorLoadDelay:   500,
		BeforeSaveDelay:   300,
		RowNextDelay:      200,
		Projects:          []string{},
	}
}

func normalizeConfig(cfg *FileConfig) {
	cfg.AnthropicAPIKey = strings.TrimSpace(cfg.AnthropicAPIKey)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if !isClaudeModel(cfg.Model) {
		cfg.Model = defaultModel
	}
	cfg.FallbackModel = strings.TrimSpace(cfg.FallbackModel)
	if cfg.FallbackModel != noFallbackModel && !isClaudeModel(cfg.FallbackModel) {
		cfg.FallbackModel = defaultFallbackModel
	}
	if cfg.BudgetUSD < 0 {
		cfg.BudgetUSD = 0
	}
	if strings.TrimSpace(cfg.TranslationPrompt) == "" {
		cfg.TranslationPrompt = defaultTranslationPrompt
	}
	cfg.MaxConcurrency = clamp(cfg.MaxConcurrency, 1, 8)
	for _, d := range []*int{&cfg.ScrollDelay, &cfg.EditorLoadDelay, &cfg.BeforeSaveDelay, &cfg.RowNextDelay} {
		if *d < 0 {
			*d = 0
		}
	}
	seen := map[string]bool{}
	projects := make([]string, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") || seen[p] {
			continue
		}
		seen[p] = true
		projects = append(projects, p)
	}
	cfg.Projects = projects
}

// isClaudeModel отсекает пустые и чужие названия моделей (их Anthropic API не примет).
func isClaudeModel(m string) bool { return strings.HasPrefix(strings.ToLower(m), "claude-") }

func clamp(v, lo, hi int) int {
	return max(lo, min(v, hi))
}

func toInternalConfig(fc FileConfig) Config {
	key := fc.AnthropicAPIKey
	if key == "" {
		key = strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	}
	fallback := fc.FallbackModel
	if fallback == noFallbackModel || fallback == fc.Model {
		fallback = ""
	}
	ms := func(v int) time.Duration { return time.Duration(v) * time.Millisecond }
	return Config{
		APIKey:          key,
		Model:           fc.Model,
		FallbackModel:   fallback,
		BudgetUSD:       fc.BudgetUSD,
		MaxConcurrency:  fc.MaxConcurrency,
		Prompt:          fc.TranslationPrompt,
		TgBotToken:      strings.TrimSpace(fc.TgBotToken),
		ChatId:          strings.TrimSpace(fc.ChatId),
		ScrollDelay:     ms(fc.ScrollDelay),
		EditorLoadDelay: ms(fc.EditorLoadDelay),
		BeforeSaveDelay: ms(fc.BeforeSaveDelay),
		RowNextDelay:    ms(fc.RowNextDelay),
		Projects:        append([]string(nil), fc.Projects...),
		OverwriteFilled: fc.OverwriteFilled,
	}
}

// configStore сериализует все чтения/записи конфига: UI-автосейв и удаление
// завершённых проектов больше не гоняются друг с другом.
type configStore struct {
	mu   sync.Mutex
	path string
}

func newConfigStore(path string) *configStore {
	return &configStore{path: path}
}

func (s *configStore) Load() (FileConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *configStore) loadLocked() (FileConfig, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), fmt.Errorf("config.json повреждён: %w", err)
	}
	normalizeConfig(&cfg)
	return cfg, nil
}

func (s *configStore) Save(cfg FileConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(cfg)
}

func (s *configStore) saveLocked(cfg FileConfig) error {
	normalizeConfig(&cfg)
	// Не затираем молча битый файл: в нём могут быть ключи и очередь проектов.
	if data, err := os.ReadFile(s.path); err == nil && !json.Valid(data) {
		backup := fmt.Sprintf("%s.broken-%s", s.path, time.Now().Format("20060102-150405"))
		if err := os.Rename(s.path, backup); err == nil {
			slog.Warn("⚠️ Повреждённый config.json сохранён как резервная копия", "path", backup)
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, data, 0o600)
}

// Update читает, меняет и пишет конфиг под одной блокировкой.
func (s *configStore) Update(fn func(*FileConfig)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg, err := s.loadLocked()
	if err != nil {
		return err
	}
	fn(&cfg)
	return s.saveLocked(cfg)
}

func (s *configStore) RemoveProject(url string) error {
	return s.Update(func(cfg *FileConfig) {
		kept := cfg.Projects[:0]
		for _, p := range cfg.Projects {
			if p != url {
				kept = append(kept, p)
			}
		}
		cfg.Projects = kept
	})
}

// writeFileAtomic пишет во временный файл рядом и переименовывает: при падении
// посреди записи остаётся либо старый, либо новый файл, но не обрубок.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op после успешного rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
