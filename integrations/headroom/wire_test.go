package main

import (
	"bytes"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

func TestNativeProtocolPreservation(t *testing.T) {
	cases := []struct{ protocol, body string }{
		{"chat", `{ "model":"m", "messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"charge_card","arguments":"{\"amount\":17}"}}]},{"role":"tool","tool_call_id":"c","content":"original output"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"opaque"}}]}],"response_format":{"type":"json_schema","json_schema":{"strict":true}},"unknown":9007199254740993 }`},
		{"responses", `{ "model":"m", "input":[{"type":"reasoning","encrypted_content":"opaque-signed"},{"type":"function_call","call_id":"c","arguments":"{\"x\":19}"},{"type":"function_call_output","call_id":"c","output":"original output"},{"type":"message","phase":"commentary","content":[{"type":"input_image","image_url":"opaque"}]}],"tools":[{"type":"function","strict":true}],"include":["reasoning.encrypted_content"] }`},
		{"anthropic", `{ "model":"m", "messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"signed"},{"type":"tool_use","id":"c","name":"write_file","input":{"path":"/a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"original output"},{"type":"image","source":{"type":"base64","data":"opaque"}}]}],"tools":[{"name":"write_file","input_schema":{"type":"object","additionalProperties":false}}] }`},
	}
	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			paths, _, reason := slots([]byte(tc.body), tc.protocol, 8, false)
			if reason != "" || len(paths) != 1 {
				t.Fatal(paths, reason)
			}
			got, err := patchSlots([]byte(tc.body), paths, []string{"short"})
			want := strings.Replace(tc.body, `"original output"`, `"short"`, 1)
			if err != nil || string(got) != want {
				t.Fatalf("wire changed outside selected text:\n%s", got)
			}
		})
	}
}

func TestBypassBoundaries(t *testing.T) {
	for _, body := range []string{`{"previous_response_id":"r","input":[]}`, `{"params":{"prompt_cache_key":"prefix"},"messages":[]}`, `{"messages":[{"content":[{"cache_control":{"type":"ephemeral"}}]}]}`, `{"background":true}`, `garbage`, `[]`, `{"messages":[{"role":"tool","content":[{"type":"image_url"}]}]}`} {
		if p, _, reason := slots([]byte(body), "chat", 8, false); len(p) != 0 || reason == "" {
			t.Fatal("unsafe eligibility", body)
		}
	}
	for _, text := range []string{"1234567", "<<ccr:abc>>", "headroom_retrieve"} {
		body := []byte(`{"messages":[{"role":"tool","content":"` + text + `"}]}`)
		if p, _, _ := slots(body, "chat", 8, false); len(p) != 0 {
			t.Fatal("threshold/CCR boundary")
		}
	}
	if p, _, _ := slots([]byte(`{"messages":[{"role":"tool","content":"12345678"}]}`), "chat", 8, false); len(p) != 1 {
		t.Fatal("off-by-one threshold")
	}
}

func TestCacheHintsDoNotBlockCompression(t *testing.T) {
	for _, wrapper := range []string{`%s`, `"params":{%s}`, `"params":{"extra_params":{%s}}`} {
		for _, state := range []string{"", "previous_response_id", "conversation"} {
			hints := `"prompt_cache_key":"session","prompt_cache_retention":"24h","prompt_cache_options":{"mode":"implicit"}`
			if state != "" {
				hints += `,"` + state + `":"opaque"`
			}
			body := []byte(`{"input":[{"type":"function_call_output","call_id":"c","output":"original output"}],` + strings.Replace(wrapper, "%s", hints, 1) + `}`)
			paths, _, reason := slots(body, "responses", 8, false)
			if state != "" {
				if len(paths) != 0 || reason != "provider_state_"+state {
					t.Fatalf("continuation admitted or wrong reason: %s %v %s", state, paths, reason)
				}
				continue
			}
			if len(paths) != 1 || reason != "" {
				t.Fatalf("cache hint blocked compression: %v %s", paths, reason)
			}
			out, err := patchSlots(body, paths, []string{"short"})
			if err != nil || string(out) != strings.Replace(string(body), "original output", "short", 1) {
				t.Fatal("cache metadata or tool identity changed")
			}
		}
	}
}

func TestCCRToolAdvertisementAndRetrievedOutputExclusion(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","function":{"name":"tenant__headroom_retrieve"}}],"messages":[{"role":"assistant","tool_calls":[{"id":"r1","function":{"name":"tenant__headroom_retrieve"}},{"id":"c1","function":{"name":"other"}}]},{"role":"tool","tool_call_id":"r1","content":"retrieved original output"},{"role":"tool","tool_call_id":"c1","content":"ordinary tool output"}]}`)
	if got := advertisedRetrievalTool(body, "chat"); got != "tenant__headroom_retrieve" {
		t.Fatalf("actual advertised name lost: %q", got)
	}
	paths, texts, reason := slots(body, "chat", 8, false)
	if reason != "" || len(paths) != 1 || texts[0] != "ordinary tool output" {
		t.Fatalf("retrieval output replayed/compressed: %v %v %s", paths, texts, reason)
	}
	without := []byte(`{"tools":[{"type":"function","function":{"name":"other"}}]}`)
	if advertisedRetrievalTool(without, "chat") != "" {
		t.Fatal("CCR marker allowed without exact advertised tool")
	}
	marker := ccrMarker("tenant__headroom_retrieve", strings.Repeat("a", 64))
	if !strings.Contains(marker, "tenant__headroom_retrieve") || !strings.Contains(marker, `{"hash":"`) {
		t.Fatal("marker contract changed")
	}
}

