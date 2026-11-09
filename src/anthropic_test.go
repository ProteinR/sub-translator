package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type res struct{ id, lang, tr string }

func toolResponse(stop string, results ...res) string {
	list := []map[string]string{}
	for _, r := range results {
		list = append(list, map[string]string{"id": r.id, "lang": r.lang, "translation": r.tr})
	}
	input, _ := json.Marshal(map[string]any{"results": list})
	return fmt.Sprintf(`{"content":[{"type":"tool_use","name":"submit_translations","input":%s}],"stop_reason":%q,"usage":{"input_tokens":1000,"output_tokens":500,"cache_read_input_tokens":200}}`, input, stop)
}

func testClient(url string, usage *usageTracker) *anthropicClient {
	c := newAnthropicClient("sk-ant-test-key-123456", "claude-haiku-5-5", "claude-sonnet-5-5", "style", usage)
	c.endpoint = url
	c.maxRetries = 4
	c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return c
}

func cell(id, text, lang string) TranslationItem {
	return TranslationItem{ID: id, Original: text, SourceLang: "English", TargetLang: lang, TargetID: lang}
}

// Две строки × два языка = 4 ячейки.
var multiItems = []TranslationItem{
	cell("1", "Hello there", "Polish"), cell("1", "Hello there", "Russian"),
	cell("2", "Good morning", "Polish"), cell("2", "Good morning", "Russian"),
}

func decodeReq(t *testing.T, r *http.Request) messagesRequest {
	t.Helper()
	var req messagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestAllLanguagesInOneRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("x-api-key") == "" || strings.Contains(r.URL.RawQuery, "key") {
			t.Errorf("key must be sent as header only")
		}
		req := decodeReq(t, r)
		user := req.Messages[0].Content
		if strings.Count(user, "Hello there") != 1 {
			t.Errorf("source text must be sent once for all languages:\n%s", user)
		}
		if !strings.Contains(user, "Target languages: Polish, Russian") || strings.Contains(user, `"to"`) {
			t.Errorf("unexpected prompt:\n%s", user)
		}
		if strings.Contains(user, "\n  ") {
			t.Errorf("items JSON must be compact")
		}
		if req.System[0].CacheControl == nil {
			t.Errorf("system prompt must carry cache_control")
		}
		fmt.Fprint(w, toolResponse("tool_use",
			res{"1", "Polish", "Cześć"}, res{"1", "Russian", "Привет"},
			res{"2", "Polish", "Dzień dobry"}, res{"2", "Russian", "Доброе утро"}))
	}))
	defer srv.Close()

	got, err := testClient(srv.URL, nil).Translate(context.Background(), translateRequest{Items: multiItems})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || got[1].Translation != "Привет" || got[2].Translation != "Dzień dobry" || got[0].Original != "Hello there" {
		t.Fatalf("calls=%d got=%+v", calls.Load(), got)
	}
}

func TestPartialRetryOnlyResendsMissing(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := decodeReq(t, r).Messages[0].Content
		if calls.Add(1) == 1 {
			// Пропущена ячейка 2/Russian.
			fmt.Fprint(w, toolResponse("tool_use", res{"1", "Polish", "Cześć"}, res{"1", "Russian", "Привет"}, res{"2", "Polish", "Dzień dobry"}))
			return
		}
		if strings.Contains(user, `"id":"1"`) {
			t.Errorf("accepted row 1 must not be re-sent:\n%s", user)
		}
		if !strings.Contains(user, `"to":["Russian"]`) && !strings.Contains(user, "Target languages: Russian") {
			t.Errorf("retry must ask only for missing language:\n%s", user)
		}
		fmt.Fprint(w, toolResponse("tool_use", res{"2", "Russian", "Доброе утро"}))
	}))
	defer srv.Close()

	got, err := testClient(srv.URL, nil).Translate(context.Background(), translateRequest{Items: multiItems})
	if err != nil || calls.Load() != 2 || got[3].Translation != "Доброе утро" {
		t.Fatalf("calls=%d err=%v got=%+v", calls.Load(), err, got)
	}
}

func TestValidationRejectsLostPlaceholder(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, toolResponse("tool_use", res{"1", "Polish", "Witaj"}))
			return
		}
		fmt.Fprint(w, toolResponse("tool_use", res{"1", "Polish", "Witaj, {name}"}))
	}))
	defer srv.Close()

	items := []TranslationItem{cell("1", "Hello, {name}", "Polish")}
	got, err := testClient(srv.URL, nil).Translate(context.Background(), translateRequest{Items: items})
	if err != nil || got[0].Translation != "Witaj, {name}" || calls.Load() != 2 {
		t.Fatalf("calls=%d err=%v got=%+v", calls.Load(), err, got)
	}
}

