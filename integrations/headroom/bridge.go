// Package main implements the native Bifrost plugin ABI. Compression remains
// entirely in the external Headroom service; this package owns policy and fidelity.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
)

// Config is also described by config.schema.json. Secrets are environment names,
// never literal values: Bifrost persists and displays custom plugin configuration.
type Config struct {
	Enabled          bool   `json:"enabled"`
	Scope            string `json:"scope"`
	ProjectID        string `json:"project_id"`
	VirtualKeyID     string `json:"virtual_key_id"`
	Endpoint         string `json:"endpoint"`
	TokenEnv         string `json:"token_env"`
	ModalKeyEnv      string `json:"modal_key_env"`
	ModalSecretEnv   string `json:"modal_secret_env"`
	ScopeKeyEnv      string `json:"scope_key_env"`
	FailurePolicy    string `json:"failure_policy"`
	TimeoutMS        int    `json:"timeout_ms"`
	MaxBodyBytes     int64  `json:"max_body_bytes"`
	MinTextBytes     int    `json:"min_text_bytes"`
	CCR              bool   `json:"ccr"`
	MetricsAddress   string `json:"metrics_address"`
	MetricsTokenEnv  string `json:"metrics_token_env"`
	RetentionSeconds int    `json:"retention_seconds"`
}

type bridge struct {
	config      Config
	client      *http.Client
	token       string
	key         []byte
	modalKey    string
	modalSecret string
}

func newBridge(config Config) (*bridge, error) {
	if config.Scope != "" && config.Scope != "gateway" && config.Scope != "restricted" {
		return nil, errors.New("scope must be gateway or restricted")
	}
	if config.CCR {
		return nil, errors.New("ccr is not supported: internal tools and continuations cannot be safely exposed")
	}
	if config.FailurePolicy == "" {
		config.FailurePolicy = "open"
	}
	if config.FailurePolicy != "open" && config.FailurePolicy != "closed" {
		return nil, errors.New("failure_policy must be open or closed")
	}
	if config.TimeoutMS == 0 {
		config.TimeoutMS = 2000
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = 4 << 20
	}
	if config.MinTextBytes == 0 {
		config.MinTextBytes = 1024
	}
	if config.TimeoutMS < 1 || config.TimeoutMS > 30000 || config.MaxBodyBytes < 1024 || config.MaxBodyBytes > 16<<20 || config.MinTextBytes < 1 {
		return nil, errors.New("invalid timeout or size limits")
	}
	b := &bridge{config: config}
	if !config.Enabled {
		return b, nil
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("endpoint must be an HTTP(S) service origin without credentials, path, query or fragment")
	}
	b.config.Endpoint = strings.TrimRight(config.Endpoint, "/")
	b.token, b.key = os.Getenv(config.TokenEnv), []byte(os.Getenv(config.ScopeKeyEnv))
	if len(b.token) < 32 || len(b.key) < 32 {
		return nil, errors.New("token_env and scope_key_env must resolve to secrets of at least 32 bytes")
	}
	if config.ModalKeyEnv != "" || config.ModalSecretEnv != "" {
		b.modalKey, b.modalSecret = os.Getenv(config.ModalKeyEnv), os.Getenv(config.ModalSecretEnv)
		if u.Scheme != "https" || b.modalKey == "" || b.modalSecret == "" {
			return nil, errors.New("modal authentication requires HTTPS and both credential environment references")
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Do not disclose tool results to ambient HTTP proxies.
	b.client = &http.Client{Transport: transport, Timeout: time.Duration(config.TimeoutMS) * time.Millisecond,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return b, nil
}

// scopeID uses length-delimited JSON, not concatenation. Model/provider are part of
// scope so fallback attempts can never reuse another route's compression state.
func (b *bridge) scopeID(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	mac := hmac.New(sha256.New, b.key)
	mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

type estimate struct {
	Before int `json:"before_estimated_tokens"`
	After  int `json:"after_estimated_tokens"`
}

type compressReply struct {
	Messages    []json.RawMessage `json:"messages"`
	Before      *int              `json:"tokens_before"`
	After       *int              `json:"tokens_after"`
	CCRHashes   []string          `json:"ccr_hashes"`
	Obligations []string          `json:"obligations"`
}

// compress sends ONLY text tool outputs, never credentials, system prompts, tool
// definitions, encrypted reasoning or multimedia. There is no session/retrieval
// state: scope is an opaque attribution tag, not a client-supplied authority.
func (b *bridge) compress(ctx context.Context, model, scope string, texts []string) ([]string, estimate, error) {
	messages := make([]map[string]string, len(texts))
	for i, text := range texts {
		messages[i] = map[string]string{"role": "tool", "tool_call_id": fmt.Sprintf("slot-%d", i), "content": text}
	}
	body, err := schemas.MarshalSorted(map[string]any{
		"model": model, "messages": messages,
		"config":  map[string]any{"protect_recent": 0, "compress_user_messages": false},
		"gateway": map[string]any{"can_redrive": false, "can_relay_response": false, "session_affinity": false, "plugin_version": "bifrost-headroom/1"},
	})
	if err != nil {
		return nil, estimate{}, err
	}
	if int64(len(body)) > b.config.MaxBodyBytes {
		return nil, estimate{}, errors.New("compression request exceeds limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.config.Endpoint+"/v1/compress", bytes.NewReader(body))
	if err != nil {
		return nil, estimate{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Headroom-Proxy-Token", b.token)
	req.Header.Set("X-Headroom-Project", scope)
	if b.modalKey != "" {
		req.Header.Set("Modal-Key", b.modalKey)
		req.Header.Set("Modal-Secret", b.modalSecret)
	}
	response, err := b.client.Do(req)
	if err != nil {
		return nil, estimate{}, errors.New("headroom unavailable or request cancelled")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, estimate{}, fmt.Errorf("headroom returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, b.config.MaxBodyBytes+1))
	if err != nil || int64(len(data)) > b.config.MaxBodyBytes {
		return nil, estimate{}, errors.New("headroom response unreadable or exceeds limit")
	}
	var result compressReply
	if err = json.Unmarshal(data, &result); err != nil || len(result.Messages) != len(messages) || result.Before == nil || result.After == nil || *result.Before < 0 || *result.After < 0 || *result.After > *result.Before || len(result.CCRHashes) != 0 || len(result.Obligations) != 0 {
		return nil, estimate{}, errors.New("invalid headroom response or unsupported obligation")
	}
	out := make([]string, len(texts))
	for i, raw := range result.Messages {
		var msg map[string]any
		if json.Unmarshal(raw, &msg) != nil {
			return nil, estimate{}, errors.New("invalid compressed message")
		}
		text, ok := msg["content"].(string)
		if !ok || text == "" || len(text) > len(texts[i]) || strings.Contains(text, "<<ccr:") || strings.Contains(text, "headroom_retrieve") {
			return nil, estimate{}, errors.New("invalid text or residual ccr reference")
		}
		msg["content"] = texts[i]
		original := map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("slot-%d", i), "content": texts[i]}
		if !reflect.DeepEqual(msg, original) {
			return nil, estimate{}, errors.New("headroom changed protected message fields")
		}
		out[i] = text
	}
	return out, estimate{Before: *result.Before, After: *result.After}, nil
}
