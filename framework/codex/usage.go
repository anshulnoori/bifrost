package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
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
	if row.State != "connected" && row.State != "refreshing" {
		return nil, ErrReconnect
	}
	// OpenAI rate-limits this endpoint, and routing reserves read it on every
	// request. All readers share one reading per connection: refresh at most
	// every usageFresh, back off after failures (honoring Retry-After), and keep
	// serving the last good reading for at most usageMaxStale.
	key := row.ID
	now := usageNow()
	cached := loadUsage(key)
	if cached.value != nil && now.Sub(cached.value.CheckedAt) < usageFresh {
		return cached.value, nil
	}
	if now.Before(cached.retryAt) {
		return staleUsage(cached, now)
	}
	result, err, _ := usageFlight.Do(key, func() (any, error) {
		// Detached so one cancelled caller cannot fail the shared read for others.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		value, wait, err := s.fetchUsage(readCtx, owner, row.ID)
		usageMu.Lock()
		defer usageMu.Unlock()
		entry := usageCache[key]
		if err != nil {
			entry.retryAt = usageNow().Add(wait)
		} else {
			entry = usageEntry{value: value}
		}
		usageCache[key] = entry
		return value, err
	})
	if err != nil {
		return staleUsage(loadUsage(key), usageNow())
	}
	return result.(*Usage), nil
}

const (
	usageFresh            = time.Minute
	usageMaxStale         = time.Hour
	usageErrorBackoff     = time.Minute
	usageRateLimitBackoff = 5 * time.Minute
)

type usageEntry struct {
	value   *Usage
	retryAt time.Time
}

// Process-wide: a Store is constructed per request, so the cache cannot live on it.
var (
	usageMu     sync.Mutex
	usageCache  = map[string]usageEntry{}
	usageFlight singleflight.Group
	usageNow    = time.Now
)

func loadUsage(key string) usageEntry {
	usageMu.Lock()
	defer usageMu.Unlock()
	return usageCache[key]
}

// A reading too old to trust fails closed, so a reserve is never decided on it.
func staleUsage(entry usageEntry, now time.Time) (*Usage, error) {
	if entry.value == nil || now.Sub(entry.value.CheckedAt) > usageMaxStale {
		return nil, errors.New("codex usage unavailable")
	}
	return entry.value, nil
}

// retryWait returns how long to wait before asking again after a failed read.
func retryWait(status int, header string) time.Duration {
	if status != http.StatusTooManyRequests {
		return usageErrorBackoff
	}
	wait := time.Duration(0)
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(header); err == nil {
		wait = at.Sub(usageNow())
	}
	if wait <= 0 {
		wait = usageRateLimitBackoff
	}
	return min(wait, usageMaxStale)
}

func (s *Store) fetchUsage(ctx context.Context, owner, id string) (*Usage, time.Duration, error) {
	access, account, err := s.Credential(ctx, owner, id)
	if err != nil {
		return nil, usageErrorBackoff, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
	if err != nil {
		return nil, usageErrorBackoff, errors.New("invalid codex usage request")
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("ChatGPT-Account-Id", account)
	req.Header.Set("User-Agent", "bifrost")
	response, err := s.client.http.Do(req)
	if err != nil {
		return nil, usageErrorBackoff, errors.New("codex usage service unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, retryWait(response.StatusCode, response.Header.Get("Retry-After")), fmt.Errorf("codex usage returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, usageErrorBackoff, errors.New("invalid codex usage response")
	}
	var result Usage
	if json.Unmarshal(data, &result) != nil {
		return nil, usageErrorBackoff, errors.New("invalid codex usage response")
	}
	result.CheckedAt = usageNow().UTC()
	return &result, 0, nil
}
