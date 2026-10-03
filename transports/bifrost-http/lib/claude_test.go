package lib

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
	// 60% remaining clears a 25% reserve; 10% does not; unreadable usage fails closed;
	// an account without a reserve is never checked.
	if strings.Join(ids, ",") != "roomy,unlimited" {
		t.Fatalf("eligible accounts %v", ids)
	}
	if reads.Load() != 3 {
		t.Fatalf("usage reads %d, want 3 (accounts without a reserve are not read)", reads.Load())
	}
}
