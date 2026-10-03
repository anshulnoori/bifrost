package lib

import (
	"context"
	"errors"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/claude"
	"github.com/maximhq/bifrost/framework/configstore"
)

// claudeAboveReserve reads usage through the bridge, which shares one cached
// reading per account (refreshed at most once a minute, with 429 backoff), so
// routing checks do not add upstream usage calls. Any failure fails closed.
func claudeAboveReserve(ctx context.Context, store configstore.ConfigStore, key *schemas.Key) bool {
	if key.CodexReservePercent == nil {
		return true
	}
	config, err := store.GetProvider(ctx, schemas.Claude)
	if err != nil || config == nil {
		return false
	}
	base := ""
	if config.NetworkConfig != nil {
		base = config.NetworkConfig.BaseURL
	}
	client, err := claude.NewClient(base)
	if err != nil {
		return false
	}
	usage, _, err := client.Usage(ctx, key.ID)
	return err == nil && usage.AboveReserve(*key.CodexReservePercent)
}

func claudeAccount(store configstore.ConfigStore) func(*schemas.BifrostContext, schemas.Key, string) error {
	return func(ctx *schemas.BifrostContext, selected schemas.Key, model string) error {
		denied := errors.New("Claude account is no longer authorized")
		if store == nil || ctx == nil || ctx.Grant() == nil || ctx.Grant().Identity() == nil || ctx.Grant().Identity().VirtualKey() == nil {
			return denied
		}
		key, err := store.GetProviderKey(ctx, schemas.Claude, selected.ID)
		if err != nil || key == nil || key.ID != selected.ID || key.Enabled != nil && !*key.Enabled || !key.Models.IsAllowed(model) || key.BlacklistedModels.IsBlocked(model) {
			return denied
		}
		// Also enforce explicit/sticky account selection, which bypasses the pool filter.
		if !claudeAboveReserve(ctx, store, key) {
			return schemas.ErrCodexReserve
		}
		vk, err := store.GetVirtualKey(ctx, ctx.Grant().Identity().VirtualKey().ID)
		if err != nil || vk == nil || !vk.IsActiveValue() || vk.IsExpiredAt(time.Now()) {
			return denied
		}
		for _, config := range vk.ProviderConfigs {
			if config.Provider != string(schemas.Claude) || !config.AllowedModels.IsAllowed(model) || config.BlacklistedModels.IsBlocked(model) {
				continue
			}
			if config.AllowAllKeys {
				return nil
			}
			for _, account := range config.Keys {
				if account.KeyID == selected.ID {
					return nil
				}
			}
		}
		return denied
	}
}
