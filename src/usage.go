package main

import (
	"errors"
	"fmt"
	"sync"
)

// Цены за 1M токенов, USD (страница цен Anthropic, ноябрь 2026).
// Haiku 5.5: ставки для промптов до 100k токенов — наши запросы на порядки меньше.
// Запись в кэш (5 мин) — стандартный множитель 1.25× к входу.
type modelPrice struct{ In, Out, CacheWrite, CacheRead float64 }

var modelPrices = map[string]modelPrice{
	"claude-haiku-5-5":  {In: 0.10, Out: 0.50, CacheWrite: 0.125, CacheRead: 0.01},
	"claude-sonnet-5-5": {In: 2, Out: 10, CacheWrite: 2.50, CacheRead: 0.10},
	"claude-opus-5-5":   {In: 4, Out: 20, CacheWrite: 5, CacheRead: 0.20},
}

var errBudgetExceeded = errors.New("достигнут лимит бюджета на прогон")

type usageSnapshot struct {
	Requests     int     `json:"requests"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CacheRead    int64   `json:"cacheRead"`
	CacheWrite   int64   `json:"cacheWrite"`
	MemoryHits   int     `json:"memoryHits"`
	CostUSD      float64 `json:"costUsd"`
	BudgetUSD    float64 `json:"budgetUsd"`
	// true, если среди моделей была такая, чьей цены мы не знаем — сумма занижена.
	CostIncomplete bool `json:"costIncomplete"`
}

type usageTracker struct {
	mu       sync.Mutex
	s        usageSnapshot
	onChange func(usageSnapshot)
}

func newUsageTracker(budgetUSD float64, onChange func(usageSnapshot)) *usageTracker {
	return &usageTracker{s: usageSnapshot{BudgetUSD: budgetUSD}, onChange: onChange}
}

func (u *usageTracker) Add(model string, usage apiUsage) {
	u.mu.Lock()
	u.s.Requests++
	u.s.InputTokens += usage.InputTokens
	u.s.OutputTokens += usage.OutputTokens
	u.s.CacheRead += usage.CacheReadInputTokens
	u.s.CacheWrite += usage.CacheCreationInputTokens
	if p, ok := modelPrices[model]; ok {
		u.s.CostUSD += (float64(usage.InputTokens)*p.In +
			float64(usage.OutputTokens)*p.Out +
			float64(usage.CacheCreationInputTokens)*p.CacheWrite +
			float64(usage.CacheReadInputTokens)*p.CacheRead) / 1e6
	} else {
		u.s.CostIncomplete = true
	}
	snap := u.s
	u.mu.Unlock()
	u.notify(snap)
}

func (u *usageTracker) AddMemoryHits(n int) {
	if n == 0 {
		return
	}
	u.mu.Lock()
	u.s.MemoryHits += n
	snap := u.s
	u.mu.Unlock()
	u.notify(snap)
}

func (u *usageTracker) CheckBudget() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.s.BudgetUSD > 0 && u.s.CostUSD >= u.s.BudgetUSD {
		return fmt.Errorf("%w: потрачено ~$%.4f из $%.2f", errBudgetExceeded, u.s.CostUSD, u.s.BudgetUSD)
	}
	return nil
}

func (u *usageTracker) Snapshot() usageSnapshot {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.s
}

func (u *usageTracker) notify(s usageSnapshot) {
	if u.onChange != nil {
		u.onChange(s)
	}
}
