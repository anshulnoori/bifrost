package utils

import (
	"errors"
	"net/http"
	"net/url"
	"os"

	"github.com/maximhq/bifrost/core/schemas"
)

// SubscriptionProxyEnv names the deployment's egress proxy for subscription
// providers (Codex, ChatGPT): an http://host:port URL. When set, it applies to
// their inference and account traffic and overrides any provider proxy setting,
// so all subscription traffic leaves from one address.
const SubscriptionProxyEnv = "BIFROST_SUBSCRIPTION_PROXY"

// SubscriptionProxyConfig returns the subscription egress proxy, or configured
// when SubscriptionProxyEnv is unset.
func SubscriptionProxyConfig(configured *schemas.ProxyConfig) *schemas.ProxyConfig {
	raw := os.Getenv(SubscriptionProxyEnv)
	if raw == "" {
		return configured
	}
	return &schemas.ProxyConfig{Type: schemas.HTTPProxy, URL: schemas.NewSecretVar(raw)}
}

// SubscriptionTransport returns a net/http transport for subscription account
// traffic (OAuth, usage) that uses the subscription egress proxy, or nil when
// SubscriptionProxyEnv is unset. An invalid URL fails every request rather
// than falling back to direct egress.
func SubscriptionTransport() http.RoundTripper {
	raw := os.Getenv(SubscriptionProxyEnv)
	if raw == "" {
		return nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxy, err := url.Parse(raw)
	if err != nil || proxy.Scheme != "http" || proxy.Host == "" {
		transport.Proxy = func(*http.Request) (*url.URL, error) {
			return nil, errors.New("invalid " + SubscriptionProxyEnv)
		}
		return transport
	}
	transport.Proxy = http.ProxyURL(proxy)
	return transport
}
