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
		// Also enforce explicit/sticky account selection, which bypasses the pool
		// filter. Re-read policy above so stale replica config cannot bypass it.
		if key.CodexReservePercent != nil {
			usage, err := s.Usage(ctx, owner)
			if err != nil || !usage.AboveReserve(*key.CodexReservePercent) {
				return "", "", schemas.ErrCodexReserve
			}
		}
		return s.Credential(ctx, owner, row.ID)
	}
}

// Filter only the already-admitted pool. Never fail open: the core's generic
// filter error path intentionally uses unfiltered keys, so failed checks omit
// that account and return a nil error. Credential resolution rechecks policy.
func CodexKeyPoolFilter(store configstore.ConfigStore) schemas.KeyPoolFilter {
	return func(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string, keys []schemas.Key) ([]schemas.Key, error) {
		if provider != schemas.Codex {
			return keys, nil
		}
		eligible := make([]schemas.Key, 0, len(keys))
		if store == nil {
			return eligible, nil
		}
		s, err := codex.NewStore(store.DB)
		if err != nil {
			return eligible, nil
		}
		for _, candidate := range keys {
			key, err := store.GetProviderKey(ctx, provider, candidate.ID)
			if err != nil || key == nil || key.Enabled != nil && !*key.Enabled {
				continue
			}
			if key.CodexReservePercent != nil {
				usage, err := s.Usage(ctx, "provider:codex:"+key.ID)
				if err != nil || !usage.AboveReserve(*key.CodexReservePercent) {
					continue
				}
			}
			eligible = append(eligible, candidate)
		}
		return eligible, nil
	}
}
