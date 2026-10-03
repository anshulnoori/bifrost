package claude

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
