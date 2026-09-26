package main

import (
	"bytes"
	"strings"
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
			paths, _, reason := slots([]byte(tc.body), tc.protocol, 8)
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
		if p, _, reason := slots([]byte(body), "chat", 8); len(p) != 0 || reason == "" {
			t.Fatal("unsafe eligibility", body)
		}
	}
	for _, text := range []string{"1234567", "<<ccr:abc>>", "headroom_retrieve"} {
		body := []byte(`{"messages":[{"role":"tool","content":"` + text + `"}]}`)
		if p, _, _ := slots(body, "chat", 8); len(p) != 0 {
			t.Fatal("threshold/CCR boundary")
		}
	}
	if p, _, _ := slots([]byte(`{"messages":[{"role":"tool","content":"12345678"}]}`), "chat", 8); len(p) != 1 {
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
			paths, _, reason := slots(body, "responses", 8)
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
		paths, _, reason := slots(body, "chat", 1)
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
