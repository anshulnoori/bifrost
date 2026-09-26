package codex

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

func (testIdentity) Presented() bool { return true }

type testAccess struct{ schemas.Access }

func (testAccess) IsProviderAllowed(p string) bool              { return p == "codex" }
func (testAccess) KeysForModel(string, string) ([]string, bool) { return nil, false }
func (testAccess) IsModelAllowed(p, model string) bool {
	return p == "codex" && (model == "" || model == "gpt-5.3-codex")
}

type testGrant struct{ schemas.Grant }

func (testGrant) Identity() schemas.Identity { return testIdentity{} }
func (testGrant) Access() schemas.Access     { return testAccess{} }

type restrictedAccess struct{ testAccess }

func (restrictedAccess) KeysForModel(string, string) ([]string, bool) {
	return []string{"account-A"}, true
}

type restrictedGrant struct{ testGrant }

func (restrictedGrant) Access() schemas.Access { return restrictedAccess{} }

func TestAccountSelectionCannotBypassGrant(t *testing.T) {
	var selected string
	p, err := New(&schemas.ProviderConfig{CodexCredential: func(_ *schemas.BifrostContext, key schemas.Key) (string, string, error) {
		selected = key.ID
		return "access", "account", nil
	}}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetGrant(restrictedGrant{})
	if _, err := p.auth(ctx, schemas.Key{ID: "account-B"}, "gpt-5.3-codex"); err == nil || selected != "" {
		t.Fatal("resolved an unpermitted account")
	}
	if _, err := p.auth(ctx, schemas.Key{ID: "account-A"}, "gpt-5.3-codex"); err != nil || selected != "account-A" {
		t.Fatal("allowed account was not selected")
	}
}

func admitted(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	if !ctx.SetGrant(testGrant{}) {
		t.Fatal("failed to set grant")
	}
	return ctx
}

const terminal = `{"type":"response.completed","sequence_number":4,"response":{"id":"resp_fixture","object":"response","created_at":123,"model":"gpt-5.3-codex","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_7","name":"weather","arguments":"{\"city\":\"Oslo\"}","status":"completed"}],"usage":{"input_tokens":123,"output_tokens":17,"total_tokens":140,"input_tokens_details":{"cached_tokens":31},"output_tokens_details":{"reasoning_tokens":9}}}}`

func TestResponsesPreservesCodexErrorDetail(t *testing.T) {
	for _, tc := range []struct{ body, message string }{
		{`{"detail":"This model is not supported for this account."}`, "This model is not supported for this account."},
		{`{"error":{"message":"Standard error"},"detail":"Other detail"}`, "Standard error"},
		{`{"detail":{"internal":"not a public message"}}`, "provider API error (status 400)"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(400)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) { return "access", "account", nil }}, testLogger{})
			if err != nil {
				t.Fatal(err)
			}
			p.url = server.URL
			_, failure := p.Responses(admitted(t), schemas.Key{}, &schemas.BifrostResponsesRequest{Provider: schemas.Codex, Model: "gpt-5.3-codex", Input: []schemas.ResponsesMessage{}})
			if failure == nil || failure.StatusCode == nil || *failure.StatusCode != 400 || failure.Error == nil || failure.Error.Message != tc.message {
				t.Fatalf("unexpected upstream error: %+v", failure)
			}
		})
	}
}

