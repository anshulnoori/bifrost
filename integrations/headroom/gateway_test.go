package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// Runs the built gateway and native plugin against the actual Headroom sidecar.
// Only the provider is a fixture. No paid inference or OAuth credentials are used.
func TestGatewayWithLiveHeadroom(t *testing.T) {
	binary, plugin, endpoint := os.Getenv("HEADROOM_GATEWAY_BINARY"), os.Getenv("HEADROOM_PLUGIN_PATH"), os.Getenv("HEADROOM_BENCH_URL")
	if binary == "" || plugin == "" || endpoint == "" {
		t.Skip("set HEADROOM_GATEWAY_BINARY, HEADROOM_PLUGIN_PATH, HEADROOM_BENCH_URL and HEADROOM_BENCH_TOKEN")
	}
	var mu sync.Mutex
	var seen []string
	sse := "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"TX-731\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-4.1","object":"model"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "prompt_cache_key").String() != "fixture-session" {
			t.Error("gateway changed or dropped prompt cache key")
		}
		mu.Lock()
		seen = append(seen, gjson.GetBytes(body, "messages.1.content").String())
		mu.Unlock()
		if gjson.GetBytes(body, "stream").Bool() {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, sse)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fixture","object":"chat.completion","model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"TX-731"},"finish_reason":"stop"}],"usage":{"prompt_tokens":317,"completion_tokens":29,"total_tokens":346}}`)
	}))
	defer provider.Close()
	port := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		p := l.Addr().(*net.TCPAddr).Port
		l.Close()
		return p
	}
	gatewayPort, metricsPort := port(), port()
	dir := t.TempDir()
	config := map[string]any{
		"client":       map[string]any{"enable_logging": false, "enforce_auth_on_inference": false},
		"config_store": map[string]any{"enabled": true, "type": "sqlite", "config": map[string]any{"path": filepath.Join(dir, "config.db")}},
		"providers":    map[string]any{"openai": map[string]any{"keys": []any{map[string]any{"id": "fixture-key", "value": "fixture-only", "models": []string{"*"}, "weight": 1}}, "network_config": map[string]any{"base_url": provider.URL, "allow_private_network": true}}},
		"governance":   map[string]any{"virtual_keys": []any{map[string]any{"id": "owner-a", "name": "owner-a", "value": "sk-bf-headroom-fixture", "is_active": true, "provider_configs": []any{map[string]any{"provider": "openai", "allowed_models": []string{"*"}, "key_ids": []string{"*"}, "weight": 1}}}}},
		"plugins":      []any{map[string]any{"name": "telemetry", "enabled": false}},
	}
	config["governance"].(map[string]any)["auth_config"] = map[string]any{"is_enabled": true, "admin_username": "fixture-admin", "admin_password": "fixture-admin-password"}
	keys := config["governance"].(map[string]any)["virtual_keys"].([]any)
	config["governance"].(map[string]any)["virtual_keys"] = append(keys, map[string]any{"id": "owner-b", "name": "owner-b", "value": "sk-bf-other-fixture", "is_active": true})
	data, _ := json.Marshal(config)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(dir, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(binary, "-app-dir", dir, "-host", "127.0.0.1", "-port", fmt.Sprint(gatewayPort))
	cmd.Env = append(os.Environ(), "HEADROOM_PROXY_TOKEN="+os.Getenv("HEADROOM_BENCH_TOKEN"), "HEADROOM_SCOPE_KEY="+strings.Repeat("s", 32), "HEADROOM_METRICS_TOKEN="+strings.Repeat("m", 32), fmt.Sprintf("BIFROST_HEADROOM_METRICS_PORT=%d", metricsPort))
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	client := &http.Client{Timeout: 30 * time.Second}
	base := fmt.Sprintf("http://127.0.0.1:%d", gatewayPort)
	ready := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		req, _ := http.NewRequest("GET", base+"/api/plugins", nil)
		req.SetBasicAuth("fixture-admin", "fixture-admin-password")
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !ready {
		contents, _ := os.ReadFile(filepath.Join(dir, "gateway.log"))
		t.Fatalf("gateway not ready: %s", contents)
	}
	withIdentity := true
	virtualKey := "sk-bf-headroom-fixture"
	call := func(method, path string, body any) (int, []byte) {
		encoded, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		if strings.HasPrefix(path, "/openai_passthrough/") {
			req.Header.Set("x-model-provider", "openai")
		}
		req.SetBasicAuth("fixture-admin", "fixture-admin-password")
		if withIdentity {
			req.Header.Set("x-bf-vk", virtualKey)
			req.Header.Set("x-bf-session-id", "fixture-session")
			req.Header.Set("x-headroom-thread", "fixture-thread")
		}
		req.Header.Set("X-Headroom-Admin-Token", strings.Repeat("m", 32))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, data
	}
	settings := map[string]any{"name": "headroom", "path": plugin, "enabled": true, "placement": "post_builtin", "config": map[string]any{"enabled": true, "virtual_key_id": "owner-a", "endpoint": endpoint, "token_env": "HEADROOM_PROXY_TOKEN", "scope_key_env": "HEADROOM_SCOPE_KEY", "metrics_address": fmt.Sprintf("127.0.0.1:%d", metricsPort), "metrics_token_env": "HEADROOM_METRICS_TOKEN", "retention_seconds": 900, "timeout_ms": 30000}}
	settings["config"].(map[string]any)["cache_dir"] = filepath.Join(dir, "decisions")
	settings["config"].(map[string]any)["ccr"] = true
	settings["config"].(map[string]any)["amp_deferred_retrieval_virtual_key_id"] = "owner-a"
	if os.Getenv("HEADROOM_MODAL_KEY") != "" || os.Getenv("HEADROOM_MODAL_SECRET") != "" {
		settings["config"].(map[string]any)["modal_key_env"] = "HEADROOM_MODAL_KEY"
		settings["config"].(map[string]any)["modal_secret_env"] = "HEADROOM_MODAL_SECRET"
	}
	if status, data := call("POST", "/api/plugins", settings); status != 200 && status != 201 {
		t.Fatalf("create plugin=%d %s", status, data)
	}
	text := strings.Repeat("2026-09-22 INFO request completed successfully\n", 250) + "FATAL transaction=TX-731 amount=1949.37 failed integrity check\n"
	request := map[string]any{"model": "openai/gpt-4.1", "prompt_cache_key": "fixture-session", "messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c", "type": "function", "function": map[string]any{"name": "read_logs", "arguments": "{}"}}}}, map[string]any{"role": "tool", "tool_call_id": "c", "content": text}}}
	request["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "headroom_retrieve", "parameters": map[string]any{"type": "object", "properties": map[string]any{"hash": map[string]any{"type": "string"}}, "required": []string{"hash"}}}}}
	if status, data := call("POST", "/v1/chat/completions", request); status != 200 {
		t.Fatalf("inference=%d %s", status, data)
	}
	mu.Lock()
	first := append([]string(nil), seen...)
	mu.Unlock()
	if len(first) != 1 || len(first[0]) >= len(text) || !strings.Contains(first[0], "TX-731") {
		_, events := call("GET", "/api/headroom/events", nil)
		t.Fatalf("provider did not receive fact-preserving compressed text (calls=%d), events=%s", len(first), events)
	}
	status, events := call("GET", "/api/headroom/events", nil)
	if status != 200 || gjson.GetBytes(events, "events.0.status").String() != "compressed" || gjson.GetBytes(events, "events.0.provider_usage.prompt_tokens").Int() != 317 {
		t.Fatalf("monitor=%d %s", status, events)
	}
	markerStart := strings.LastIndex(first[0], `{"hash":"`)
	if markerStart < 0 || len(first[0]) < markerStart+9+64 {
		t.Fatal("gateway did not emit a CCR handle")
	}
	handle := first[0][markerStart+9 : markerStart+9+64]
	if status, data := call("POST", "/v1/headroom/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); status != 200 || gjson.GetBytes(data, "result.tools.#").Int() != 1 || gjson.GetBytes(data, "result.tools.0.name").String() != "headroom_retrieve" {
		t.Fatalf("CCR tool list=%d %s", status, data)
	}
	if status, data := call("POST", "/v1/headroom/mcp", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "headroom_retrieve", "arguments": map[string]any{"hash": handle}}}); status != 200 || gjson.GetBytes(data, "result.isError").Bool() || gjson.GetBytes(data, "result.content.0.text").String() != text {
		t.Fatalf("CCR exact retrieval failed: status=%d isError=%v", status, gjson.GetBytes(data, "result.isError").Bool())
	}
	virtualKey = "sk-bf-other-fixture"
	if status, data := call("POST", "/v1/headroom/mcp", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "headroom_retrieve", "arguments": map[string]any{"hash": handle}}}); status != 200 || !gjson.GetBytes(data, "result.isError").Bool() || bytes.Contains(data, []byte("TX-731")) {
		t.Fatalf("CCR cross-owner denial failed: status=%d", status)
	}
	virtualKey = "sk-bf-headroom-fixture"
	settings["config"].(map[string]any)["enabled"] = false
	delete(settings, "path") // The UI omits an unchanged native path.
	if status, data := call("PUT", "/api/plugins/headroom", settings); status != 200 {
		t.Fatalf("disable=%d %s", status, data)
	}
	if status, data := call("POST", "/v1/chat/completions", request); status != 200 {
		t.Fatalf("disabled inference=%d %s", status, data)
	}
	mu.Lock()
	disabledOK := len(seen) == 2 && seen[1] == text
	mu.Unlock()
	if !disabledOK {
		t.Fatal("configuration update did not disable compression")
	}
	settings["config"].(map[string]any)["enabled"] = true
	if status, data := call("PUT", "/api/plugins/headroom", settings); status != 200 {
		t.Fatalf("enable=%d %s", status, data)
	}
	request["stream"], request["model"] = true, "gpt-4.1"
	if status, data := call("POST", "/openai_passthrough/v1/chat/completions", request); status != 200 || string(data) != sse {
		t.Fatalf("raw SSE changed=%d %s", status, data)
	}
	mu.Lock()
	streamOK := len(seen) == 3 && seen[2] == first[0]
	mu.Unlock()
	if !streamOK {
		_, events := call("GET", "/api/headroom/events", nil)
		t.Fatalf("stream input not compressed after re-enabling: %s", events)
	}
	settings["config"].(map[string]any)["scope"] = "gateway"
	settings["config"].(map[string]any)["virtual_key_id"] = ""
	if status, data := call("PUT", "/api/plugins/headroom", settings); status != 200 {
		t.Fatalf("gateway scope=%d %s", status, data)
	}
	withIdentity = false
	request["stream"], request["model"] = false, "openai/gpt-4.1"
	if status, data := call("POST", "/v1/chat/completions", request); status != 200 {
		t.Fatalf("normal provider=%d %s", status, data)
	}
	mu.Lock()
	normalOK := len(seen) == 4 && len(seen[3]) < len(text)
	mu.Unlock()
	if !normalOK {
		_, events := call("GET", "/api/headroom/events", nil)
		t.Fatalf("normal provider without virtual key not compressed: %s", events)
	}
	withIdentity = true
	fresh := text + "INFO deferred retrieval fixture\n"
	request["messages"].([]any)[1].(map[string]any)["content"] = fresh
	request["tools"] = []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "tool_search", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "code_exec", "parameters": map[string]any{"type": "object"}}},
	}
	if status, data := call("POST", "/v1/chat/completions", request); status != 200 {
		t.Fatalf("deferred inference=%d %s", status, data)
	}
	mu.Lock()
	deferred := seen[len(seen)-1]
	mu.Unlock()
	start := strings.LastIndex(deferred, `hash:"`)
	if start < 0 || len(deferred) < start+6+64 || !strings.Contains(deferred, "advertised tool_search") {
		t.Fatal("missing deferred discovery marker")
	}
	handle = deferred[start+6 : start+6+64]
	retrieve := map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "headroom_retrieve", "arguments": map[string]any{"hash": handle}}}
	if status, data := call("POST", "/v1/headroom/mcp", retrieve); status != 200 || gjson.GetBytes(data, "result.isError").Bool() || gjson.GetBytes(data, "result.content.0.text").String() != fresh {
		t.Fatalf("deferred exact retrieval failed: status=%d", status)
	}
	virtualKey = "sk-bf-other-fixture"
	if status, data := call("POST", "/v1/headroom/mcp", retrieve); status != 200 || !gjson.GetBytes(data, "result.isError").Bool() {
		t.Fatalf("deferred cross-owner denial failed: status=%d", status)
	}
	virtualKey = "sk-bf-headroom-fixture"
	request["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] = "code_exec"
	delete(request, "tools") // Protection survives absent declarations and cached originals.
	if status, data := call("POST", "/v1/chat/completions", request); status != 200 {
		t.Fatalf("retrieval continuation=%d %s", status, data)
	}
	mu.Lock()
	untouched := seen[len(seen)-1] == fresh
	mu.Unlock()
	if !untouched {
		t.Fatal("retrieved original was recompressed or shortened from cache")
	}
	t.Logf("gateway -> local Headroom -> fixture provider: %d -> %d bytes; CCR exact retrieval and cross-owner denial, stable replay, runtime disable/re-enable and byte-identical raw SSE verified", len(text), len(first[0]))
}