func TestFallbackModelAfterOverload(t *testing.T) {
	var mu sync.Mutex
	var models []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := decodeReq(t, r)
		mu.Lock()
		models = append(models, req.Model)
		mu.Unlock()
		if req.Model == "claude-haiku-5-5" {
			w.WriteHeader(529)
			fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		fmt.Fprint(w, toolResponse("tool_use", res{"1", "Polish", "Cześć"}))
	}))
	defer srv.Close()

	items := []TranslationItem{cell("1", "Hi", "Polish")}
	if _, err := testClient(srv.URL, nil).Translate(context.Background(), translateRequest{Items: items}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(models, ",") != "claude-haiku-5-5,claude-haiku-5-5,claude-sonnet-5-5" {
		t.Fatalf("unexpected model sequence: %v", models)
	}
}

func TestNoRetryOn400(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)
	}))
	defer srv.Close()

	_, err := testClient(srv.URL, nil).Translate(context.Background(), translateRequest{Items: multiItems})
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestSplitsOnMaxTokensAtRowBoundary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := decodeReq(t, r).Messages[0].Content
		has1, has2 := strings.Contains(user, `"id":"1"`), strings.Contains(user, `"id":"2"`)
		switch {
		case has1 && has2:
			fmt.Fprint(w, toolResponse("max_tokens"))
		case has1:
			fmt.Fprint(w, toolResponse("tool_use", res{"1", "Polish", "a"}, res{"1", "Russian", "b"}))
		default:
			fmt.Fprint(w, toolResponse("tool_use", res{"2", "Polish", "c"}, res{"2", "Russian", "d"}))
		}
	}))
	defer srv.Close()

	got, err := testClient(srv.URL, nil).Translate(context.Background(), translateRequest{Items: multiItems})
	if err != nil || got[3].Translation != "d" {
		t.Fatalf("split failed: %+v %v", got, err)
	}
}

func TestContextIsSentButNotTranslated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := decodeReq(t, r).Messages[0].Content
		if !strings.Contains(user, "<context>") || !strings.Contains(user, "Zrobiłam to") {
			t.Errorf("context missing:\n%s", user)
		}
		fmt.Fprint(w, toolResponse("tool_use", res{"1", "Polish", "Cześć"}))
	}))
	defer srv.Close()

	req := translateRequest{
		Items:   []TranslationItem{cell("1", "Hi", "Polish")},
		Context: []contextRow{{Text: "I did it", Translations: map[string]string{"Polish": "Zrobiłam to"}}},
	}
	if _, err := testClient(srv.URL, nil).Translate(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetStopsRequests(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, toolResponse("tool_use", res{"1", "Polish", "Cześć"}))
	}))
	defer srv.Close()

	usage := newUsageTracker(0.0001, nil)
	c := testClient(srv.URL, usage)
	items := []TranslationItem{cell("1", "Hi", "Polish")}
	if _, err := c.Translate(context.Background(), translateRequest{Items: items}); err != nil {
		t.Fatal(err)
	}
	// 1000 in × $0.10 + 500 out × $0.50 + 200 cache read × $0.01 за 1M токенов
	if s := usage.Snapshot(); s.CostUSD < 0.000351 || s.CostUSD > 0.000353 {
		t.Fatalf("cost = %v", s.CostUSD)
	}
	if _, err := c.Translate(context.Background(), translateRequest{Items: items}); !errors.Is(err, errBudgetExceeded) {
		t.Fatalf("expected budget error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("request after budget exceeded")
	}
}

func TestHonorsCancel(t *testing.T) {
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
	_, err := testClient(srv.URL, nil).Translate(ctx, translateRequest{Items: multiItems})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancel not honored: %v after %v", err, time.Since(start))
	}
}

func TestSplitBatchesKeepsRowsTogether(t *testing.T) {
	items := []TranslationItem{
		cell("1", strings.Repeat("a", 30), "PL"), cell("1", strings.Repeat("a", 30), "RU"), // вес 60
		cell("2", strings.Repeat("b", 30), "PL"), cell("2", strings.Repeat("b", 30), "RU"), // вес 60
		cell("3", strings.Repeat("c", 200), "PL"), // больше лимита — отдельной пачкой
		cell("4", "d", "PL"), cell("5", "e", "PL"), cell("6", "f", "PL"),
	}
	var sizes []int
	for _, b := range splitBatches(items, 100, 2) {
		sizes = append(sizes, len(b))
	}
	if fmt.Sprint(sizes) != "[2 2 1 2 1]" {
		t.Fatalf("unexpected batching: %v", sizes)
	}
}

func TestContextFromTakesLastRows(t *testing.T) {
	items := []TranslationItem{
		{ID: "1", Original: "a", TargetLang: "PL", Translation: "a1"},
		{ID: "2", Original: "b", TargetLang: "PL", Translation: "b1"}, {ID: "2", Original: "b", TargetLang: "RU", Translation: "b2"},
		{ID: "3", Original: "c", TargetLang: "PL", Translation: "c1"},
	}
	rows := contextFrom(items, 2)
	if len(rows) != 2 || rows[0].Text != "b" || rows[0].Translations["RU"] != "b2" || rows[1].Text != "c" {
		t.Fatalf("got %+v", rows)
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("retry-after", "7")
	if d := parseRetryAfter(h); d != 7*time.Second {
		t.Fatalf("got %v", d)
	}
}
