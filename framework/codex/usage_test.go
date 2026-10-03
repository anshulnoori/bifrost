package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// resetUsage isolates the process-wide usage cache and clock for one test.
func resetUsage(t *testing.T) *time.Time {
	t.Helper()
	now := time.Now()
	usageMu.Lock()
	usageCache = map[string]usageEntry{}
	usageMu.Unlock()
	usageNow = func() time.Time { return now }
	t.Cleanup(func() {
		usageNow = time.Now
		usageMu.Lock()
		usageCache = map[string]usageEntry{}
		usageMu.Unlock()
	})
	return &now
}

func TestUsageIsAccountScopedAndSanitized(t *testing.T) {
	now := resetUsage(t)
	s, db := testStore(t, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected refresh") })
	for _, id := range []string{"A", "B"} {
		row := insertConnected(t, db, "provider:codex:"+id, id)
		secret, err := seal(envelope{ID: id, Owner: row.Owner, Tokens: tokens{Access: "access-" + id, Account: "account-" + id}})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&row).Updates(map[string]any{"secret": secret, "expires_at": time.Now().Add(time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	s.client.http.Transport = usageTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://chatgpt.com/backend-api/wham/usage" || r.Method != "GET" {
			t.Fatalf("wrong usage endpoint: %s", r.URL)
		}
		id := strings.TrimPrefix(r.Header.Get("ChatGPT-Account-Id"), "account-")
		if r.Header.Get("Authorization") != "Bearer access-"+id {
			t.Fatal("mixed account credentials")
		}
		percent := 23
		if id == "B" {
			percent = 86
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"account_id":"private-account","plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":%d,"limit_window_seconds":18000,"reset_at":1900000000},"secondary_window":null},"access_token":"private-token"}`, percent)))}, nil
	})
	for _, tc := range []struct {
		id   string
		used float64
	}{{"A", 23}, {"B", 86}} {
		u, err := s.Usage(context.Background(), "provider:codex:"+tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if *u.Limits.Primary.UsedPercent != tc.used || u.Limits.Secondary != nil || u.Limits.Primary.ResetAt != 1900000000 {
			t.Fatalf("incorrect usage: %+v", u)
		}
		wire, _ := json.Marshal(u)
		if strings.Contains(string(wire), "private-") {
			t.Fatal("private upstream fields exposed")
		}
	}
	if _, err := s.Usage(context.Background(), "unknown"); err == nil || calls != 2 {
		t.Fatal("unknown owner queried upstream")
	}
	s.client.http.Transport = usageTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("private-token"))}, nil
	})
	// Past the stale limit, so the failure is reported rather than masked by the cache.
	*now = now.Add(usageMaxStale + time.Minute)
	if _, err := s.Usage(context.Background(), "provider:codex:A"); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("upstream failure not sanitized")
	}
}

func TestUsageRespectsRateLimitsAndSharesReads(t *testing.T) {
	now := resetUsage(t)
	s, db := testStore(t, func(http.ResponseWriter, *http.Request) { t.Fatal("unexpected refresh") })
	row := insertConnected(t, db, "provider:codex:A", "A")
	secret, err := seal(envelope{ID: "A", Owner: row.Owner, Tokens: tokens{Access: "access-A", Account: "account-A"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&row).Updates(map[string]any{"secret": secret, "expires_at": time.Now().Add(24 * time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	release := make(chan struct{})
	s.client.http.Transport = usageTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		if limited.Load() {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"allowed":true,"secondary_window":{"used_percent":40,"limit_window_seconds":604800,"reset_at":1900000000}}}`))}, nil
	})
	ctx, owner := context.Background(), "provider:codex:A"
	// Rate limited with nothing cached: fail closed and do not re-ask during Retry-After.
	for i := 0; i < 3; i++ {
		if _, err := s.Usage(ctx, owner); err == nil {
			t.Fatal("rate-limited read reported usage")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("re-asked during Retry-After: %d calls", calls.Load())
	}
	// After Retry-After, concurrent readers (dashboard rows, routing) share one request.
	*now = now.Add(121 * time.Second)
	limited.Store(false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u, err := s.Usage(ctx, owner); err != nil || !u.AboveReserve(25) {
				t.Error("shared read failed", err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 2 {
		t.Fatalf("concurrent reads not shared: %d calls", calls.Load())
	}
	// Fresh readings are served from cache.
	*now = now.Add(usageFresh - time.Second)
	if _, err := s.Usage(ctx, owner); err != nil || calls.Load() != 2 {
		t.Fatal("fresh reading re-requested", err)
	}
	// A later 429 keeps serving the last good reading, then fails closed once too old.
	limited.Store(true)
	*now = now.Add(2 * time.Second)
	if u, err := s.Usage(ctx, owner); err != nil || !u.AboveReserve(25) || calls.Load() != 3 {
		t.Fatal("last good reading not kept during rate limit", err, calls.Load())
	}
	if _, err := s.Usage(ctx, owner); err != nil || calls.Load() != 3 {
		t.Fatal("re-asked during Retry-After", err)
	}
	*now = now.Add(usageMaxStale)
	if _, err := s.Usage(ctx, owner); err == nil {
		t.Fatal("stale reading used for a routing decision")
	}
	if got := retryWait(429, ""); got != usageRateLimitBackoff {
		t.Fatalf("default 429 backoff %s", got)
	}
	if got := retryWait(429, "999999"); got != usageMaxStale {
		t.Fatalf("Retry-After not capped: %s", got)
	}
	if got := retryWait(503, "5"); got != usageErrorBackoff {
		t.Fatalf("non-429 backoff %s", got)
	}
}
