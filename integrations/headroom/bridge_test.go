package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

func testBridge(t *testing.T, handler http.HandlerFunc) *bridge {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	t.Setenv("HEADROOM_TEST_TOKEN", strings.Repeat("t", 32))
	t.Setenv("HEADROOM_TEST_KEY", strings.Repeat("k", 32))
	b, err := newBridge(Config{Enabled: true, ProjectID: "project-a", Endpoint: s.URL, TokenEnv: "HEADROOM_TEST_TOKEN", ScopeKeyEnv: "HEADROOM_TEST_KEY", MinTextBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.client.CloseIdleConnections)
	return b
}

func goodReply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []map[string]string `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	for _, msg := range req.Messages {
		msg["content"] = "short"
	}
	json.NewEncoder(w).Encode(map[string]any{"messages": req.Messages, "tokens_before": 53, "tokens_after": 12, "ccr_hashes": []string{}, "obligations": []string{}})
}

func TestPrivateModalConfiguration(t *testing.T) {
	t.Setenv("PRIVATE_MODAL_ID", "test-id")
	t.Setenv("PRIVATE_MODAL_SECRET", "test-secret")
	t.Setenv("PRIVATE_SCOPE_KEY", strings.Repeat("k", 32))
	c := Config{Enabled: true, ModalApp: "bifrost-headroom", ModalEnvironment: "main",
		ModalKeyEnv: "PRIVATE_MODAL_ID", ModalSecretEnv: "PRIVATE_MODAL_SECRET", ScopeKeyEnv: "PRIVATE_SCOPE_KEY"}
	b, err := newBridge(c)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	if b.client != nil || b.config.Endpoint != "" || b.token != "" {
		t.Fatal("private SDK mode configured an HTTP endpoint or proxy credential")
	}
	c.Endpoint = "https://public.modal.run"
	if _, err := newBridge(c); err == nil {
		t.Fatal("accepted ambiguous private and HTTP transports")
	}
	c.Endpoint = ""
	t.Setenv("PRIVATE_MODAL_SECRET", "")
	if _, err := newBridge(c); err == nil {
		t.Fatal("accepted missing SDK credential")
	}
}

func TestModalProxyAuth(t *testing.T) {
	t.Setenv("MODAL_TEST_ID", "fixture-id")
	t.Setenv("MODAL_TEST_SECRET", "fixture-secret")
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Modal-Key") != "fixture-id" || r.Header.Get("Modal-Secret") != "fixture-secret" {
			t.Error("missing Modal proxy credentials")
		}
		goodReply(w, r)
	})
	c := b.config
	c.ModalKeyEnv, c.ModalSecretEnv = "MODAL_TEST_ID", "MODAL_TEST_SECRET"
	// Credentials must never travel over cleartext outside local fixtures.
	if _, err := newBridge(c); err == nil {
		t.Fatal("accepted Modal credentials over HTTP")
	}
	c.Endpoint = "https://service.example"
	m, err := newBridge(c)
	if err != nil {
		t.Fatal(err)
	}
	m.config.Endpoint = b.config.Endpoint
	defer m.client.CloseIdleConnections()
	if _, _, err := m.compress(context.Background(), "m", "s", []string{"original text"}); err != nil {
		t.Fatal(err)
	}
	c.ModalSecretEnv = "MISSING_MODAL_SECRET"
	if _, err := newBridge(c); err == nil {
		t.Fatal("accepted incomplete Modal credentials")
	}
}

func TestCompressContract(t *testing.T) {
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/compress" || r.Header.Get("X-Headroom-Proxy-Token") != strings.Repeat("t", 32) || r.Header.Get("Authorization") != "" {
			t.Error("wrong service auth or path")
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "gateway.can_redrive").Bool() || gjson.GetBytes(body, "gateway.session_affinity").Bool() || gjson.GetBytes(body, "config.session_id").Exists() {
			t.Error("unsafe capabilities")
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		goodReply(w, r)
	})
	out, est, err := b.compress(context.Background(), "gpt-4.1", b.scopeID("a", "bc"), []string{"0123456789abcdef"})
	if err != nil || !reflect.DeepEqual(out, []string{"short"}) || est.Before != 53 || est.After != 12 {
		t.Fatalf("%v %+v %v", out, est, err)
	}
	if b.scopeID("a", "bc") == b.scopeID("ab", "c") || b.scopeID("tenant-a", "project", "vk", "thread") == b.scopeID("tenant-b", "project", "vk", "thread") {
		t.Fatal("scope collision")
	}
	for i := 0; i < 6; i++ {
		parts := []string{"project", "principal", "session", "thread", "provider", "model"}
		other := append([]string(nil), parts...)
		other[i] += "-other"
		if b.scopeID(parts...) == b.scopeID(other...) {
			t.Fatal("scope omitted dimension", i)
		}
	}
}

func TestMalformedRepliesNeverCommit(t *testing.T) {
	cases := []string{
		`not json`, `{}`, `{"messages":[],"tokens_before":10,"tokens_after":2}`,
		`{"messages":[{"role":"tool","tool_call_id":"slot-0","content":"short"}],"tokens_before":10,"tokens_after":20}`,
		`{"messages":[{"role":"tool","tool_call_id":"slot-0","content":"short"}],"tokens_before":10,"tokens_after":2,"obligations":["redrive"]}`,
		`{"messages":[{"role":"tool","tool_call_id":"slot-0","content":"short"}],"tokens_before":10,"tokens_after":2,"ccr_hashes":["abc"]}`,
		`{"messages":[{"role":"assistant","tool_call_id":"slot-0","content":"short"}],"tokens_before":10,"tokens_after":2}`,
		`{"messages":[{"role":"tool","tool_call_id":"slot-1","content":"short"}],"tokens_before":10,"tokens_after":2}`,
		`{"messages":[{"role":"tool","tool_call_id":"slot-0","content":"<<ccr:123>>"}],"tokens_before":10,"tokens_after":2}`,
		`{"messages":[{"role":"tool","tool_call_id":"slot-0","content":""}],"tokens_before":10,"tokens_after":2}`,
	}
	for i, body := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
			if _, _, err := b.compress(context.Background(), "m", "s", []string{strings.Repeat("original", 20)}); err == nil {
				t.Fatal("accepted malformed output")
			}
		})
	}
}

func TestCancellationRedirectAndLimits(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := b.compress(ctx, "m", "s", []string{"abcdefghijk"}); err == nil {
			t.Fatal("ignored cancellation")
		}
	})
	t.Run("redirect", func(t *testing.T) {
		b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1:1", 307) })
		if _, _, err := b.compress(context.Background(), "m", "s", []string{"abcdefghijk"}); err == nil {
			t.Fatal("followed redirect")
		}
	})
	t.Run("limit", func(t *testing.T) {
		b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", 1100)) })
		b.config.MaxBodyBytes = 1024
		if _, _, err := b.compress(context.Background(), "m", "s", []string{"abcdefghijk"}); err == nil {
			t.Fatal("accepted oversized response")
		}
	})
	if _, err := newBridge(Config{CCR: true}); err == nil {
		t.Fatal("enabled unsupported CCR")
	}
}

// Embedded interfaces keep test doubles limited to the grant methods consumed by
// the integration, without inventing a second authorization implementation.
type testIdentity struct {
	schemas.Identity
	project, principal string
}

func (i testIdentity) Project() *schemas.EntityRef    { return &schemas.EntityRef{ID: i.project} }
func (i testIdentity) VirtualKey() *schemas.EntityRef { return &schemas.EntityRef{ID: i.principal} }

type testGrant struct {
	schemas.Grant
	id schemas.Identity
}

func (g testGrant) Identity() schemas.Identity { return g.id }
func (g testGrant) Access() schemas.Access     { return testAccess{} }
func (g testGrant) Limits() schemas.Limits     { return testLimits{} }

type testAccess struct{ schemas.Access }
type testLimits struct{ schemas.Limits }

func admitted(project, principal string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetGrant(testGrant{id: testIdentity{project: project, principal: principal}})
	ctx.SetValue(schemas.BifrostContextKeySessionID, "session-1")
	ctx.SetValue(threadKey, "thread-1")
	return ctx
}
func chatRequest() *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-4.1", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleTool, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("original long tool output")}, ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: schemas.Ptr("call-42")}}}}}
}

func TestAdmissionIsolationAndCopyOnWrite(t *testing.T) {
	var calls atomic.Int32
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodReply(w, r) })
	for _, ctx := range []*schemas.BifrostContext{schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), admitted("project-b", "vk-a")} {
		req := chatRequest()
		out, sc, err := b.pre(ctx, req)
		if out != req || sc != nil || err != nil {
			t.Fatal("bypass modified request")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("disclosed data before project admission")
	}
	req := chatRequest()
	out, sc, err := b.pre(admitted("project-a", "vk-a"), req)
	if sc != nil || err != nil || *out.ChatRequest.Input[0].Content.ContentStr != "short" {
		t.Fatal("did not compress", err)
	}
	if *req.ChatRequest.Input[0].Content.ContentStr != "original long tool output" || out.ChatRequest == req.ChatRequest {
		t.Fatal("mutated fallback source")
	}
	if out.ChatRequest.Provider != req.ChatRequest.Provider || out.ChatRequest.Model != req.ChatRequest.Model || *out.ChatRequest.Input[0].ToolCallID != "call-42" {
		t.Fatal("changed routing or tools")
	}
}

func TestVirtualKeyScopeWithoutProject(t *testing.T) {
	var calls atomic.Int32
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodReply(w, r) })
	b.config.ProjectID = ""
	b.config.VirtualKeyID = "vk-a"
	for _, principal := range []string{"vk-b", "vk-a"} {
		ctx := admitted("", principal)
		b.pre(ctx, chatRequest())
		if got := ctx.Value(eventKey).(*Event).Status; (principal == "vk-a") != (got == "compressed") {
			t.Fatal("virtual key isolation failed", principal, got)
		}
	}
	b.config.ProjectID = "required-project"
	b.pre(admitted("other-project", "vk-a"), chatRequest())
	if calls.Load() != 1 {
		t.Fatal("sidecar saw unauthorized scope")
	}
}

func TestFailPolicyAndStreamIdentity(t *testing.T) {
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private server detail", 500) })
	req := chatRequest()
	ctx := admitted("project-a", "vk-a")
	if out, sc, _ := b.pre(ctx, req); out != req || sc != nil {
		t.Fatal("fail-open modified original")
	}
	b.config.FailurePolicy = "closed"
	if _, sc, _ := b.pre(admitted("project-a", "vk-a"), req); sc == nil || sc.Error.AllowFallbacks == nil || *sc.Error.AllowFallbacks {
		t.Fatal("failure bypasses closed policy")
	}
	chunk := &schemas.BifrostStreamChunk{}
	if got, err := HTTPTransportStreamChunkHook(ctx, nil, chunk); got != chunk || err != nil {
		t.Fatal("changed SSE chunk")
	}
}

func TestUsageNotReplacedByEstimate(t *testing.T) {
	ctx := admitted("project-a", "vk-a")
	e := &Event{Started: time.Now(), Status: "compressed", Quality: "not_evaluated", Estimate: &estimate{Before: 1000, After: 100}}
	ctx.SetValue(eventKey, e)
	usage := &schemas.BifrostLLMUsage{PromptTokens: 317, CompletionTokens: 29, TotalTokens: 346, PromptTokensDetails: &schemas.ChatPromptTokensDetails{CachedReadTokens: 211, CachedWriteTokens: 7}}
	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{Usage: usage}}
	got, err, _ := PostLLMHook(ctx, resp, nil)
	if got != resp || err != nil || got.ChatResponse.Usage != usage || gjson.GetBytes(e.Usage, "prompt_tokens").Int() != 317 {
		t.Fatal("provider accounting was overwritten")
	}
	if !e.Done.Load() {
		t.Fatal("missing terminal event")
	}
}

func TestPartialStreamFailureFinalizesEvent(t *testing.T) {
	ctx := admitted("project-a", "vk-a")
	event := &Event{Started: time.Now(), Status: "compressed"}
	ctx.SetValue(eventKey, event)
	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionStreamRequest}}}
	PostLLMHook(ctx, resp, nil)
	if event.Done.Load() {
		t.Fatal("partial chunk finalized accounting")
	}
	failure := &schemas.BifrostError{ExtraFields: schemas.BifrostErrorExtraFields{RequestType: schemas.ChatCompletionStreamRequest}}
	_, got, _ := PostLLMHook(ctx, nil, failure)
	if got != failure || !event.Done.Load() || !event.ProviderFailed {
		t.Fatal("partial-stream error was not accounted")
	}
}

// Opt-in local service benchmark. No provider call, no billed-token or answer-
// quality claim. Paired baseline/bridge paths consume identical deterministic data.
func TestLiveHeadroomFixture(t *testing.T) {
	endpoint := os.Getenv("HEADROOM_BENCH_URL")
	if endpoint == "" {
		t.Skip("set HEADROOM_BENCH_URL and HEADROOM_BENCH_TOKEN to run the pinned local service fixture")
	}
	t.Setenv("HEADROOM_BENCH_SCOPE", strings.Repeat("b", 32))
	b, err := newBridge(Config{Enabled: true, ProjectID: "project-a", Endpoint: endpoint, TokenEnv: "HEADROOM_BENCH_TOKEN", ScopeKeyEnv: "HEADROOM_BENCH_SCOPE", TimeoutMS: 30000})
	if err != nil {
		t.Fatal(err)
	}
	defer b.client.CloseIdleConnections()
	var fixture strings.Builder
	for i := 0; i < 250; i++ {
		level := "INFO"
		message := "request completed successfully"
		if i == 167 {
			level = "FATAL"
			message = "transaction=TX-731 amount=1949.37 failed integrity check"
		}
		fmt.Fprintf(&fixture, "2026-09-22T10:%02d:%02dZ %s worker=%d %s\n", i/60, i%60, level, i%7, message)
	}
	text := fixture.String()
	req := chatRequest()
	req.ChatRequest.Input[0].Content.ContentStr = &text
	baseline := *b
	baseline.config.Enabled = false
	start := time.Now()
	plain, _, _ := baseline.pre(admitted("project-a", "vk-a"), req)
	baselineUS := time.Since(start).Microseconds()
	ctx := admitted("project-a", "vk-a")
	start = time.Now()
	out, sc, err := b.pre(ctx, req)
	compressionUS := time.Since(start).Microseconds()
	e := ctx.Value(eventKey).(*Event)
	if err != nil || sc != nil || e.Status == "failed" {
		t.Fatalf("real service contract failed: event=%+v err=%v", e, err)
	}
	got := *out.ChatRequest.Input[0].Content.ContentStr
	sentinel := "transaction=TX-731 amount=1949.37 failed integrity check"
	if !strings.Contains(*plain.ChatRequest.Input[0].Content.ContentStr, sentinel) || !strings.Contains(got, sentinel) {
		t.Fatal("fixture critical fact lost")
	}
	counts, _ := json.Marshal(e.Estimate)
	t.Logf("fixture=logs250 baseline_us=%d headroom_us=%d bytes_before=%d bytes_after=%d status=%s estimates=%s critical_fact_preserved=true provider_usage=null answer_quality=not_evaluated ccr=unsupported", baselineUS, compressionUS, len(text), len(got), e.Status, counts)
}
