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

func TestRawSSEAndUnknownEndpointsBypass(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/messages", "/v1/chat/completions", "/v1/batches", "/v1/responses/id"} {
		body := []byte(`{"model":"m","stream":true,"input":[{"type":"function_call_output","output":"original output"}]}`)
		req := &schemas.BifrostRequest{RequestType: schemas.PassthroughStreamRequest, PassthroughRequest: &schemas.BifrostPassthroughRequest{Method: "POST", Path: path, Model: "m", Body: body}}
		_, _, _, reason := requestBody(admitted("p", "v"), req)
		if reason == "" || !bytes.Equal(body, req.PassthroughRequest.Body) {
			t.Fatal("raw SSE changed")
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
