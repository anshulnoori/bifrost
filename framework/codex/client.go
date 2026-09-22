// Package codex implements subscription credential lifecycle independently of gateway authentication.
package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	Issuer   = "https://auth.openai.com"
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

var (
	ErrPending   = errors.New("codex authorization pending")
	ErrReconnect = errors.New("codex authorization requires reconnect")
	ErrBusy      = errors.New("codex credential operation in progress")
	ErrNotFound  = errors.New("codex connection not found")
)

// Client uses the device flow implemented by the official Codex CLI. This is
// not a claim that OpenAI offers a supported OAuth contract for hosted gateways.
// The issuer is deliberately not configurable by API callers.
type Client struct {
	http   *http.Client
	issuer string
}

func NewClient() *Client {
	return &Client{http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, issuer: Issuer}
}

type device struct {
	ID         string          `json:"device_auth_id"`
	Code       string          `json:"user_code"`
	LegacyCode string          `json:"usercode,omitempty"`
	Interval   json.RawMessage `json:"interval"`
}

type tokens struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token"`
	ID        string    `json:"id_token"`
	ExpiresIn int64     `json:"expires_in"`
	Account   string    `json:"account"`
	ExpiresAt time.Time `json:"expires_at"`
}

// request never exposes an upstream body or transport error: either can contain
// credentials. It also bounds successful responses and refuses redirects.
func (c *Client) request(ctx context.Context, path, contentType string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer+path, bytes.NewReader(body))
	if err != nil {
		return 0, errors.New("invalid codex auth request")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("codex auth service unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("codex auth service returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return resp.StatusCode, errors.New("invalid codex auth response")
	}
	if json.Unmarshal(data, out) != nil {
		return resp.StatusCode, errors.New("invalid codex auth response")
	}
	return resp.StatusCode, nil
}

func (c *Client) start(ctx context.Context) (device, time.Duration, error) {
	var d device
	body, _ := json.Marshal(map[string]string{"client_id": ClientID})
	_, err := c.request(ctx, "/api/accounts/deviceauth/usercode", "application/json", body, &d)
	if err != nil {
		return d, 0, err
	}
	if d.Code == "" {
		d.Code = d.LegacyCode
	}
	if d.ID == "" || d.Code == "" {
		return d, 0, errors.New("incomplete codex device authorization")
	}
	interval, err := strconv.Atoi(strings.Trim(string(d.Interval), "\""))
	if err != nil || interval < 1 {
		interval = 5
	}
	if interval > 60 {
		interval = 60
	}
	return d, time.Duration(interval) * time.Second, nil
}

func (c *Client) poll(ctx context.Context, d device) (tokens, error) {
	body, _ := json.Marshal(map[string]string{"device_auth_id": d.ID, "user_code": d.Code})
	var code struct {
		Code     string `json:"authorization_code"`
		Verifier string `json:"code_verifier"`
	}
	status, err := c.request(ctx, "/api/accounts/deviceauth/token", "application/json", body, &code)
	if status == 403 || status == 404 {
		return tokens{}, ErrPending
	}
	if err != nil {
		return tokens{}, err
	}
	if code.Code == "" || code.Verifier == "" {
		return tokens{}, errors.New("incomplete codex authorization response")
	}
	return c.exchange(ctx, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {ClientID},
		"code": {code.Code}, "code_verifier": {code.Verifier},
		"redirect_uri": {c.issuer + "/deviceauth/callback"},
	}, "")
}

func (c *Client) refresh(ctx context.Context, old tokens) (tokens, error) {
	t, err := c.exchange(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {old.Refresh}}, old.Refresh)
	if err == nil && t.Account != old.Account {
		return tokens{}, errors.New("codex account changed during refresh")
	}
	return t, err
}

func (c *Client) exchange(ctx context.Context, values url.Values, previousRefresh string) (tokens, error) {
	var t tokens
	_, err := c.request(ctx, "/oauth/token", "application/x-www-form-urlencoded", []byte(values.Encode()), &t)
	if err != nil {
		return t, err
	}
	if t.Refresh == "" {
		t.Refresh = previousRefresh
	}
	if t.Access == "" || t.Refresh == "" {
		return tokens{}, errors.New("incomplete codex token response")
	}
	// Claims are metadata obtained only from the TLS-authenticated token endpoint,
	// never proof of gateway identity or acceptance of a caller-supplied JWT.
	parts := strings.Split(t.Access, ".")
	if len(parts) != 3 {
		return tokens{}, errors.New("invalid codex access token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokens{}, errors.New("invalid codex access token")
	}
	var claims struct {
		Expires int64 `json:"exp"`
		Auth    struct {
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Auth.Account == "" || claims.Expires <= time.Now().Unix()+30 {
		return tokens{}, errors.New("codex access token missing account or valid expiry")
	}
	t.Account = claims.Auth.Account
	t.ExpiresAt = time.Unix(claims.Expires, 0)
	if t.ExpiresIn > 0 && time.Now().Add(time.Duration(t.ExpiresIn)*time.Second).Before(t.ExpiresAt) {
		t.ExpiresAt = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	t.ID = "" // No ID token is needed for inference or persistence.
	return t, nil
}
