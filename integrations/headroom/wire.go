package main

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// slots recognizes only known tool-result text positions. Patching the original
// JSON preserves all other bytes, including signed/encrypted reasoning, strict
// schemas, tool arguments, multimodal blocks and unknown extension fields.
func slots(body []byte, protocol string, minBytes int) ([]string, []string, string) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, nil, "invalid_json"
	}
	// Provider-held conversation state must not be rewritten. prompt_cache_key
	// only partitions/routes the prefix cache: reuse still requires matching
	// content. Preserve that hint while allowing tool-result compression.
	for _, prefix := range []string{"", "params.", "params.extra_params."} {
		for _, key := range []string{"previous_response_id", "conversation"} {
			if value := gjson.GetBytes(body, prefix+key); value.Exists() && value.Type != gjson.Null {
				return nil, nil, "provider_state_" + key
			}
		}
	}
	if strings.Contains(string(body), `"cache_control"`) {
		return nil, nil, "explicit_cache_control"
	}
	if gjson.GetBytes(body, "background").Bool() || gjson.GetBytes(body, "params.background").Bool() {
		return nil, nil, "background"
	}
	var paths, texts []string
	add := func(path string) {
		value := gjson.GetBytes(body, path)
		if value.Type == gjson.String && len(value.Str) >= minBytes && !strings.Contains(value.Str, "<<ccr:") && !strings.Contains(value.Str, "headroom_retrieve") {
			paths = append(paths, path)
			texts = append(texts, value.Str)
		}
	}
	switch protocol {
	case "chat":
		for i, msg := range gjson.GetBytes(body, "messages").Array() {
			if msg.Get("role").Str == "tool" {
				add("messages." + strconv.Itoa(i) + ".content")
			}
		}
	case "responses":
		for i, item := range gjson.GetBytes(body, "input").Array() {
			if item.Get("type").Str == "function_call_output" || item.Get("type").Str == "custom_tool_call_output" {
				add("input." + strconv.Itoa(i) + ".output")
			}
		}
	case "anthropic":
		for i, msg := range gjson.GetBytes(body, "messages").Array() {
			if msg.Get("role").Str != "user" {
				continue
			}
			for j, part := range msg.Get("content").Array() {
				if part.Get("type").Str == "tool_result" && !part.Get("is_error").Bool() {
					add("messages." + strconv.Itoa(i) + ".content." + strconv.Itoa(j) + ".content")
				}
			}
		}
	default:
		return nil, nil, "unsupported_protocol"
	}
	if len(paths) == 0 {
		return nil, nil, "no_eligible_text"
	}
	return paths, texts, ""
}

func patchSlots(body []byte, paths, texts []string) ([]byte, error) {
	out := append([]byte(nil), body...)
	var err error
	for i, path := range paths {
		out, err = sjson.SetBytes(out, path, texts[i])
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
