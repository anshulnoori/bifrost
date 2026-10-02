package lib

import (
	"errors"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
)

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
