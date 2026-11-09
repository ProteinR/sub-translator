package main

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

// projectStatus — состояние проекта для UI: очередь показывает его бейджем,
// прогресс-баром и понятным текстом причины ошибки.
type projectStatus struct {
	URL    string `json:"url"`
	Name   string `json:"name,omitempty"`
	Status string `json:"status"` // pending | running | error | done
	Done   int    `json:"done"`
	Total  int    `json:"total"`
	Detail string `json:"detail,omitempty"`
}

const (
	statusPending = "pending"
	statusRunning = "running"
	statusError   = "error"
	statusDone    = "done"
)

// projectReporter копит состояние одного проекта и отправляет каждое изменение.
// nil-безопасен: processProject можно звать без него (тесты, фикстуры).
type projectReporter struct {
	mu   sync.Mutex
	st   projectStatus
	emit func(projectStatus)
}

func (r *projectReporter) update(fn func(*projectStatus)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	fn(&r.st)
	s := r.st
	r.mu.Unlock()
	if r.emit != nil {
		r.emit(s)
	}
}

func (r *projectReporter) detail(text string) {
	r.update(func(s *projectStatus) { s.Detail = text })
}

func (a *app) resetStatuses(urls []string) {
	a.statusMu.Lock()
	a.statuses = make(map[string]projectStatus, len(urls))
	a.statusMu.Unlock()
	for _, u := range urls {
		a.setStatus(projectStatus{URL: u, Status: statusPending, Detail: "Ждёт своей очереди"})
	}
}

func (a *app) setStatus(s projectStatus) {
	a.statusMu.Lock()
	if a.statuses == nil {
		a.statuses = map[string]projectStatus{}
	}
	a.statuses[s.URL] = s
	a.statusMu.Unlock()
	if data, err := json.Marshal(s); err == nil {
		a.broker.BroadcastControl("___PROJECT___|" + string(data))
	}
}

func (a *app) statusSnapshot() map[string]projectStatus {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	out := make(map[string]projectStatus, len(a.statuses))
	for k, v := range a.statuses {
		out[k] = v
	}
	return out
}

// humanizeError переводит ошибку в текст для человека в очереди проектов.
func humanizeError(err error) string {
	var apiErr *apiError
	switch {
	case errors.Is(err, errBudgetExceeded):
		return "Остановлено: достигнут лимит расходов на запуск. Проект остался в очереди."
	case errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403):
		return "Anthropic не принял ключ — проверьте его в настройках."
	case errors.As(err, &apiErr) && apiErr.Status == 404:
		return "Модель не найдена — выберите другую в настройках."
	case strings.Contains(err.Error(), "не удалось вставить"):
		return truncate(secrets.Redact(err.Error()), 300) + ". Проект остался в очереди — запустите ещё раз, готовое не перезапишется."
	case strings.Contains(err.Error(), "не найдены строки Multilingual"):
		return "Не удалось открыть строки проекта. Проверьте ссылку и что в Lokalise видны нужные языки."
	}
	return truncate(secrets.Redact(err.Error()), 300)
}
