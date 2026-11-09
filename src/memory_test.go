package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type fakeTranslator struct{ calls, cells atomic.Int32 }

func (f *fakeTranslator) Translate(_ context.Context, req translateRequest) ([]TranslationItem, error) {
	f.calls.Add(1)
	f.cells.Add(int32(len(req.Items)))
	out := append([]TranslationItem(nil), req.Items...)
	for i := range out {
		out[i].Translation = out[i].Original + "@" + out[i].TargetLang
	}
	return out, nil
}

func TestMemoryDedupesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tm.jsonl")
	tm, err := openTranslationMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeTranslator{}
	usage := newUsageTracker(0, nil)
	c := &cachedTranslator{inner: fake, tm: tm, scope: memoryScope("m", "p"), usage: usage}

	items := []TranslationItem{
		cell("1", "Okay", "Polish"), cell("2", "Let's begin", "Polish"), cell("3", "Okay", "Polish"), // 3 — повтор 1
	}
	got, err := c.Translate(context.Background(), translateRequest{Items: items})
	if err != nil || got[2].Translation != "Okay@Polish" || fake.cells.Load() != 2 {
		t.Fatalf("dedupe failed: cells=%d got=%+v err=%v", fake.cells.Load(), got, err)
	}
	tm.Close()

	// Новый прогон: всё из памяти, API не трогаем.
	tm2, _ := openTranslationMemory(path)
	defer tm2.Close()
	c2 := &cachedTranslator{inner: fake, tm: tm2, scope: memoryScope("m", "p"), usage: usage}
	if _, err := c2.Translate(context.Background(), translateRequest{Items: items}); err != nil || fake.calls.Load() != 1 {
		t.Fatalf("expected memory hit, calls=%d err=%v", fake.calls.Load(), err)
	}
	if usage.Snapshot().MemoryHits != 1+3 {
		t.Fatalf("memory hits = %d", usage.Snapshot().MemoryHits)
	}

	// Другой промпт — память не подмешивается.
	c3 := &cachedTranslator{inner: fake, tm: tm2, scope: memoryScope("m", "other prompt"), usage: usage}
	c3.Translate(context.Background(), translateRequest{Items: items})
	if fake.calls.Load() != 2 {
		t.Fatalf("prompt change must invalidate memory")
	}
}

func TestMemorySurvivesTruncatedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tm.jsonl")
	os.WriteFile(path, []byte("{\"k\":\"a\",\"t\":\"A\"}\n{\"k\":\"b\",\"t\":"), 0o600)
	tm, err := openTranslationMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := tm.Get("a"); !ok || v != "A" || tm.Len() != 1 {
		t.Fatalf("got %q %v len=%d", v, ok, tm.Len())
	}
	tm.Put("c", "C")
	tm.Close()
	tm2, _ := openTranslationMemory(path)
	defer tm2.Close()
	if v, ok := tm2.Get("c"); !ok || v != "C" {
		t.Fatalf("record after truncated line lost: %q %v", v, ok)
	}
}
