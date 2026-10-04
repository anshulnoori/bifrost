package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func modelIDs(resp *schemas.BifrostListModelsResponse) string {
	var ids []string
	for _, model := range resp.Data {
		ids = append(ids, model.ID)
	}
	return strings.Join(ids, ",")
}

// The subscription bridge has no model-listing endpoint, so the catalog is the
// fixed set of models verified on the subscription. Listing must never reach the
// bridge, and must honour each account key's allow and block lists.
func TestClaudeListModelsServesCatalogWithoutBridge(t *testing.T) {
	t.Setenv("CLAUDE_BRIDGE_TOKEN", "synthetic-bridge-token-for-tests-only")
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	p, err := New(&schemas.ProviderConfig{NetworkConfig: schemas.NetworkConfig{BaseURL: server.URL}}, testLogger{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)

	resp, failure := p.ListModels(ctx, []schemas.Key{{ID: "account-a", Models: []string{"*"}}}, &schemas.BifrostListModelsRequest{Provider: schemas.Claude})
	if failure != nil {
		t.Fatalf("list models failed: %+v", failure.Error)
	}
	// The shared list-models handler sorts by ID.
	want := "claude/claude-fable-5-1,claude/claude-haiku-4-5,claude/claude-opus-5,claude/claude-opus-5-5,claude/claude-sonnet-5,claude/claude-sonnet-5-5"
	if got := modelIDs(resp); got != want {
		t.Fatalf("catalog = %s", got)
	}
	for _, model := range resp.Data {
		if model.OwnedBy == nil || *model.OwnedBy != "anthropic" || model.Name == nil || *model.Name == "" || model.ContextLength == nil {
			t.Fatalf("model %s missing metadata: %+v", model.ID, model)
		}
		if model.Pricing != nil {
			t.Fatalf("model %s carries hardcoded pricing; pricing comes from the datasheet", model.ID)
		}
	}

	resp, failure = p.ListModels(ctx, []schemas.Key{{ID: "account-a", Models: []string{"claude-haiku-4-5", "claude-sonnet-5-5"}}}, &schemas.BifrostListModelsRequest{Provider: schemas.Claude})
	if failure != nil {
		t.Fatal(failure.Error)
	}
	if got := modelIDs(resp); got != "claude/claude-haiku-4-5,claude/claude-sonnet-5-5" {
		t.Fatalf("allow-listed catalog = %s", got)
	}
	resp, failure = p.ListModels(ctx, []schemas.Key{{ID: "account-a", Models: []string{"*"}, BlacklistedModels: []string{"claude-fable-5-1"}}}, &schemas.BifrostListModelsRequest{Provider: schemas.Claude})
	if failure != nil {
		t.Fatal(failure.Error)
	}
	if strings.Contains(modelIDs(resp), "fable") {
		t.Fatalf("blocked model listed: %s", modelIDs(resp))
	}
	if called {
		t.Fatal("listing models must not call the bridge")
	}
}
