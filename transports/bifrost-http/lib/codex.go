package lib

import (
	"context"
	"errors"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/codex"
	"github.com/maximhq/bifrost/framework/configstore"
)

// CodexOwner identifies a configured provider account, never a caller-supplied
// upstream credential. Management authentication is enforced by the handler.
func CodexOwner(ctx context.Context, store configstore.ConfigStore, id string) (string, error) {
	if store == nil || id == "" {
		return "", errors.New("a configured Codex account is required")
	}
	key, err := store.GetProviderKey(ctx, schemas.Codex, id)
	if err != nil || key == nil || key.ID != id {
		return "", errors.New("Codex account not found")
	}
	return "provider:codex:" + key.ID, nil
}

func codexCredential(store configstore.ConfigStore) func(*schemas.BifrostContext, schemas.Key) (string, string, error) {
	return func(ctx *schemas.BifrostContext, selected schemas.Key) (string, string, error) {
		if store == nil || ctx == nil || ctx.Grant() == nil {
			return "", "", errors.New("codex requires gateway admission")
		}
		// Recheck mutable records, rather than trusting a cached routing snapshot.
		if identity := ctx.Grant().Identity(); identity != nil && identity.VirtualKey() != nil {
			vk, err := store.GetVirtualKey(ctx, identity.VirtualKey().ID)
			if err != nil || vk == nil || !vk.IsActiveValue() || vk.IsExpiredAt(time.Now()) {
				return "", "", errors.New("gateway virtual key is no longer active")
			}
		}
		key, err := store.GetProviderKey(ctx, schemas.Codex, selected.ID)
		if err != nil || key == nil || key.ID == "" || key.Enabled != nil && !*key.Enabled {
			return "", "", errors.New("Codex account is missing or disabled")
		}
		owner := "provider:codex:" + key.ID
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
