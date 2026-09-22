package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"
)

// Usage is the subscription allowance reported by OpenAI, not gateway billing.
// Pointers preserve the distinction between absent limits and exhausted limits.
type UsageWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt       int64    `json:"reset_at"`
}

type UsageLimits struct {
	Allowed      bool         `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Primary      *UsageWindow `json:"primary_window"`
	Secondary    *UsageWindow `json:"secondary_window"`
}

type Usage struct {
	Plan       string       `json:"plan_type"`
	Limits     *UsageLimits `json:"rate_limit"`
	Additional []struct {
		Name    string       `json:"limit_name"`
		Feature string       `json:"metered_feature"`
		Limits  *UsageLimits `json:"rate_limit"`
	} `json:"additional_rate_limits,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// AboveReserve checks only the standard weekly allowance, identified by duration
// rather than position. Overall allowed/limit_reached can reflect the five-hour
// window and do not govern this reserve; OpenAI still enforces its own limits.
func (u *Usage) AboveReserve(reserve float64) bool {
	if u == nil || u.Limits == nil || math.IsNaN(reserve) || reserve < 0 || reserve > 100 {
		return false
	}
	known := false
	for _, window := range []*UsageWindow{u.Limits.Primary, u.Limits.Secondary} {
		if window == nil || window.WindowSeconds != 7*24*60*60 {
			continue
		}
		if window.UsedPercent == nil || math.IsNaN(*window.UsedPercent) || *window.UsedPercent < 0 || *window.UsedPercent > 100 {
			return false
		}
		known = true
		if 100-*window.UsedPercent <= reserve {
			return false
		}
	}
	return known
}

func (s *Store) Usage(ctx context.Context, owner string) (*Usage, error) {
	row, err := s.Current(ctx, owner)
	if err != nil {
		return nil, err
	}
	access, account, err := s.Credential(ctx, owner, row.ID)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, errors.New("invalid codex usage request")
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("ChatGPT-Account-Id", account)
	req.Header.Set("User-Agent", "bifrost")
	response, err := s.client.http.Do(req)
	if err != nil {
		return nil, errors.New("codex usage service unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("codex usage returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid codex usage response")
	}
	var result Usage
	if json.Unmarshal(data, &result) != nil {
		return nil, errors.New("invalid codex usage response")
	}
	result.CheckedAt = time.Now().UTC()
	return &result, nil
}
