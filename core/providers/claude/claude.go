// Package claude connects native Anthropic Messages to the pinned Claude bridge.
package claude

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/maximhq/bifrost/core/providers/anthropic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/sjson"
)

// ClaudeProvider reuses Anthropic's native passthrough, hooks and usage accounting.
// All other operations are disabled by the embedded provider's allowlist.
type ClaudeProvider struct {
	*anthropic.AnthropicProvider
	token   string
	account func(*schemas.BifrostContext, schemas.Key, string) error
}

// BridgeConfig is shared by inference and dashboard account management. Neither
// path may forward the deployment credential to an arbitrary host.
func BridgeConfig(base string) (string, string, error) {
	if base == "" {
		base = "http://127.0.0.1:8091"
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return "", "", errors.New("claude base_url must be http://127.0.0.1:<bridge-port>")
	}
	token := os.Getenv("CLAUDE_BRIDGE_TOKEN")
	if len(token) < 32 || strings.ContainsAny(token, "\r\n") {
		return "", "", errors.New("claude requires CLAUDE_BRIDGE_TOKEN with at least 32 characters")
	}
	return base, token, nil
}

func New(config *schemas.ProviderConfig, logger schemas.Logger) (*ClaudeProvider, error) {
	if config.CustomProviderConfig != nil || config.ProxyConfig != nil || len(config.NetworkConfig.ExtraHeaders) != 0 {
		return nil, errors.New("claude does not accept custom providers, proxies or extra headers")
	}
	if config.NetworkConfig.MaxRetries != 0 {
		return nil, errors.New("claude requires max_retries=0 because replay can duplicate inference")
	}
	// Runtime wrapper configuration must not become persisted provider settings.
	c := *config
	config = &c
	base, token, err := BridgeConfig(config.NetworkConfig.BaseURL)
	if err != nil {
		return nil, err
	}
	config.NetworkConfig.BaseURL = base
	config.NetworkConfig.AllowPrivateNetwork = true
	// Do not repeat inference after an ambiguous failure.
	config.NetworkConfig.MaxRetries = 0
	config.CustomProviderConfig = &schemas.CustomProviderConfig{
		CustomProviderKey: string(schemas.Claude), BaseProviderType: schemas.Anthropic,
		IsKeyLess:       false,
		AllowedRequests: &schemas.AllowedRequests{Passthrough: true, PassthroughStream: true},
	}
	return &ClaudeProvider{AnthropicProvider: anthropic.NewAnthropicProvider(config, logger), token: token, account: config.ClaudeAccount}, nil
}

func failure(message string) *schemas.BifrostError {
	return &schemas.BifrostError{
		IsBifrostError: true, StatusCode: schemas.Ptr(400), AllowFallbacks: schemas.Ptr(false),
		Error:       &schemas.ErrorField{Message: message},
		ExtraFields: schemas.BifrostErrorExtraFields{Provider: schemas.Claude},
	}
}

func (p *ClaudeProvider) admitted(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughRequest, *schemas.BifrostError) {
	if ctx.Value(schemas.BifrostContextKeyIntegrationType) != "anthropic" || request.Method != "POST" || request.Path != "/v1/messages" || request.RawQuery != "" || request.UpstreamURL != "" {
		return nil, failure("claude supports only the anthropic messages endpoint")
	}
	grant := ctx.Grant()
	if grant == nil || grant.Identity() == nil || grant.Identity().VirtualKey() == nil || grant.Identity().VirtualKey().ID == "" || grant.Access() == nil || !grant.Access().IsModelAllowed(string(schemas.Claude), request.Model) {
		err := failure("claude requires an admitted virtual key with access to the requested model")
		err.StatusCode = schemas.Ptr(403)
		return nil, err
	}
	if key.ID == "" || p.account == nil || p.account(ctx, key, request.Model) != nil {
		err := failure("Claude account is missing, disabled or unauthorized")
		err.StatusCode = schemas.Ptr(403)
		return nil, err
	}
	body, err := sjson.SetBytes(request.Body, "model", request.Model)
	if err != nil {
		return nil, failure("invalid claude messages body")
	}
	r := *request
	r.Body = body
	r.SafeHeaders = map[string]string{
		"Content-Type":            "application/json",
		"X-Claude-Bridge-Token":   p.token,
		"X-Claude-Bridge-Owner":   grant.Identity().VirtualKey().ID,
		"X-Claude-Bridge-Account": key.ID,
	}
	for name, value := range request.SafeHeaders {
		if strings.EqualFold(name, "x-bifrost-claude-session-id") && value != "" {
			return nil, failure("claude requests are independent; session continuation is not supported")
		}
		if (strings.EqualFold(name, "anthropic-beta") || strings.EqualFold(name, "anthropic-version")) && value != "" {
			r.SafeHeaders[name] = value
		}
	}
	return &r, nil
}

func (p *ClaudeProvider) Passthrough(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	r, err := p.admitted(ctx, key, request)
	if err != nil {
		return nil, err
	}
	return p.AnthropicProvider.Passthrough(ctx, schemas.Key{}, r)
}

func (p *ClaudeProvider) PassthroughStream(ctx *schemas.BifrostContext, hook schemas.PostHookRunner, finalize func(context.Context), key schemas.Key, request *schemas.BifrostPassthroughRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	r, err := p.admitted(ctx, key, request)
	if err != nil {
		return nil, err
	}
	return p.AnthropicProvider.PassthroughStream(ctx, hook, finalize, schemas.Key{}, r)
}
