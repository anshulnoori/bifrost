package lib

import (
	"context"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

type codexOwnerStore struct{ configstore.ConfigStore }

func (codexOwnerStore) GetProviderKey(_ context.Context, provider schemas.ModelProvider, id string) (*schemas.Key, error) {
	if provider != schemas.Codex || id != "account-a" && id != "account-b" {
		return nil, nil
	}
	return &schemas.Key{ID: id}, nil
}

func TestCodexOwnerUsesConfiguredProviderAccount(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"", ""}, {"eyJ.upstream.jwt", ""}, {"missing", ""},
		{"account-a", "provider:codex:account-a"}, {"account-b", "provider:codex:account-b"},
	} {
		owner, err := CodexOwner(context.Background(), codexOwnerStore{}, tc.id)
		if owner != tc.want || (err == nil) != (tc.want != "") {
			t.Fatalf("id=%s owner=%s err=%v", tc.id, owner, err)
		}
	}
}
