package main

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// slots recognizes only known tool-result text positions. Patching the original
// JSON preserves all other bytes, including signed/encrypted reasoning, strict
// schemas, tool arguments, multimodal blocks and unknown extension fields.
func slots(body []byte, protocol string, minBytes int, excludeAmpDeferred bool) ([]string, []string, string) {
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
	retrievalIDs := protectedCallIDs(body, protocol, excludeAmpDeferred)
	protected := func(id string) bool {
		value, known := retrievalIDs[id]
		return value || (excludeAmpDeferred && !known)
	}
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
			if msg.Get("role").Str == "tool" && !protected(msg.Get("tool_call_id").Str) {
				add("messages." + strconv.Itoa(i) + ".content")
			}
		}
	case "responses":
		for i, item := range gjson.GetBytes(body, "input").Array() {
			if (item.Get("type").Str == "function_call_output" || item.Get("type").Str == "custom_tool_call_output") && !protected(item.Get("call_id").Str) {
				add("input." + strconv.Itoa(i) + ".output")
			}
		}
	case "anthropic":
		for i, msg := range gjson.GetBytes(body, "messages").Array() {
			if msg.Get("role").Str != "user" {
				continue
			}
			for j, part := range msg.Get("content").Array() {
				if part.Get("type").Str == "tool_result" && !part.Get("is_error").Bool() && !protected(part.Get("tool_use_id").Str) {
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

func isRetrievalTool(name string) bool {
	return name == "headroom_retrieve" || strings.HasSuffix(name, "__headroom_retrieve") || strings.HasSuffix(name, ".headroom_retrieve")
}

type ccrMode string

const (
	ccrNone        ccrMode = "none"
	ccrDirect      ccrMode = "direct"
	ccrAmpDeferred ccrMode = "amp_deferred"
)

type retrievalDescriptor struct {
	mode ccrMode
	tool string
}

func (r retrievalDescriptor) enabled() bool {
	return r.mode == ccrDirect || r.mode == ccrAmpDeferred
}

func (r retrievalDescriptor) marker(handle string) string {
	if r.mode == ccrAmpDeferred {
		return "\n\n[Exact original available. Use the advertised tool_search to discover headroom.headroom_retrieve; then use the advertised code_exec with: import {headroom_retrieve} from \"headroom\"; text(await headroom_retrieve({hash:\"" + handle + "\"}))]"
	}
	if r.mode != ccrDirect {
		return ""
	}
	return ccrMarker(r.tool, handle)
}

func advertisedRetrievalTool(body []byte, protocol string) string {
	for _, tool := range advertisedTools(body, protocol) {
		if tool.Get("type").Str == "namespace" {
			for _, child := range tool.Get("tools").Array() {
				if child.Get("type").Str == "function" && isRetrievalTool(child.Get("name").Str) && tool.Get("name").Str != "" {
					return tool.Get("name").Str + "." + child.Get("name").Str
				}
			}
			continue
		}
		if kind := tool.Get("type").Str; kind != "" && kind != "function" {
			continue
		}
		name := tool.Get("name").Str
		if name == "" {
			name = tool.Get("function.name").Str
		}
		if isRetrievalTool(name) {
			return name
		}
	}
	return ""
}

func toolLeaf(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

func isAmpDeferredTool(name string) bool {
	leaf := toolLeaf(name)
	return leaf == "code_exec" || leaf == "tool_search"
}

func advertisedTools(body []byte, protocol string) []gjson.Result {
	paths := []string{"tools"}
	if protocol != "anthropic" {
		paths = append(paths, "params.tools", "params.extra_params.tools")
	}
	var tools []gjson.Result
	for _, path := range paths {
		tools = append(tools, gjson.GetBytes(body, path).Array()...)
	}
	return tools
}

func advertisesAmpDeferredTools(body []byte, protocol string) bool {
	byNamespace := map[string]map[string]bool{}
	for _, declaration := range advertisedTools(body, protocol) {
		if declaration.Get("type").Str == "namespace" {
			namespace := declaration.Get("name").Str
			if namespace == "" {
				continue
			}
			for _, tool := range declaration.Get("tools").Array() {
				name := tool.Get("name").Str
				if tool.Get("type").Str == "function" && (name == "code_exec" || name == "tool_search") {
					if byNamespace[namespace] == nil {
						byNamespace[namespace] = map[string]bool{}
					}
					byNamespace[namespace][name] = true
				}
			}
			continue
		}
		if kind := declaration.Get("type").Str; kind != "" && kind != "function" && kind != "custom" {
			continue
		}
		name := declaration.Get("name").Str
		if name == "" {
			name = declaration.Get("function.name").Str
		}
		if name == "code_exec" || name == "tool_search" {
			if byNamespace[""] == nil {
				byNamespace[""] = map[string]bool{}
			}
			byNamespace[""][name] = true
		}
	}
	for _, names := range byNamespace {
		if names["code_exec"] && names["tool_search"] {
			return true
		}
	}
	return false
}

func protectedCallIDs(body []byte, protocol string, excludeAmpDeferred bool) map[string]bool {
	ids := map[string]bool{}
	protect := func(name, id string) {
		if id != "" && name != "" {
			ids[id] = ids[id] || isRetrievalTool(name) || (excludeAmpDeferred && isAmpDeferredTool(name))
		}
	}
	switch protocol {
	case "chat":
		for _, msg := range gjson.GetBytes(body, "messages").Array() {
			for _, call := range msg.Get("tool_calls").Array() {
				protect(call.Get("function.name").Str, call.Get("id").Str)
			}
		}
	case "responses":
		for _, item := range gjson.GetBytes(body, "input").Array() {
			if item.Get("type").Str == "function_call" || item.Get("type").Str == "custom_tool_call" {
				id := item.Get("call_id").Str
				if id == "" {
					id = item.Get("id").Str
				}
				name := item.Get("name").Str
				if namespace := item.Get("namespace").Str; namespace != "" && !strings.Contains(name, ".") {
					name = namespace + "." + name
				}
				protect(name, id)
			}
		}
	case "anthropic":
		for _, msg := range gjson.GetBytes(body, "messages").Array() {
			for _, part := range msg.Get("content").Array() {
				if part.Get("type").Str == "tool_use" {
					protect(part.Get("name").Str, part.Get("id").Str)
				}
			}
		}
	}
	return ids
}

func promptCacheKey(body []byte) string {
	for _, path := range []string{"prompt_cache_key", "params.prompt_cache_key", "params.extra_params.prompt_cache_key"} {
		if v := gjson.GetBytes(body, path); v.Type == gjson.String && v.Str != "" && len(v.Str) <= 256 {
			return v.Str
		}
	}
	return ""
}

func ccrMarker(tool, handle string) string {
	argument, _ := json.Marshal(map[string]string{"hash": handle})
	return "\n\n[Exact original available: call " + tool + " with " + string(argument) + "]"
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