func TestUnaryCollectsItemsWhenTerminalOutputIsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"TX-731 1949.37\"}],\"future\":42}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"r1\",\"summary\":[]}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"output\":[],\"usage\":{\"input_tokens\":24,\"output_tokens\":12,\"total_tokens\":36}}}\n\n")
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) { return "access", "account", nil }}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	p.url = server.URL
	response, bfErr := p.Responses(admitted(t), schemas.Key{}, &schemas.BifrostResponsesRequest{Model: "gpt-5.3-codex", Input: []schemas.ResponsesMessage{{Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hi")}}}})
	if bfErr != nil {
		t.Fatal(bfErr)
	}
	wire, _ := schemas.MarshalSorted(response)
	if gjson.GetBytes(wire, "output.1.content.0.text").String() != "TX-731 1949.37" || gjson.GetBytes(wire, "output.0.id").String() != "r1" {
		t.Fatalf("missing/unsorted completed items: %s", wire)
	}
	chat := response.ToBifrostChatResponse()
	wire, _ = schemas.MarshalSorted(chat)
	if !strings.Contains(string(wire), "TX-731 1949.37") {
		t.Fatalf("missing Chat answer: %s", wire)
	}
	raw, bfErr := p.Passthrough(admitted(t), schemas.Key{}, &schemas.BifrostPassthroughRequest{Method: "POST", Path: "/responses", Model: "gpt-5.3-codex", Body: []byte(`{"model":"gpt-5.3-codex","input":"hi"}`)})
	if bfErr != nil {
		t.Fatal(bfErr)
	}
	if gjson.GetBytes(raw.Body, "output.1.future").Int() != 42 || gjson.GetBytes(raw.Body, "output.1.content.0.text").String() != "TX-731 1949.37" {
		t.Fatalf("raw items lost: %s", raw.Body)
	}
}

func TestResponsesUsesSubscriptionAndPreservesTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer owner-access" || r.Header.Get("ChatGPT-Account-ID") != "owner-account" {
			t.Error("wrong credential or route")
		}
		body, _ := io.ReadAll(r.Body)
		if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "store").Bool() {
			t.Error("wrong subscription protocol")
		}
		if gjson.GetBytes(body, "reasoning.effort").String() != "high" {
			t.Error("reasoning lost")
		}
		if gjson.GetBytes(body, "prompt_cache_key").String() != "caller-session" || gjson.GetBytes(body, "prompt_cache_retention").String() != "24h" {
			t.Error("prompt cache isolation hints lost")
		}
		if gjson.GetBytes(body, "prompt_cache_options.mode").String() != "implicit" || gjson.GetBytes(body, "prompt_cache_options.ttl").String() != "30m" {
			t.Error("prompt cache options lost")
		}
		if gjson.GetBytes(body, "text.format.name").String() != "weather_result" || !gjson.GetBytes(body, "text.format.strict").Bool() || gjson.GetBytes(body, "text.format.schema.required.0").String() != "city" {
			t.Error("structured output schema lost")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", terminal)
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) {
		return "owner-access", "owner-account", nil
	}}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	p.url = server.URL
	ctx := admitted(t)
	request := &schemas.BifrostResponsesRequest{Provider: schemas.Codex, Model: "gpt-5.3-codex", Params: &schemas.ResponsesParameters{
		Reasoning:            &schemas.ResponsesParametersReasoning{Effort: schemas.Ptr("high")},
		PromptCacheKey:       schemas.Ptr("caller-session"),
		PromptCacheRetention: schemas.Ptr("24h"),
		PromptCacheOptions:   &schemas.PromptCacheOptions{Mode: schemas.Ptr("implicit"), TTL: schemas.Ptr("30m")},
	}}
	request.Params.Text = &schemas.ResponsesTextConfig{Format: &schemas.ResponsesTextConfigFormat{Type: "json_schema", Name: schemas.Ptr("weather_result"), Strict: schemas.Ptr(true), JSONSchema: &schemas.ResponsesTextConfigFormatJSONSchema{Type: schemas.Ptr("object"), Required: []string{"city"}}}}
	request.Input = []schemas.ResponsesMessage{{Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("Weather in Oslo?")}}}
	response, bfErr := p.Responses(ctx, schemas.Key{}, request)
	if bfErr != nil {
		t.Fatalf("responses failed: %+v", bfErr)
	}
	if response == nil || len(response.Output) != 1 || response.Usage == nil {
		t.Fatal("missing terminal output or usage")
	}
	wire, err := schemas.MarshalSorted(response)
	if err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{"output.0.call_id": "call_7", "output.0.name": "weather", "output.0.arguments": `{"city":"Oslo"}`, "usage.input_tokens": "123", "usage.output_tokens": "17"} {
		if gjson.GetBytes(wire, path).String() != expected {
			t.Errorf("%s lost: %s", path, wire)
		}
	}
	if request.Params.Store != nil || request.Params.Instructions != nil {
		t.Fatal("caller parameters mutated")
	}
}

