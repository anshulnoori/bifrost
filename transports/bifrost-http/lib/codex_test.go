package lib

import (
	"context"
	"testing"
	"time"

	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

type codexOwnerStore struct {
	configstore.ConfigStore
	key *tables.TableVirtualKey
}

func (s codexOwnerStore) GetVirtualKeyByValue(_ context.Context, value string) (*tables.TableVirtualKey, error) {
	if value != "gateway-secret" {
		return nil, nil
	}
	return s.key, nil
}

func TestCodexOwnerRequiresActiveCodexVirtualKey(t *testing.T) {
	active := true
	inactive := false
	past := time.Now().Add(-time.Minute)
	for _, tc := range []struct {
		name  string
		key   *tables.TableVirtualKey
		value string
		want  string
	}{
		{"missing", nil, "", ""},
		{"upstream JWT", nil, "eyJ.upstream.jwt", ""},
		{"inactive", &tables.TableVirtualKey{ID: "one", IsActive: &inactive, AllowAllProviders: true}, "gateway-secret", ""},
		{"expired", &tables.TableVirtualKey{ID: "one", IsActive: &active, AllowAllProviders: true, ExpiresAt: &past}, "gateway-secret", ""},
		{"deny by default", &tables.TableVirtualKey{ID: "one", IsActive: &active}, "gateway-secret", ""},
		{"other provider", &tables.TableVirtualKey{ID: "one", IsActive: &active, ProviderConfigs: []tables.TableVirtualKeyProviderConfig{{Provider: "openai"}}}, "gateway-secret", ""},
		{"codex", &tables.TableVirtualKey{ID: "one", IsActive: &active, ProviderConfigs: []tables.TableVirtualKeyProviderConfig{{Provider: "codex"}}}, "gateway-secret", "vk:one"},
		{"different owner", &tables.TableVirtualKey{ID: "two", IsActive: &active, AllowAllProviders: true}, "gateway-secret", "vk:two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, err := CodexOwner(context.Background(), codexOwnerStore{key: tc.key}, tc.value)
			if owner != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("owner=%q error=%v", owner, err)
			}
		})
	}
}
