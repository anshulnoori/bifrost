package lib

import (
	"context"
	"errors"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/codex"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
)

func codexKeyOwner(vk *tables.TableVirtualKey) (string, error) {
	if vk == nil || !vk.IsActiveValue() || vk.IsExpiredAt(time.Now()) {
		return "", errors.New("an active gateway virtual key is required")
	}
	allowed := vk.AllowAllProviders
	for _, provider := range vk.ProviderConfigs {
		if provider.Provider == string(schemas.Codex) {
			allowed = true
		}
	}
	if !allowed {
		return "", errors.New("virtual key does not allow codex")
	}
	// Deliberately key-scoped in OSS: knowing another connection ID or upstream
	// account ID grants nothing. A dashboard admin session is not this identity.
	return "vk:" + vk.ID, nil
}

// CodexOwner authenticates onboarding independently of dashboard authentication.
func CodexOwner(ctx context.Context, store configstore.ConfigStore, value string) (string, error) {
	if store == nil || value == "" {
		return "", errors.New("a gateway virtual key is required")
	}
	vk, err := store.GetVirtualKeyByValue(ctx, value)
	if err != nil {
		return "", errors.New("invalid gateway virtual key")
	}
	return codexKeyOwner(vk)
}

func codexCredential(store configstore.ConfigStore) func(*schemas.BifrostContext) (string, string, error) {
	return func(ctx *schemas.BifrostContext) (string, string, error) {
		if store == nil || ctx.Grant() == nil || ctx.Grant().Access() == nil || ctx.Grant().Identity() == nil || ctx.Grant().Identity().VirtualKey() == nil {
			return "", "", errors.New("codex requires an admitted gateway virtual key")
		}
		vk, err := store.GetVirtualKey(ctx, ctx.Grant().Identity().VirtualKey().ID)
		if err != nil {
			return "", "", errors.New("gateway virtual key no longer exists")
		}
		owner, err := codexKeyOwner(vk)
		if err != nil {
			return "", "", err
		}
		s, err := codex.NewStore(store.DB)
		if err != nil {
			return "", "", err
		}
		row, err := s.Current(ctx, owner)
		if err != nil {
			return "", "", err
		}
		return s.Credential(ctx, owner, row.ID)
	}
}
