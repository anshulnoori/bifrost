package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type usageTransport func(*http.Request) (*http.Response, error)

func (f usageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUsageIsAccountScopedAndSanitized(t *testing.T) {
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
	if _, err := s.Usage(context.Background(), "provider:codex:A"); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal("upstream failure not sanitized")
	}
}
