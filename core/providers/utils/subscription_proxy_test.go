package utils

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// connectRecorder is an HTTP proxy that records CONNECT targets and refuses them.
func connectRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var targets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targets = append(targets, r.Method+" "+r.Host)
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), targets...)
	}
}

func TestSubscriptionProxyUnsetKeepsProviderSetting(t *testing.T) {
	t.Setenv(SubscriptionProxyEnv, "")
	configured := &schemas.ProxyConfig{Type: schemas.Socks5Proxy, URL: schemas.NewSecretVar("socks5://configured:1080")}
	if got := SubscriptionProxyConfig(configured); got != configured {
		t.Fatalf("unset env must keep the provider proxy, got %+v", got)
	}
	if SubscriptionProxyConfig(nil) != nil {
		t.Fatal("unset env must not invent a proxy")
	}
	if SubscriptionTransport() != nil {
		t.Fatal("unset env must keep the default transport")
	}
}

func TestSubscriptionProxyOverridesProviderAndRoutesBothClients(t *testing.T) {
	proxy, targets := connectRecorder(t)
	t.Setenv(SubscriptionProxyEnv, proxy.URL)

	configured := &schemas.ProxyConfig{Type: schemas.Socks5Proxy, URL: schemas.NewSecretVar("socks5://configured:1080")}
	got := SubscriptionProxyConfig(configured)
	if got.Type != schemas.HTTPProxy || got.URL.GetValue() != proxy.URL {
		t.Fatalf("env must override the provider proxy, got %+v", got)
	}

	// Provider inference path: fasthttp with the provider proxy dialer.
	client := ConfigureProxy(&fasthttp.Client{}, got, nil)
	req, resp := fasthttp.AcquireRequest(), fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)
	req.SetRequestURI("https://chatgpt.com/backend-api/codex/responses")
	if err := client.Do(req, resp); err == nil {
		t.Fatal("a refused CONNECT must fail the request")
	}

	// Account path: net/http OAuth and usage clients.
	httpClient := &http.Client{Transport: SubscriptionTransport()}
	if _, err := httpClient.Get("https://auth.openai.com/oauth/token"); err == nil {
		t.Fatal("a refused CONNECT must fail the request")
	}

	want := []string{"CONNECT chatgpt.com:443", "CONNECT auth.openai.com:443"}
	gotTargets := targets()
	if len(gotTargets) != len(want) || gotTargets[0] != want[0] || gotTargets[1] != want[1] {
		t.Fatalf("proxy saw %v, want %v", gotTargets, want)
	}
}

func TestSubscriptionTransportFailsClosedOnInvalidURL(t *testing.T) {
	for _, raw := range []string{"bifrost-proxy:3128", "socks5://proxy:1080", "http://"} {
		t.Setenv(SubscriptionProxyEnv, raw)
		direct := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Errorf("%q: request bypassed the proxy", raw)
		}))
		_, err := (&http.Client{Transport: SubscriptionTransport()}).Get(direct.URL)
		direct.Close()
		if err == nil {
			t.Fatalf("%q: invalid proxy must fail the request", raw)
		}
	}
}