func TestPassthroughPreservesOpaqueFieldsAndIsolatesHeaders(t *testing.T) {
	p, err := New(&schemas.ProviderConfig{CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) { return "resolved", "account", nil }}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gpt-5.3-codex","input":[{"type":"reasoning","encrypted_content":"opaque"}],"future":{"z":7,"a":3},"stream":true}`)
	req := &schemas.BifrostPassthroughRequest{Method: "POST", Path: "/responses", Model: "gpt-5.3-codex", Body: body, UpstreamURL: "https://attacker.invalid", SafeHeaders: map[string]string{"Authorization": "Bearer attacker", "ChatGPT-Account-ID": "victim"}}
	r, bfErr := p.passthrough(admitted(t), schemas.Key{}, req)
	if bfErr != nil {
		t.Fatal(bfErr)
	}
	if r.UpstreamURL != baseURL || r.SafeHeaders["Authorization"] != "Bearer resolved" || r.SafeHeaders["ChatGPT-Account-ID"] != "account" {
		t.Fatal("caller controlled upstream auth")
	}
	if !strings.Contains(string(r.Body), `"future":{"z":7,"a":3}`) || !strings.Contains(string(r.Body), `"encrypted_content":"opaque"`) {
		t.Fatal("opaque request fields lost")
	}
	if req.SafeHeaders["Authorization"] != "Bearer attacker" || string(req.Body) != string(body) {
		t.Fatal("caller request mutated")
	}
}

func TestUnsupportedAndUnadmittedRequestsFailClosed(t *testing.T) {
	for _, body := range []string{`{"model":"gpt-5.3-codex","model":"other"}`, `{"model":"gpt-5.3-codex","store":true}`, `{"model":"gpt-5.3-codex","max_output_tokens":200}`, `{"model":"other"}`, `{"model":"gpt-5.3-codex","background":true}`} {
		if _, err := prepare([]byte(body), "gpt-5.3-codex"); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	called := false
	p, err := New(&schemas.ProviderConfig{CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) {
		called = true
		return "token", "account", nil
	}}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if _, bfErr := p.auth(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), schemas.Key{}, "gpt-5.3-codex"); bfErr == nil || called {
		t.Fatal("credential resolved without admission")
	}
	if _, bfErr := p.Embedding(admitted(t), schemas.Key{}, &schemas.BifrostEmbeddingRequest{}); bfErr == nil {
		t.Fatal("unsupported operation reached network")
	}
}

func TestExtraParamsCannotOverrideAdmittedRequest(t *testing.T) {
	for _, extra := range []map[string]any{{"store": true}, {"model": "unadmitted-model"}, {"temperature": 0.5}} {
		p, err := New(&schemas.ProviderConfig{CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) { return "token", "account", nil }}, testLogger{})
		if err != nil {
			t.Fatal(err)
		}
		p.url = "http://127.0.0.1:1"
		ctx := admitted(t)
		ctx.SetValue(schemas.BifrostContextKeyPassthroughExtraParams, true)
		_, bfErr := p.Responses(ctx, schemas.Key{}, &schemas.BifrostResponsesRequest{Model: "gpt-5.3-codex", Input: []schemas.ResponsesMessage{{Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hello")}}}, Params: &schemas.ResponsesParameters{ExtraParams: extra}})
		if bfErr == nil || bfErr.Error == nil || !(strings.Contains(bfErr.Error.Message, "requires store=false") || strings.Contains(bfErr.Error.Message, "differs from admitted model") || strings.Contains(bfErr.Error.Message, "does not support temperature")) {
			t.Fatalf("unsafe extra params reached transport: %+v", bfErr)
		}
	}
}

func TestChatStreamKeepsParallelToolIndexes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`{"type":"response.created","response":{"id":"resp_fixture","model":"gpt-5.3-codex","created_at":123}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"weather","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"city\":\"Oslo\"}"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"time","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_2","delta":"{}"}`,
			terminal,
		} {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) { return "token", "account", nil }}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	p.url = server.URL
	ch, bfErr := p.ChatCompletionStream(admitted(t), func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return r, e
	}, func(context.Context) {}, schemas.Key{}, &schemas.BifrostChatRequest{Model: "gpt-5.3-codex", Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: schemas.Ptr("compare")}}}})
	if bfErr != nil {
		t.Fatal(bfErr)
	}
	indexes := []string{}
	for chunk := range ch {
		if chunk.BifrostError != nil {
			t.Fatal(chunk.BifrostError)
		}
		b, err := schemas.MarshalSorted(chunk.BifrostChatResponse)
		if err != nil {
			t.Fatal(err)
		}
		for _, call := range gjson.GetBytes(b, "choices.0.delta.tool_calls").Array() {
			indexes = append(indexes, call.Get("index").String())
		}
	}
	if strings.Join(indexes, ",") != "0,0,1,1" {
		t.Fatalf("parallel tools collided: %v", indexes)
	}
}

