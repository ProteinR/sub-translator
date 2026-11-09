package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(url string) *anthropicClient {
	c := newAnthropicClient("sk-ant-test-key-123456", "claude-haiku-5-5")
	c.endpoint = url
	c.maxRetries = 3
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return c
}

func toolResponse(results map[string]string, stop string) string {
	var list []map[string]string
	for id, tr := range results {
		list = append(list, map[string]string{"id": id, "translation": tr})
	}
	input, _ := json.Marshal(map[string]any{"results": list})
	return fmt.Sprintf(`{"content":[{"type":"tool_use","name":"submit_translations","input":%s}],"stop_reason":%q,"usage":{"input_tokens":10,"output_tokens":5}}`, input, stop)
}

var testItems = []TranslationItem{{ID: "1", Original: "Hello"}, {ID: "2", Original: "World"}}
var testCfg = Config{Prompt: "style", SourceLang: "English", TranslateToLang: "Polish"}

func TestAnthropicSuccessAndHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant-test-key-123456" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing auth headers")
		}
		if strings.Contains(r.URL.RawQuery, "key") {
			t.Errorf("API key must not be in URL")
		}
		var req messagesRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if req.ToolChoice.Name != translationToolName || req.Model != "claude-haiku-5-5" {
			t.Errorf("bad request: %+v", req)
		}
		if !strings.Contains(req.Messages[0].Content, "English into Polish") {
			t.Errorf("direction missing from prompt")
		}
		fmt.Fprint(w, toolResponse(map[string]string{"1": "Cześć", "2": "Świat"}, "tool_use"))
	}))
	defer srv.Close()

	got, err := testClient(srv.URL).Translate(context.Background(), testItems, testCfg)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Translation != "Cześć" || got[1].Translation != "Świat" || got[0].Original != "Hello" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestAnthropicRetriesOn429ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("retry-after", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		fmt.Fprint(w, toolResponse(map[string]string{"1": "a", "2": "b"}, "tool_use"))
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).Translate(context.Background(), testItems, testCfg); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
}

func TestAnthropicNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).Translate(context.Background(), testItems, testCfg)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("expected 400 apiError, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("400 must not be retried, got %d calls", calls.Load())
	}
}

func TestAnthropicRetriesBadOutput(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, toolResponse(map[string]string{"1": "only one"}, "tool_use")) // пропущен id 2
			return
		}
		fmt.Fprint(w, toolResponse(map[string]string{"1": "a", "2": "b"}, "tool_use"))
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).Translate(context.Background(), testItems, testCfg); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected retry after bad output, got %d calls", calls.Load())
	}
}

func TestAnthropicSplitsOnMaxTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req messagesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		content := req.Messages[0].Content
		has1, has2 := strings.Contains(content, `"id": "1"`), strings.Contains(content, `"id": "2"`)
		switch {
		case has1 && has2:
			fmt.Fprint(w, toolResponse(map[string]string{"1": "a"}, "max_tokens"))
		case has1:
			fmt.Fprint(w, toolResponse(map[string]string{"1": "a"}, "tool_use"))
		default:
			fmt.Fprint(w, toolResponse(map[string]string{"2": "b"}, "tool_use"))
		}
	}))
	defer srv.Close()

	got, err := testClient(srv.URL).Translate(context.Background(), testItems, testCfg)
	if err != nil || len(got) != 2 || got[1].Translation != "b" {
		t.Fatalf("split failed: %+v %v", got, err)
	}
}

func TestAnthropicHonorsCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release) // отпускаем хэндлер до srv.Close

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := testClient(srv.URL).Translate(ctx, testItems, testCfg)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel not honored: %v after %v", err, time.Since(start))
	}
}

func TestSplitBatches(t *testing.T) {
	items := []TranslationItem{
		{ID: "1", Original: strings.Repeat("a", 60)},
		{ID: "2", Original: strings.Repeat("b", 60)},
		{ID: "3", Original: strings.Repeat("c", 200)}, // длиннее лимита — отдельной пачкой
		{ID: "4", Original: "d"},
		{ID: "5", Original: "e"},
	}
	got := splitBatches(items, 100, 2)
	var sizes []int
	for _, b := range got {
		sizes = append(sizes, len(b))
	}
	if fmt.Sprint(sizes) != "[1 1 1 2]" {
		t.Fatalf("unexpected batching: %v", sizes)
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after", "7")
	if d := parseRetryAfter(h); d != 7*time.Second {
		t.Fatalf("got %v", d)
	}
}
