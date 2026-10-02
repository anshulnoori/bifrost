// Package claude manages subscription accounts in the local native bridge.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	provider "github.com/maximhq/bifrost/core/providers/claude"
)

type Connection struct {
	ID               string `json:"id,omitempty"`
	State            string `json:"state"`
	Email            string `json:"email,omitempty"`
	AuthorizationURL string `json:"authorization_url,omitempty"`
	IntervalSeconds  int    `json:"interval_seconds,omitempty"`
}

type Client struct{ base, token string }

var accountHTTP = &http.Client{
	Timeout: 40 * time.Second, Transport: &http.Transport{Proxy: nil, IdleConnTimeout: 30 * time.Second},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func NewClient(base string) (*Client, error) {
	base, token, err := provider.BridgeConfig(base)
	if err != nil {
		return nil, err
	}
	return &Client{base: base, token: token}, nil
}

var accountID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

// Return only display metadata. OAuth tokens and raw native error text never
// cross this boundary into configuration, dashboard responses, or logs.
func (c *Client) Do(ctx context.Context, account, method, action string, body []byte) (*Connection, int, error) {
	if !accountID.MatchString(account) || action != "" && action != "/start" && action != "/code" {
		return nil, 400, errors.New("invalid Claude account request")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/accounts/"+account+action, bytes.NewReader(body))
	if err != nil {
		return nil, 502, errors.New("Claude account service unavailable")
	}
	req.Header.Set("X-Claude-Bridge-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := accountHTTP.Do(req)
	if err != nil {
		return nil, 502, errors.New("Claude account service unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		status := response.StatusCode
		if status != 400 && status != 404 && status != 409 && status != 429 {
			status = 502
		}
		return nil, status, errors.New("Claude account operation failed; check the code or reconnect")
	}
	var result Connection
	if err := json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&result); err != nil {
		return nil, 502, errors.New("invalid Claude account response")
	}
	switch result.State {
	case "disconnected", "pending", "connecting", "connected", "expired", "reconnect_required":
	default:
		return nil, 502, errors.New("invalid Claude account state")
	}
	if result.AuthorizationURL != "" {
		u, err := url.Parse(result.AuthorizationURL)
		if err != nil || u.Scheme != "https" || u.Host != "claude.com" || u.Path != "/cai/oauth/authorize" || u.User != nil || u.Fragment != "" {
			return nil, 502, errors.New("invalid Claude authorization URL")
		}
	}
	return &result, 200, nil
}
