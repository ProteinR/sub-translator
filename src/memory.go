package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// translationMemory — локальный кэш готовых переводов в data/tm.jsonl.
// Перезапуск после ошибки, режим перезаписи и повторяющиеся строки больше не
// оплачиваются повторно. Формат append-only: одна строка — одна запись.
type translationMemory struct {
	mu      sync.Mutex
	path    string
	entries map[string]string
	file    *os.File
}

type tmRecord struct {
	K string `json:"k"`
	T string `json:"t"`
}

func openTranslationMemory(path string) (*translationMemory, error) {
	tm := &translationMemory{path: path, entries: map[string]string{}}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
		bad := 0
		for sc.Scan() {
			var r tmRecord
			if json.Unmarshal(sc.Bytes(), &r) != nil || r.K == "" {
				bad++ // обрубок последней строки после падения — просто пропускаем
				continue
			}
			tm.entries[r.K] = r.T
		}
		f.Close()
		if bad > 0 {
			slog.Warn("⚠️ В памяти переводов пропущены битые строки", "count", bad)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// Если прошлый процесс упал посреди записи, дописываем перевод строки,
	// чтобы новая запись не приклеилась к обрубку.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		if last, err := lastByte(path); err == nil && last != '\n' {
			_, _ = f.Write([]byte{'\n'})
		}
	}
	tm.file = f
	return tm, nil
}

func (tm *translationMemory) Len() int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return len(tm.entries)
}

func (tm *translationMemory) Get(key string) (string, bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t, ok := tm.entries[key]
	return t, ok
}

func (tm *translationMemory) Put(key, translation string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if old, ok := tm.entries[key]; ok && old == translation {
		return
	}
	tm.entries[key] = translation
	line, _ := json.Marshal(tmRecord{K: key, T: translation})
	if _, err := tm.file.Write(append(line, '\n')); err != nil {
		slog.Warn("⚠️ Не удалось записать в память переводов", "error", err)
	}
}

func (tm *translationMemory) Close() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.file.Close()
}

// memoryScope — часть ключа, общая для прогона: основная модель и промпт.
// Поменял промпт или модель — старые переводы не подмешиваются.
func memoryScope(model, prompt string) string {
	h := sha256.Sum256([]byte(model + "\x00" + prompt))
	return hex.EncodeToString(h[:8])
}

func memoryKey(scope string, it TranslationItem) string {
	h := sha256.Sum256([]byte(scope + "\x00" + it.SourceLang + "\x00" + it.TargetLang + "\x00" + it.Original))
	return hex.EncodeToString(h[:16])
}

// cachedTranslator оборачивает переводчик: сначала память, затем дедупликация
// одинаковых строк внутри пачки, и только остаток уходит в API.
type cachedTranslator struct {
	inner translator
	tm    *translationMemory
	scope string
	usage *usageTracker
}

func (c *cachedTranslator) Translate(ctx context.Context, req translateRequest) ([]TranslationItem, error) {
	out := append([]TranslationItem(nil), req.Items...)
	keys := make([]string, len(out))
	first := map[string]int{}
	dupOf := map[int]int{}
	var missIdx []int
	hits := 0

	for i, it := range out {
		k := memoryKey(c.scope, it)
		keys[i] = k
		if t, ok := c.tm.Get(k); ok {
			out[i].Translation = t
			hits++
			continue
		}
		if j, ok := first[k]; ok {
			dupOf[i] = j
			continue
		}
		first[k] = i
		missIdx = append(missIdx, i)
	}

	if len(missIdx) > 0 {
		sub := make([]TranslationItem, len(missIdx))
		for n, i := range missIdx {
			sub[n] = out[i]
		}
		res, err := c.inner.Translate(ctx, translateRequest{Items: sub, Context: req.Context})
		if err != nil {
			return nil, err
		}
		for n, i := range missIdx {
			out[i].Translation = res[n].Translation
			c.tm.Put(keys[i], res[n].Translation)
		}
	}
	for i, j := range dupOf {
		out[i].Translation = out[j].Translation
		hits++
	}
	if hits > 0 {
		logOf(ctx).Info("🧠 Взято из памяти переводов", "cells", hits)
		if c.usage != nil {
			c.usage.AddMemoryHits(hits)
		}
	}
	return out, nil
}

func lastByte(path string) (byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, mustSize(f)-1); err != nil {
		return 0, err
	}
	return b[0], nil
}

func mustSize(f *os.File) int64 {
	fi, err := f.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}
