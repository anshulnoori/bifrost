package claude

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

type testLogger struct{ schemas.Logger }

func (testLogger) Debug(string, ...any) {}
func (testLogger) Warn(string, ...any)  {}
func (testLogger) Info(string, ...any)  {}
func (testLogger) Error(string, ...any) {}

type testIdentity struct{ schemas.Identity }

func (testIdentity) VirtualKey() *schemas.EntityRef {
	return &schemas.EntityRef{ID: "vk-owner"}
}

type testAccess struct{ schemas.Access }

func (testAccess) IsModelAllowed(provider, model string) bool {
	return provider == "claude" && model == "claude-sonnet-4-6"
}

type testGrant struct{ schemas.Grant }

func (testGrant) Identity() schemas.Identity { return testIdentity{} }
func (testGrant) Access() schemas.Access     { return testAccess{} }

func newContext() *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetGrant(testGrant{})
	ctx.SetValue(schemas.BifrostContextKeyIntegrationType, "anthropic")
	return ctx
}

func TestClaudeNativePassthroughAndIsolation(t *testing.T) {
	t.Setenv("CLAUDE_BRIDGE_TOKEN", "synthetic-bridge-token-for-tests-only")
	var received http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Clone()
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"41"}],"stop_reason":"end_turn","usage":{"input_tokens":37,"output_tokens":2}}`)
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL}, ClaudeAccount: func(*schemas.BifrostContext, schemas.Key, string) error { return nil }}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	req := &schemas.BifrostPassthroughRequest{Provider: schemas.Claude, Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages", Body: []byte(`{"model":"claude/claude-sonnet-4-6","max_tokens":731,"messages":[{"role":"user","content":"hi"}]}`), SafeHeaders: map[string]string{"Authorization": "stolen", "x-claude-bridge-owner": "forged", "anthropic-beta": "caller-beta", "anthropic-version": "2023-06-01"}}
	resp, failure := p.Passthrough(newContext(), schemas.Key{ID: "account-a"}, req)
	if failure != nil {
		t.Fatal(failure)
	}
	if !strings.Contains(string(resp.Body), `"text":"41"`) || resp.PassthroughUsage == nil || resp.PassthroughUsage.LLMUsage.PromptTokens != 37 {
		t.Fatalf("native response/usage missing: %+v", resp)
	}
	if received.Get("Authorization") != "" || received.Get("X-Claude-Bridge-Owner") != "vk-owner" || received.Get("X-Bifrost-Claude-Session-ID") != "" {
		t.Fatalf("unsafe bridge headers: %+v", received)
	}
	if received.Get("Anthropic-Beta") != "caller-beta" || received.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatal("native feature headers lost", received)
	}
	admitted, failure := p.admitted(newContext(), schemas.Key{ID: "account-a"}, req)
	if failure != nil || gjson.GetBytes(admitted.Body, "model").String() != req.Model {
		t.Fatalf("model not unprefixed: %+v", failure)
	}
	ctx := newContext()
	ctx.SetValue(schemas.BifrostContextKeyIntegrationType, "openai")
	if _, failure := p.Passthrough(ctx, schemas.Key{}, req); failure == nil {
		t.Fatal("OpenAI endpoint accepted")
	}
	ctx = schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyIntegrationType, "anthropic")
	if _, failure := p.Passthrough(ctx, schemas.Key{}, req); failure == nil {
		t.Fatal("request without admission accepted")
	}
	if _, failure := p.ChatCompletion(newContext(), schemas.Key{}, &schemas.BifrostChatRequest{}); failure == nil {
		t.Fatal("chat completions accepted")
	}
}

// default_request_timeout_in_seconds must not cap Claude requests: a long
// generation ends when the caller or Anthropic ends it. The bridge delays its
// response headers past the configured timeout on both the unary and stream paths.
func TestClaudeRequestsOutliveTheRequestTimeout(t *testing.T) {
	t.Setenv("CLAUDE_BRIDGE_TOKEN", "synthetic-bridge-token-for-tests-only")
	const delay = 1500 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		if gjson.GetBytes(mustRead(t, r), "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_slow","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{
		NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL, DefaultRequestTimeoutInSeconds: 1},
		ClaudeAccount: func(*schemas.BifrostContext, schemas.Key, string) error { return nil },
	}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	unary := &schemas.BifrostPassthroughRequest{Provider: schemas.Claude, Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages",
		Body: []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)}
	resp, failure := p.Passthrough(newContext(), schemas.Key{ID: "account-a"}, unary)
	if failure != nil {
		t.Fatalf("unary request was cut at the request timeout: %+v", failure.Error)
	}
	if resp.StatusCode != 200 || !strings.Contains(string(resp.Body), `"text":"done"`) {
		t.Fatalf("unexpected unary response: %d %s", resp.StatusCode, resp.Body)
	}

	stream := *unary
	stream.Body = []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	runner := func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return r, e
	}
	chunks, failure := p.PassthroughStream(newContext(), runner, func(context.Context) {}, schemas.Key{ID: "account-a"}, &stream)
	if failure != nil {
		t.Fatalf("stream was cut at the request timeout: %+v", failure.Error)
	}
	var body strings.Builder
	for chunk := range chunks {
		if chunk.BifrostError != nil {
			t.Fatalf("stream error: %+v", chunk.BifrostError.Error)
		}
		if chunk.BifrostPassthroughResponse != nil {
			body.Write(chunk.BifrostPassthroughResponse.Body)
		}
	}
	if !strings.Contains(body.String(), "message_stop") {
		t.Fatalf("stream body missing: %q", body.String())
	}
}

func mustRead(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestClaudeRejectsRemoteBridgeAndProviderOverrides(t *testing.T) {
	t.Setenv("CLAUDE_BRIDGE_TOKEN", "synthetic-bridge-token-for-tests-only")
	for _, base := range []string{"https://api.anthropic.com", "http://localhost:8091", "http://127.0.0.1:8091/path", "http://user@127.0.0.1:8091"} {
		if _, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: base}}, testLogger{}); err == nil {
			t.Fatalf("accepted %s", base)
		}
	}
	p, err := New(&schemas.ProviderConfig{}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []*schemas.BifrostPassthroughRequest{
		{Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages", UpstreamURL: "http://evil.invalid"},
		{Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages", RawQuery: "x=1"},
		{Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages/count_tokens"},
		{Model: "claude-unauthorized", Method: "POST", Path: "/v1/messages"},
		{Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages", Body: []byte(`{"messages":[]}`), SafeHeaders: map[string]string{"x-bifrost-claude-session-id": "session-fixture"}},
	} {
		if _, failure := p.admitted(newContext(), schemas.Key{ID: "account-a"}, req); failure == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
}

func TestClaudeReserveRejectionIsDistinctAndNeverReachesTheBridge(t *testing.T) {
	t.Setenv("CLAUDE_BRIDGE_TOKEN", "synthetic-bridge-token-for-tests-only")
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL},
		ClaudeAccount: func(*schemas.BifrostContext, schemas.Key, string) error { return schemas.ErrCodexReserve }}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	req := &schemas.BifrostPassthroughRequest{Provider: schemas.Claude, Model: "claude-sonnet-4-6", Method: "POST", Path: "/v1/messages", Body: []byte(`{"model":"claude-sonnet-4-6","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)}
	_, failure := p.Passthrough(newContext(), schemas.Key{ID: "account-a"}, req)
	if failure == nil || *failure.StatusCode != 403 || !strings.Contains(failure.Error.Message, "reserve") {
		t.Fatalf("reserve rejection: %+v", failure)
	}
	if called {
		t.Fatal("a reserved account must not reach the bridge")
	}
}