func TestAmpDeferredCapabilityAndOutputExclusion(t *testing.T) {
	flat := []byte(`{"tools":[{"type":"function","name":"code_exec"},{"type":"function","name":"tool_search"}]}`)
	namespace := []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"code_exec"},{"type":"function","name":"tool_search"}]}]}`)
	for name, body := range map[string][]byte{"flat": flat, "namespace": namespace} {
		t.Run(name, func(t *testing.T) {
			if !advertisesAmpDeferredTools(body, "responses") {
				t.Fatal("missed structural Amp capability")
			}
		})
	}
	falsePositives := [][]byte{
		[]byte(`{"description":"Use code_exec and tool_search"}`),
		[]byte(`{"tools":[{"type":"function","name":"code_exec"}]}`),
		[]byte(`{"tools":[{"type":"function","name":"code_exec"},{"type":"web_search","name":"tool_search"}]}`),
		[]byte(`{"tools":[{"type":"namespace","name":"one","tools":[{"type":"function","name":"code_exec"}]},{"type":"namespace","name":"two","tools":[{"type":"function","name":"tool_search"}]}]}`),
	}
	for _, body := range falsePositives {
		if advertisesAmpDeferredTools(body, "responses") {
			t.Fatalf("inferred capability from non-capability input: %s", body)
		}
	}

	body := []byte(`{"input":[{"type":"function_call","namespace":"functions","name":"code_exec","call_id":"exec"},{"type":"function_call_output","call_id":"exec","output":"retrieved original output"},{"type":"custom_tool_call","name":"functions.tool_search","id":"search"},{"type":"custom_tool_call_output","call_id":"search","output":"search results here"},{"type":"function_call","name":"shell","call_id":"shell"},{"type":"function_call_output","call_id":"shell","output":"ordinary shell output"},{"type":"function_call","name":"code_exec","call_id":""},{"type":"function_call_output","call_id":"","output":"uncorrelated output"}]}`)
	paths, texts, reason := slots(body, "responses", 8, true)
	if reason != "" || len(paths) != 1 || texts[0] != "ordinary shell output" {
		t.Fatalf("wrong Amp correlation protection: %v %v %s", paths, texts, reason)
	}
	_, unprotected, reason := slots(body, "responses", 8, false)
	if reason != "" || len(unprotected) != 4 {
		t.Fatalf("non-opted caller was changed: %v %s", unprotected, reason)
	}
	if got := advertisedRetrievalTool([]byte(`{"tools":[{"type":"function","name":"headroom.headroom_retrieve"}]}`), "responses"); got != "headroom.headroom_retrieve" {
		t.Fatalf("qualified direct retrieval not recognized: %q", got)
	}
	if got := advertisedRetrievalTool([]byte(`{"tools":[{"type":"namespace","name":"headroom","tools":[{"type":"function","name":"headroom_retrieve"}]}]}`), "responses"); got != "headroom.headroom_retrieve" {
		t.Fatalf("namespaced direct retrieval not recognized: %q", got)
	}
	marker := (retrievalDescriptor{mode: ccrAmpDeferred}).marker(strings.Repeat("a", 64))
	for _, exact := range []string{"advertised tool_search", "headroom.headroom_retrieve", `import {headroom_retrieve} from "headroom"`, `text(await headroom_retrieve({hash:"`} {
		if !strings.Contains(marker, exact) {
			t.Fatalf("deferred marker omitted %q", exact)
		}
	}
}

func TestUnknownEndpointsBypass(t *testing.T) {
	for _, path := range []string{"/v1/batches", "/v1/responses/id"} {
		body := []byte(`{"model":"m","stream":true,"input":[{"type":"function_call_output","output":"original output"}]}`)
		req := &schemas.BifrostRequest{RequestType: schemas.PassthroughStreamRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Method: "POST", Path: path, Model: "m", Body: body}}
		_, _, _, reason := requestBody(admitted("p", "v"), req)
		if reason == "" || !bytes.Equal(body, req.PassthroughRequest.Body) {
			t.Fatal("raw SSE changed")
		}
	}
}

