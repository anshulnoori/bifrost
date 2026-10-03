// Package claude manages subscription accounts in the local native bridge.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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

// UsageWindow is one subscription allowance window. Utilization is percent used.
type UsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at,omitempty"`
}

type ModelUsageWindow struct {
	Name string `json:"name"`
	UsageWindow
}

// Usage is the Claude subscription allowance, not gateway billing. Only window
// percentages and reset times cross this boundary; spend and identity data do not.
type Usage struct {
	FiveHour       *UsageWindow       `json:"five_hour,omitempty"`
	SevenDay       *UsageWindow       `json:"seven_day,omitempty"`
	SevenDayOpus   *UsageWindow       `json:"seven_day_opus,omitempty"`
	SevenDaySonnet *UsageWindow       `json:"seven_day_sonnet,omitempty"`
	Models         []ModelUsageWindow `json:"models,omitempty"`
	// CheckedAt is when the bridge last read Anthropic, which may predate this request.
	CheckedAt *string `json:"checked_at,omitempty"`
}

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

func validWindow(w *UsageWindow) *UsageWindow {
	if w == nil || w.Utilization == nil || math.IsNaN(*w.Utilization) || *w.Utilization < 0 || *w.Utilization > 1000 {
		return nil
	}
	if w.ResetsAt != nil {
		if _, err := time.Parse(time.RFC3339, *w.ResetsAt); err != nil {
			w.ResetsAt = nil
		}
	}
	return w
}

// One malformed window must not hide the others.
func parseWindow(raw json.RawMessage) *UsageWindow {
	var w UsageWindow
	if len(raw) == 0 || json.Unmarshal(raw, &w) != nil {
		return nil
	}
	return validWindow(&w)
}

// Usage reads one account's subscription allowance through its native worker.
func (c *Client) Usage(ctx context.Context, account string) (*Usage, int, error) {
	if !accountID.MatchString(account) {
		return nil, 400, errors.New("invalid Claude account request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/accounts/"+account+"/usage", nil)
	if err != nil {
		return nil, 502, errors.New("Claude usage unavailable")
	}
	req.Header.Set("X-Claude-Bridge-Token", c.token)
	response, err := accountHTTP.Do(req)
	if err != nil {
		return nil, 502, errors.New("Claude usage unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == 409 {
		return nil, 409, errors.New("Claude account is not connected")
	}
	if response.StatusCode != 200 {
		return nil, 502, errors.New("Claude usage unavailable")
	}
	var raw struct {
		FiveHour       json.RawMessage   `json:"five_hour"`
		SevenDay       json.RawMessage   `json:"seven_day"`
		SevenDayOpus   json.RawMessage   `json:"seven_day_opus"`
		SevenDaySonnet json.RawMessage   `json:"seven_day_sonnet"`
		Limits         []json.RawMessage `json:"limits"`
		CheckedAt      string            `json:"checked_at"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&raw); err != nil {
		return nil, 502, errors.New("invalid Claude usage response")
	}
	result := &Usage{FiveHour: parseWindow(raw.FiveHour), SevenDay: parseWindow(raw.SevenDay),
		SevenDayOpus: parseWindow(raw.SevenDayOpus), SevenDaySonnet: parseWindow(raw.SevenDaySonnet)}
	if _, err := time.Parse(time.RFC3339, raw.CheckedAt); err == nil {
		result.CheckedAt = &raw.CheckedAt
	}
	for _, item := range raw.Limits {
		var limit struct {
			Kind    string   `json:"kind"`
			Percent *float64 `json:"percent"`
			Resets  *string  `json:"resets_at"`
			Scope   struct {
				Model struct {
					Name string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		}
		if json.Unmarshal(item, &limit) != nil {
			continue
		}
		name := limit.Scope.Model.Name
		if limit.Kind != "weekly_scoped" || name == "" || len(name) > 64 || len(result.Models) == 8 {
			continue
		}
		if w := validWindow(&UsageWindow{Utilization: limit.Percent, ResetsAt: limit.Resets}); w != nil {
			result.Models = append(result.Models, ModelUsageWindow{Name: name, UsageWindow: *w})
		}
	}
	return result, 200, nil
}

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
