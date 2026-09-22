// This harness runs the real gateway against a local TLS OAuth/inference fixture.
// No production endpoint override, real credential, or paid provider is needed.
package codexe2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthThroughGateway(t *testing.T) {
	binary := os.Getenv("BIFROST_CODEX_TEST_BINARY")
	if binary == "" {
		t.Skip("set BIFROST_CODEX_TEST_BINARY to the built gateway; see tests/codex/run.sh")
	}
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Codex local test only"}, DNSNames: []string{"auth.openai.com", "chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caPath := filepath.Join(dir, "fixture-ca.pem")
	if err = os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	claims := fmt.Sprintf(`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`, time.Now().Add(time.Hour).Unix())
	token := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".fixture"
	var refreshes, inference atomic.Int32
	fixture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			fmt.Fprint(w, `{"device_auth_id":"fixture-device","user_code":"TEST-ONLY","interval":"1"}`)
		case "/api/accounts/deviceauth/token":
			fmt.Fprint(w, `{"authorization_code":"fixture-code","code_verifier":"fixture-verifier","code_challenge":"fixture-challenge"}`)
		case "/oauth/token":
			_ = r.ParseForm()
			expiry := 1
			refresh := "fixture-refresh-initial"
			if r.Form.Get("grant_type") == "refresh_token" {
				if r.Form.Get("refresh_token") != "fixture-refresh-initial" {
					t.Error("wrong refresh token")
				}
				refreshes.Add(1)
				expiry = 3600
				refresh = "fixture-refresh-rotated"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "refresh_token": refresh, "expires_in": expiry})
		case "/backend-api/codex/models":
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("catalog credential isolation failed")
			}
			fmt.Fprint(w, `{"models":[{"slug":"gpt-5.3-codex","display_name":"Fixture Codex","context_window":123456}]}`)
		case "/backend-api/codex/responses":
			inference.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
				t.Error("upstream credential isolation failed")
			}
			var body struct {
				Model  string
				Stream bool
				Store  bool
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "gpt-5.3-codex" || !body.Stream || body.Store {
				t.Error("invalid subscription request")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"model\":\"gpt-5.3-codex\"}}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"fixture answer\"}\n\n")
			fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"model\":\"gpt-5.3-codex\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"fixture answer\"}]}],\"usage\":{\"input_tokens\":123,\"output_tokens\":17,\"total_tokens\":140}}}\n\n")
		default:
			http.Error(w, "fixture refuses unknown endpoint", 404)
		}
	})
	// A closed local proxy accepts only the two fixed production hosts. The test
	// CA is trusted by this child process only, never installed system-wide.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || (r.Host != "auth.openai.com:443" && r.Host != "chatgpt.com:443") {
			http.Error(w, "test egress denied", 403)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		_ = secure.SetDeadline(time.Now().Add(30 * time.Second))
		reader := bufio.NewReader(secure)
		for {
			req, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			recorder := httptest.NewRecorder()
			fixture.ServeHTTP(recorder, req)
			_, _ = io.Copy(io.Discard, req.Body)
			_ = req.Body.Close()
			resp := recorder.Result()
			resp.ContentLength = int64(recorder.Body.Len())
			if err = resp.Write(secure); err != nil {
				return
			}
			_ = resp.Body.Close()
			if req.Close {
				return
			}
		}
	}))
	defer proxy.Close()
	pricingPath := filepath.Join(dir, "pricing.json")
	if err = os.WriteFile(pricingPath, []byte(`{"gpt-5.3-codex":{"litellm_provider":"openai","mode":"responses","input_cost_per_token":0.000002,"output_cost_per_token":0.000009}}`), 0600); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"encryption_key":"fixture-only-encryption-key-32bytes","framework":{"pricing":{"pricing_url":%q,"model_parameters_url":%q,"mcp_library_sync_interval":0}},"client":{"enable_logging":false,"enforce_auth_on_inference":true},"plugins":[{"name":"telemetry","enabled":false}],"config_store":{"enabled":true,"type":"sqlite","config":{"path":%q}},"providers":{"codex":{"keys":[],"network_config":{"allow_private_network":true},"proxy_config":{"type":"http","url":%q,"ca_cert_pem":%q}}},"governance":{"virtual_keys":[{"id":"owner-a","name":"owner-a","value":"sk-bf-fixture-owner-a","is_active":true,"provider_configs":[{"provider":"codex","allowed_models":["*"],"key_ids":["*"],"weight":1}]},{"id":"owner-b","name":"owner-b","value":"sk-bf-fixture-owner-b","is_active":true,"provider_configs":[{"provider":"codex","allowed_models":["*"],"key_ids":["*"],"weight":1}]}]}}`, "file://"+pricingPath, "file://"+pricingPath, filepath.Join(dir, "config.db"), proxy.URL, string(ca))
	plugin := os.Getenv("BIFROST_CODEX_TEST_PLUGIN")
	if plugin != "" {
		config = strings.Replace(config, `"plugins":[`, fmt.Sprintf(`"plugins":[{"name":"headroom","enabled":true,"path":%q,"placement":"post_builtin","config":{"enabled":false}},`, plugin), 1)
	}
	config = strings.Replace(config, `"virtual_keys":[`, `"virtual_keys":[{"id":"budget-blocked","name":"budget-blocked","value":"sk-bf-fixture-budget-blocked","is_active":true,"budgets":[{"id":"zero-budget","max_limit":0,"reset_duration":"1d"}],"provider_configs":[{"provider":"codex","allowed_models":["*"],"key_ids":["*"],"weight":1}]},`, 1)
	if err = os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-app-dir", dir, "-host", "127.0.0.1", "-port", port)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "HTTPS_PROXY=" + proxy.URL, "HTTP_PROXY=" + proxy.URL, "NO_PROXY=127.0.0.1,localhost", "SSL_CERT_FILE=" + caPath}
	logfile, err := os.Create(filepath.Join(dir, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logfile.Close()
	cmd.Stdout, cmd.Stderr = logfile, logfile
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(logfile.Name())
			t.Log(string(data))
		}
	}()
	client := &http.Client{Timeout: 10 * time.Second}
	base := "http://127.0.0.1:" + port
	for deadline := time.Now().Add(40 * time.Second); ; {
		resp, err := client.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	call := func(method, path, owner, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if strings.HasPrefix(path, "/openai_passthrough/") {
			req.Header.Set("x-model-provider", "codex")
		}
		if owner != "" {
			req.Header.Set("x-bf-vk", "sk-bf-fixture-"+owner)
		}
		req.Header.Set("Authorization", "Bearer eyJ.forged.jwt")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte("fixture-refresh")) {
			t.Fatal("credential leaked into response")
		}
		return resp.StatusCode, data
	}
	status, data := call("POST", "/api/codex/connections", "owner-a", "")
	if status != 200 {
		t.Fatalf("start=%d %s", status, data)
	}
	var login struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if json.Unmarshal(data, &login) != nil || login.ID == "" {
		t.Fatal("missing connection ID")
	}
	time.Sleep(1100 * time.Millisecond)
	status, data = call("POST", "/api/codex/connections/"+login.ID+"/poll", "owner-a", "")
	if status != 200 || !bytes.Contains(data, []byte(`"state":"connected"`)) {
		t.Fatalf("poll=%d %s", status, data)
	}
	request := `{"model":"codex/gpt-5.3-codex","input":"hello"}`
	for _, owner := range []string{"", "owner-b"} {
		status, data = call("POST", "/v1/responses", owner, request)
		if status < 400 {
			t.Fatalf("unauthorized inference accepted for %q: %s", owner, data)
		}
	}
	if inference.Load() != 0 {
		t.Fatal("unadmitted request reached upstream")
	}
	status, data = call("POST", "/v1/responses", "budget-blocked", request)
	if status < 400 || !bytes.Contains(bytes.ToLower(data), []byte("budget")) || inference.Load() != 0 {
		t.Fatalf("budget admission bypassed: %d %s", status, data)
	}
	status, data = call("POST", "/v1/responses", "owner-a", request)
	if status != 200 || !bytes.Contains(data, []byte("fixture answer")) || !bytes.Contains(data, []byte(`"total_tokens":140`)) {
		t.Fatalf("responses=%d %s", status, data)
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh count=%d", refreshes.Load())
	}
	status, data = call("POST", "/v1/chat/completions", "owner-a", `{"model":"codex/gpt-5.3-codex","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if status != 200 || !bytes.Contains(data, []byte("fixture answer")) || !bytes.Contains(data, []byte("[DONE]")) {
		t.Fatalf("chat stream=%d %s", status, data)
	}
	if refreshes.Load() != 1 {
		t.Fatal("rotated token not persisted")
	}
	for _, stream := range []bool{true, false} {
		status, data = call("POST", "/openai_passthrough/v1/responses", "owner-a", fmt.Sprintf(`{"model":"gpt-5.3-codex","input":"hello","stream":%t}`, stream))
		if status != 200 || !bytes.Contains(data, []byte("fixture answer")) {
			t.Fatalf("raw responses stream=%t: %d %s", stream, status, data)
		}
		if stream && !bytes.Contains(data, []byte(`"type":"response.completed"`)) || !stream && !json.Valid(data) {
			t.Fatalf("raw response framing stream=%t: %s", stream, data)
		}
	}
	status, data = call("GET", "/v1/models?provider=codex", "owner-a", "")
	if status != 200 || !bytes.Contains(data, []byte("codex/gpt-5.3-codex")) {
		t.Fatalf("catalog=%d %s", status, data)
	}
	status, data = call("GET", "/v1/models?provider=codex", "owner-b", "")
	if bytes.Contains(data, []byte("codex/gpt-5.3-codex")) {
		t.Fatalf("another owner received a cached catalog: %d %s", status, data)
	}
	status, _ = call("DELETE", "/api/codex/connections/"+login.ID, "owner-b", "")
	if status != 404 {
		t.Fatalf("other owner disconnect=%d", status)
	}
	status, data = call("DELETE", "/api/codex/connections/"+login.ID, "owner-a", "")
	if status != 200 {
		t.Fatalf("disconnect=%d %s", status, data)
	}
	before := inference.Load()
	status, _ = call("POST", "/v1/responses", "owner-a", request)
	if status < 400 || inference.Load() != before {
		t.Fatal("disconnected account reached upstream")
	}
	for _, name := range []string{"config.db", "config.db-wal", "gateway.log"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		if bytes.Contains(data, []byte(token)) || bytes.Contains(data, []byte("fixture-refresh")) {
			t.Fatalf("plaintext credential persisted in %s", name)
		}
		if name == "gateway.log" && plugin != "" && !bytes.Contains(data, []byte("plugin status: headroom - active")) {
			t.Fatal("Headroom native plugin failed to load")
		}
	}
	t.Log("OAuth onboarding, rotated refresh, Responses unary, Chat SSE, usage, forged JWT rejection, owner isolation and disconnect passed through real gateway")
}