func TestTypedCodexCacheHintCompression(t *testing.T) {
	b := testBridge(t, goodReply)
	var input []schemas.ResponsesMessage
	if err := schemas.Unmarshal([]byte(`[{"type":"function_call_output","call_id":"c","output":"original output"}]`), &input); err != nil {
		t.Fatal(err)
	}
	for _, requestType := range []schemas.RequestType{schemas.ResponsesRequest, schemas.ResponsesStreamRequest} {
		req := &schemas.BifrostRequest{RequestType: requestType, ResponsesRequest: &schemas.BifrostResponsesRequest{
			Provider: schemas.Codex, Model: "gpt-6-astra", Input: input,
			Params: &schemas.ResponsesParameters{PromptCacheKey: schemas.Ptr("caller-session")},
		}}
		ctx := admitted("project-a", "vk-a")
		out, sc, err := b.pre(ctx, req)
		if err != nil || sc != nil || ctx.Value(eventKey).(*Event).Status != "compressed" {
			t.Fatal("typed Codex request bypassed", err)
		}
		if *out.ResponsesRequest.Input[0].Output.ResponsesToolCallOutputStr != "short" || *input[0].Output.ResponsesToolCallOutputStr != "original output" {
			t.Fatal("compression not committed or fallback input mutated")
		}
		if *out.ResponsesRequest.Params.PromptCacheKey != "caller-session" || out.ResponsesRequest.Provider != schemas.Codex || out.RequestType != requestType {
			t.Fatal("cache key or routing changed")
		}
	}
}

func TestPromptCacheKeyReplaysAcrossRequestIDsButNotOwners(t *testing.T) {
	var calls atomic.Int32
	b := testBridge(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); goodReply(w, r) })
	var err error
	b.cache, err = openDecisionCache(t.TempDir(), b.key)
	if err != nil {
		t.Fatal(err)
	}
	b.config.CCR = true
	body := []byte(`{"model":"m","prompt_cache_key":"stable-key","tools":[{"type":"function","name":"mcp__headroom_retrieve"}],"input":[{"type":"function_call_output","call_id":"c","output":"original output for retrieval"}]}`)
	req := &schemas.BifrostRequest{RequestType: schemas.PassthroughRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{
		Provider: schemas.Codex, Method: "POST", Path: "/v1/responses", Model: "m", Body: body,
	}}
	var first []byte
	for i, owner := range []string{"vk-a", "vk-a", "vk-b"} {
		ctx := admitted("project-a", owner)
		ctx.SetValue(schemas.BifrostContextKeySessionID, "")
		ctx.SetValue(schemas.BifrostContextKeyRequestID, []string{"request-a", "request-b", "request-c"}[i])
		out, sc, err := b.pre(ctx, req)
		if err != nil || sc != nil || !bytes.Contains(out.PassthroughRequest.Body, []byte("mcp__headroom_retrieve with")) {
			t.Fatal("CCR hook did not expose retrieval marker", err)
		}
		if i == 0 {
			first = out.PassthroughRequest.Body
		} else if i == 1 && (!bytes.Equal(first, out.PassthroughRequest.Body) || calls.Load() != 1) {
			t.Fatal("request ID changed stable output or invoked compressor")
		} else if i == 2 && (bytes.Equal(first, out.PassthroughRequest.Body) || calls.Load() != 2) {
			t.Fatal("different owner reused another owner's record")
		}
	}
	if !bytes.Equal(body, req.PassthroughRequest.Body) {
		t.Fatal("original request mutated")
	}
}

func TestRawStreamingCompressesInputWithoutChangingProtocol(t *testing.T) {
	b := testBridge(t, goodReply)
	for _, provider := range []schemas.ModelProvider{schemas.OpenAI, schemas.Codex} {
		body := []byte(`{"model":"m","stream":true,"prompt_cache_key":"caller-session","prompt_cache_retention":"24h","input":[{"type":"reasoning","encrypted_content":"signed"},{"type":"function_call_output","call_id":"c","output":"original output"}],"tools":[{"type":"function","strict":true}]}`)
		req := &schemas.BifrostRequest{RequestType: schemas.PassthroughStreamRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Provider: provider, Method: "POST", Path: "/v1/responses", Model: "m", Body: body}}
		got, sc, err := b.pre(admitted("project-a", "vk-a"), req)
		if err != nil || sc != nil || string(got.PassthroughRequest.Body) != strings.Replace(string(body), "original output", "short", 1) || got.RequestType != req.RequestType || got.PassthroughRequest.Provider != provider {
			t.Fatal("stream request protocol changed or compression skipped", provider)
		}
		if !bytes.Equal(req.PassthroughRequest.Body, body) {
			t.Fatal("original request mutated")
		}
	}
}

func FuzzPatchOnlyText(f *testing.F) {
	f.Add("text \" with unicode π\n")
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 8192 {
			return
		}
		body, err := schemas.MarshalSorted(map[string]any{"messages": []any{map[string]any{"role": "tool", "content": text}}, "protected": map[string]any{"n": 123456789, "signature": "opaque"}})
		if err != nil {
			return
		}
		paths, _, reason := slots(body, "chat", 1, false)
		if reason != "" {
			return
		}
		out, err := patchSlots(body, paths, []string{"short"})
		if err != nil {
			t.Fatal(err)
		}
		// JSON encoders differ on HTML escaping within the selected string.
		// Compare the untouched prefix/suffix instead of requiring identical
		// representation when a string is re-encoded by another encoder.
		original := gjson.GetBytes(body, "messages.0.content")
		want := string(body[:original.Index]) + `"short"` + string(body[original.Index+len(original.Raw):])
		if string(out) != want {
			t.Fatal("non-text bytes changed")
		}
	})
}
