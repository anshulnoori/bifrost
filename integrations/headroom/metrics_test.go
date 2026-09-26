package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

func TestEmbeddingProxyBoundary(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("X-Headroom-Proxy-Token") != "service-token" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Headroom-Project") != "" {
			t.Error("wrong route or forwarded caller credentials")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"headroom-minilm-v1","input":"hello"}` {
			t.Error("embedding payload changed")
		}
		w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer upstream.Close()
	previous := current.Swap(&bridge{config: Config{Enabled: true, Endpoint: upstream.URL, MaxBodyBytes: 1024}, client: upstream.Client(), token: "service-token"})
	defer current.Store(previous)
	handler := ledger.handler("local-token")
	for _, tc := range []struct {
		path, token, body string
		status            int
	}{
		{"/v1/embeddings", "wrong", `{}`, 401},
		{"/v1/embeddings?endpoint=evil", "local-token", `{}`, 405},
		{"/v1/tools", "local-token", `{}`, 405},
		{"/v1/embeddings", "local-token", strings.Repeat("x", 1025), 413},
		{"/v1/embeddings", "local-token", `{"model":"headroom-minilm-v1","input":"hello"}`, 200},
	} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("X-Headroom-Project", "untrusted")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s: got %d, want %d", tc.path, w.Code, tc.status)
		}
	}
	if calls != 1 {
		t.Fatalf("unexpected upstream calls: %d", calls)
	}
}

func TestEmbeddingProxySkipsLargeInputsBeforeAdmission(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	previous := current.Swap(&bridge{config: Config{EmbeddingProxyEnabled: true, Endpoint: upstream.URL, MaxBodyBytes: 4 << 20}, client: upstream.Client()})
	defer current.Store(previous)
	for _, tc := range []struct {
		input  string
		status int
	}{
		{`"` + strings.Repeat("a", 1024) + `"`, 200},
		{`"` + strings.Repeat("a", 1025) + `"`, 413},
		{`["short","` + strings.Repeat("é", 513) + `"]`, 413},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":`+tc.input+`}`))
		proxyEmbedding(w, r)
		if w.Code != tc.status {
			t.Fatalf("got %d, want %d", w.Code, tc.status)
		}
	}
	if calls != 1 {
		t.Fatalf("oversized input reached upstream: %d calls", calls)
	}
}

func TestEmbeddingProxyWithoutCompression(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
		w.Write([]byte(`{"data":[{"embedding":[1,0]}]}`))
	}))
	defer upstream.Close()
	t.Setenv("TEST_PROXY_TOKEN", strings.Repeat("p", 32))
	b, err := newBridge(Config{Enabled: false, EmbeddingProxyEnabled: true, Endpoint: upstream.URL, TokenEnv: "TEST_PROXY_TOKEN", MaxBodyBytes: 1024, TimeoutMS: 1})
	if err != nil {
		t.Fatal(err)
	}
	previous := current.Swap(b)
	defer current.Store(previous)
	r := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"hello"}`))
	r.Header.Set("Authorization", "Bearer local-token")
	w := httptest.NewRecorder()
	ledger.handler("local-token").ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("embedding facade unavailable with compression disabled: %d %s", w.Code, w.Body.String())
	}
}

func TestResponsesStreamUsage(t *testing.T) {
	response := &schemas.BifrostResponse{ResponsesStreamResponse: &schemas.BifrostResponsesStreamResponse{Type: schemas.ResponsesStreamResponseTypeCompleted}}
	if providerUsage(response) != nil {
		t.Fatal("missing usage must not invent accounting")
	}
	response.ResponsesStreamResponse.Response = &schemas.BifrostResponsesResponse{Usage: &schemas.ResponsesResponseUsage{InputTokens: 112, OutputTokens: 12, TotalTokens: 124}}
	usage := providerUsage(response)
	if gjson.GetBytes(usage, "input_tokens").Int() != 112 || gjson.GetBytes(usage, "output_tokens").Int() != 12 {
		t.Fatalf("stream accounting lost: %s", usage)
	}
}

func TestMonitorReload(t *testing.T) {
	t.Setenv("TEST_MONITOR_TOKEN", strings.Repeat("m", 32))
	config := Config{MetricsAddress: "127.0.0.1:0", MetricsTokenEnv: "TEST_MONITOR_TOKEN", RetentionSeconds: 60}
	if err := configureMonitor(config); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		monitorMu.Lock()
		defer monitorMu.Unlock()
		monitor.Close()
		monitor = nil
	})
	config.RetentionSeconds = 0
	if err := configureMonitor(config); err != nil {
		t.Fatal("unchanged listener must permit policy reload", err)
	}
	t.Setenv("TEST_MONITOR_TOKEN", strings.Repeat("n", 32))
	if err := configureMonitor(config); err == nil {
		t.Fatal("credential rotation must require restart")
	}
}

func TestMetricsPrivacyRetentionAndRestart(t *testing.T) {
	l := &eventLedger{retention: time.Minute, totals: map[string]uint64{}}
	for i := 0; i < 1005; i++ {
		l.add(&Event{Started: time.Now(), Status: "bypassed", Model: "sensitive-model", Thread: "sensitive-thread", Quality: "not_evaluated"})
	}
	if len(l.events) != 1000 {
		t.Fatal("unbounded storage", len(l.events))
	}
	req := httptest.NewRequest("GET", "/v1/events", nil)
	resp := httptest.NewRecorder()
	l.handler("token").ServeHTTP(resp, req)
	if resp.Code != 401 {
		t.Fatal("unauthenticated metrics")
	}
	req = httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp = httptest.NewRecorder()
	l.handler("token").ServeHTTP(resp, req)
	if strings.Contains(resp.Body.String(), "sensitive") || !strings.Contains(resp.Body.String(), `status="bypassed"} 1005`) {
		t.Fatal("unsafe labels or wrong counts")
	}
	l.prune(time.Now().Add(2 * time.Minute))
	if len(l.events) != 0 {
		t.Fatal("expired metadata retained")
	}
	fresh := &eventLedger{totals: map[string]uint64{}}
	req = httptest.NewRequest("GET", "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp = httptest.NewRecorder()
	fresh.handler("token").ServeHTTP(resp, req)
	if gjson.GetBytes(resp.Body.Bytes(), "events.#").Int() != 0 || gjson.GetBytes(resp.Body.Bytes(), "quality").Str != "not_evaluated" || gjson.GetBytes(resp.Body.Bytes(), "cost_savings").Type != gjson.Null {
		t.Fatal("restart invented measurements")
	}
	fresh.add(&Event{Status: "compressed"})
	if len(fresh.events) != 0 || fresh.totals["compressed"] != 1 {
		t.Fatal("zero-retention not honored")
	}
}
