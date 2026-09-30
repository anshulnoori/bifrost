package lib

import (
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/chatgpt"
	"github.com/maximhq/bifrost/framework/grant"
	"github.com/stretchr/testify/require"
)

func TestChatGPTCacheScopeUsesOwnConnectionAndFreshAdmission(t *testing.T) {
	store, db := newCodexScopeStore(t)
	seedCodexScope(t, db)
	require.NoError(t, db.AutoMigrate(chatgpt.MigrationModels()...))
	require.NoError(t, db.Exec(`UPDATE config_providers SET name = 'chatgpt'`).Error)
	require.NoError(t, db.Exec(`UPDATE governance_virtual_key_provider_configs SET provider = 'chatgpt'`).Error)
	ctx := codexScopeContext("vk-1", "account-b")
	g := grant.New()
	permit := grant.NewPermit(grant.PermitVirtualKey, "vk-1", "test key", true, false, []schemas.ProviderPermit{{
		Provider: "chatgpt", AllowedModels: schemas.WhiteList{"*"}, KeyIDs: []string{"account-b"},
	}}, nil)
	g.SetAccess(grant.NewAccess([]schemas.Permit{permit}, nil, "", nil))
	g.SetIdentity(ctx.Grant().Identity())
	ctx.SetGrant(g)
	resolve := CodexCacheScopeResolver(store)
	// A Codex connection with the same account key cannot authorize ChatGPT.
	_, _, err := resolve(ctx, schemas.ChatGPT, "gpt-6.1-sol")
	require.Error(t, err)
	require.NoError(t, db.Create(&chatgpt.Connection{ID: "plan-connection", Owner: "provider:chatgpt:account-b", State: "connected", Secret: "not-ciphertext", ExpiresAt: time.Now().Add(-time.Hour)}).Error)
	// Metadata checks must not decrypt or refresh an expired access token.
	first, keys, err := resolve(ctx, schemas.ChatGPT, "gpt-6.1-sol")
	require.NoError(t, err)
	require.Equal(t, []string{"account-b"}, keys)
	require.NotEmpty(t, first)
	require.NoError(t, db.Model(&chatgpt.Connection{}).Where("id = ?", "plan-connection").Update("id", "new-plan-connection").Error)
	second, _, err := resolve(ctx, schemas.ChatGPT, "gpt-6.1-sol")
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	require.NoError(t, db.Exec(`UPDATE governance_virtual_keys SET is_active = 0`).Error)
	_, _, err = resolve(ctx, schemas.ChatGPT, "gpt-6.1-sol")
	require.Error(t, err)
}
