package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

type claudeReserveStore struct {
	configstore.ConfigStore
	base string
	keys map[string]*schemas.Key
}

func (s claudeReserveStore) GetProvider(context.Context, schemas.ModelProvider) (*tables.TableProvider, error) {
	return &tables.TableProvider{NetworkConfig: &schemas.NetworkConfig{BaseURL: s.base}}, nil
}

func (s claudeReserveStore) GetProviderKey(_ context.Context, provider schemas.ModelProvider, id string) (*schemas.Key, error) {
	if provider != schemas.Claude {
		return nil, nil
	}
	return s.keys[id], nil
}

func TestClaudeReserveFiltersAccountsByWeeklyUsage(t *testing.T) {
	const token = "synthetic-bridge-token-for-tests-only"
	t.Setenv("CLAUDE_BRIDGE_TOKEN", token)
	var reads atomic.Int32
	weekly := map[string]float64{"roomy": 40, "tight": 90}
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Claude-Bridge-Token") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reads.Add(1)
		account := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/usage")
		used, ok := weekly[account]
		if !ok {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"seven_day": map[string]any{"utilization": used}})
	}))
	defer bridge.Close()
	reserve := func(p float64) *float64 { return &p }
	enabled := true
	keys := map[string]*schemas.Key{
		"roomy":     {ID: "roomy", Enabled: &enabled, CodexReservePercent: reserve(25)},
		"tight":     {ID: "tight", Enabled: &enabled, CodexReservePercent: reserve(25)},
		"unknown":   {ID: "unknown", Enabled: &enabled, CodexReservePercent: reserve(25)},
		"unlimited": {ID: "unlimited", Enabled: &enabled},
	}
	filter := CodexKeyPoolFilter(claudeReserveStore{base: bridge.URL, keys: keys})
	pool := []schemas.Key{{ID: "roomy"}, {ID: "tight"}, {ID: "unknown"}, {ID: "unlimited"}}
	got, err := filter(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), schemas.Claude, "claude-sonnet-5", pool)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, key := range got {
		ids = append(ids, key.ID)
	}
	// 60% remaining clears a 25% reserve; 10% does not; unreadable usage fails closed
	// with a reserve and open without one.
	if strings.Join(ids, ",") != "roomy,unlimited" {
		t.Fatalf("eligible accounts %v", ids)
	}
	if reads.Load() != 4 {
		t.Fatalf("usage reads %d, want 4 (every account is checked for exhaustion)", reads.Load())
	}
}

func TestClaudePoolSkipsAccountsWithoutUsageLeft(t *testing.T) {
	const token = "synthetic-bridge-token-for-tests-only"
	t.Setenv("CLAUDE_BRIDGE_TOKEN", token)
	later := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	earlier := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	window := func(used float64, resets string) map[string]any {
		return map[string]any{"utilization": used, "resets_at": resets}
	}
	readings := map[string]map[string]any{
		"fresh":         {"five_hour": window(20, later), "seven_day": window(30, later)},
		"session-spent": {"five_hour": window(100, later), "seven_day": window(30, later)},
		"week-spent":    {"five_hour": window(0, later), "seven_day": window(100, later)},
		"reset-since":   {"five_hour": window(100, earlier), "seven_day": window(30, later)},
	}
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		account := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/accounts/"), "/usage")
		if account == "disconnected" {
			w.WriteHeader(http.StatusConflict)
			return
		}
		reading, ok := readings[account]
		if !ok {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		json.NewEncoder(w).Encode(reading)
	}))
	defer bridge.Close()
	enabled := true
	keys := map[string]*schemas.Key{}
	pool := []schemas.Key{}
	for _, id := range []string{"fresh", "session-spent", "week-spent", "reset-since", "disconnected", "unreadable"} {
		keys[id] = &schemas.Key{ID: id, Enabled: &enabled}
		pool = append(pool, schemas.Key{ID: id})
	}
	filter := CodexKeyPoolFilter(claudeReserveStore{base: bridge.URL, keys: keys})
	got, err := filter(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), schemas.Claude, "claude-sonnet-4-6", pool)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(got))
	for _, key := range got {
		ids = append(ids, key.ID)
	}
	// No reserve is configured: a spent window or a disconnected account still leaves the
	// pool, and an unreadable reading stays in it because a rejection rotates anyway.
	if strings.Join(ids, ",") != "fresh,reset-since,unreadable" {
		t.Fatalf("eligible accounts %v", ids)
	}
}