func TestCatalogUsesOwnerCredentialAndFiltersPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer owner-token" || r.Header.Get("ChatGPT-Account-ID") != "owner-account" {
			t.Error("wrong catalog account or path")
		}
		fmt.Fprint(w, `{"models":[{"slug":"gpt-5.3-codex","display_name":"Codex","context_window":123456},{"slug":"disallowed","context_window":9876}]}`)
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) {
		return "owner-token", "owner-account", nil
	}}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	p.url = server.URL
	result, bfErr := p.ListModels(admitted(t), []schemas.Key{{Models: schemas.WhiteList{"*"}}}, &schemas.BifrostListModelsRequest{})
	if bfErr != nil {
		t.Fatal(bfErr)
	}
	if len(result.Data) != 1 || result.Data[0].ID != "codex/gpt-5.3-codex" || result.Data[0].ContextLength == nil || *result.Data[0].ContextLength != 123456 {
		t.Fatalf("incorrect scoped catalog: %+v", result)
	}
}

func TestRawUnaryPreservesTerminalJSONAndRejectsTruncation(t *testing.T) {
	for _, complete := range []bool{true, false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !gjson.GetBytes(body, "stream").Bool() {
				t.Error("upstream did not receive stream=true")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			if complete {
				fmt.Fprint(w, "data: "+strings.Replace(terminal, `"object":"response"`, `"object":"response","opaque":{"z":8,"a":2}`, 1)+"\n\n")
			} else {
				fmt.Fprint(w, "data: {\"type\":\"response.created\"}\n\n")
			}
		}))
		p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) { return "token", "account", nil }}, testLogger{})
		if err != nil {
			t.Fatal(err)
		}
		p.url = server.URL
		result, bfErr := p.Passthrough(admitted(t), schemas.Key{}, &schemas.BifrostPassthroughRequest{Model: "gpt-5.3-codex", Method: "POST", Path: "/responses", Body: []byte(`{"model":"gpt-5.3-codex","input":"hello","stream":false}`)})
		server.Close()
		if !complete {
			if bfErr == nil {
				t.Fatal("truncated raw stream accepted")
			}
			continue
		}
		if bfErr != nil {
			t.Fatal(bfErr)
		}
		if !strings.Contains(string(result.Body), `"opaque":{"z":8,"a":2}`) || result.PassthroughUsage == nil || result.PassthroughUsage.LLMUsage.TotalTokens != 140 {
			t.Fatalf("raw terminal or accounting lost: %+v", result)
		}
	}
}

func TestCancellationClosesUpstreamStream(t *testing.T) {
	closed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"pending\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(closed)
		case <-time.After(5 * time.Second):
		}
	}))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{AllowPrivateNetwork: true}, CodexCredential: func(*schemas.BifrostContext, schemas.Key) (string, string, error) { return "token", "account", nil }}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	p.url = server.URL
	ctx := admitted(t)
	ch, bfErr := p.ResponsesStream(ctx, func(_ *schemas.BifrostContext, r *schemas.BifrostResponse, e *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError) {
		return r, e
	}, func(context.Context) {}, schemas.Key{}, &schemas.BifrostResponsesRequest{Model: "gpt-5.3-codex", Input: []schemas.ResponsesMessage{{Role: schemas.Ptr(schemas.ResponsesInputMessageRoleUser), Content: &schemas.ResponsesMessageContent{ContentStr: schemas.Ptr("hello")}}}})
	if bfErr != nil {
		t.Fatal(bfErr)
	}
	ctx.Cancel()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream stream was not canceled")
	}
	for range ch {
	}
}
