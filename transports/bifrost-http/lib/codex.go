package lib

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/codex"
	"github.com/maximhq/bifrost/framework/configstore"
)

type codexKeyMetadata struct {
	ID, Name, ModelsJSON, BlacklistedModelsJSON string
	Enabled                                     *bool
}

// CodexCacheScopeResolver returns only authorization metadata. It never reads
// credentials, subscription usage, or reserve policy, and never refreshes OAuth.
func CodexCacheScopeResolver(store configstore.ConfigStore) func(*schemas.BifrostContext, schemas.ModelProvider, string) (string, []string, error) {
	return func(ctx *schemas.BifrostContext, provider schemas.ModelProvider, model string) (string, []string, error) {
		if store == nil || ctx == nil || provider != schemas.Codex || ctx.Grant() == nil || ctx.Grant().Access() == nil ||
			ctx.Grant().Identity() == nil || ctx.Grant().Identity().VirtualKey() == nil || ctx.Grant().Identity().VirtualKey().ID == "" ||
			!ctx.Grant().Access().IsModelAllowed(string(provider), model) {
			return "", nil, errors.New("Codex cache authorization unavailable")
		}
		allowedIDs, restricted := ctx.Grant().Access().KeysForModel(string(provider), model)
		admittedRestricted := restricted
		allowed := make(map[string]bool, len(allowedIDs))
		for _, id := range allowedIDs {
			allowed[id] = true
		}

		vkID := ""
		if identity := ctx.Grant().Identity(); identity != nil && identity.VirtualKey() != nil {
			vkID = identity.VirtualKey().ID
			var vk struct {
				ID        string
				IsActive  *bool
				ExpiresAt *time.Time
			}
			if err := store.DB().WithContext(ctx).Table("governance_virtual_keys").Select("id", "is_active", "expires_at").Where("id = ?", vkID).First(&vk).Error; err != nil || vk.IsActive != nil && !*vk.IsActive || vk.ExpiresAt != nil && !vk.ExpiresAt.After(time.Now()) {
				return "", nil, errors.New("virtual key inactive")
			}
			var configs []struct {
				ID                               uint
				AllowedModels, BlacklistedModels string
				AllowAllKeys                     bool
			}
			if err := store.DB().WithContext(ctx).Table("governance_virtual_key_provider_configs").Select("id", "allowed_models", "blacklisted_models", "allow_all_keys").Where("virtual_key_id = ? AND provider = ?", vkID, string(provider)).Scan(&configs).Error; err != nil {
				return "", nil, err
			}
			freshAllowed := map[string]bool{}
			allowAll := false
			for _, cfg := range configs {
				var wl schemas.WhiteList
				var bl schemas.BlackList
				if json.Unmarshal([]byte(cfg.AllowedModels), &wl) != nil || json.Unmarshal([]byte(cfg.BlacklistedModels), &bl) != nil || !wl.IsAllowed(model) || bl.IsBlocked(model) {
					continue
				}
				if cfg.AllowAllKeys {
					allowAll = true
					continue
				}
				var ids []string
				if err := store.DB().WithContext(ctx).Table("governance_virtual_key_provider_config_keys AS j").Select("k.key_id").Joins("JOIN config_keys AS k ON k.id = j.table_key_id").Where("j.table_virtual_key_provider_config_id = ?", cfg.ID).Scan(&ids).Error; err != nil {
					return "", nil, err
				}
				for _, id := range ids {
					freshAllowed[id] = true
				}
			}
			if !allowAll {
				restricted = true
				for id := range allowed {
					if !freshAllowed[id] {
						delete(allowed, id)
					}
				}
				if !admittedRestricted {
					allowed = freshAllowed
				}
			}
		}

		var keys []codexKeyMetadata
		if err := store.DB().WithContext(ctx).Table("config_keys AS k").Select("k.key_id AS id", "k.name", "k.models_json", "k.blacklisted_models_json", "k.enabled").Joins("JOIN config_providers AS p ON p.id = k.provider_id").Where("p.name = ?", string(provider)).Scan(&keys).Error; err != nil {
			return "", nil, err
		}
		pinID, _ := ctx.Value(schemas.BifrostContextKeyAPIKeyID).(string)
		if pinID == "" {
			pinID, _ = ctx.Value(schemas.BifrostContextKeyRoutingPinnedAPIKeyID).(string)
		}
		pinName, _ := ctx.Value(schemas.BifrostContextKeyAPIKeyName).(string)
		if pinID != "" {
			pinName = ""
		}
		codexStore, err := codex.NewStore(store.DB)
		if err != nil {
			return "", nil, err
		}
		pool := make([]string, 0, len(keys))
		ids := make([]string, 0, len(keys))
		for _, key := range keys {
			var wl schemas.WhiteList
			var bl schemas.BlackList
			if key.Enabled != nil && !*key.Enabled || json.Unmarshal([]byte(key.ModelsJSON), &wl) != nil || json.Unmarshal([]byte(key.BlacklistedModelsJSON), &bl) != nil || !wl.IsAllowed(model) || bl.IsBlocked(model) || restricted && !allowed[key.ID] || pinID != "" && key.ID != pinID || pinName != "" && key.Name != pinName {
				continue
			}
			connection, err := codexStore.CurrentMetadata(ctx, "provider:codex:"+key.ID)
			if err != nil || connection.State != "connected" && connection.State != "refreshing" {
				continue
			}
			ids = append(ids, key.ID)
			pool = append(pool, key.ID+"\x00"+connection.ID)
		}
		if len(pool) == 0 {
			return "", nil, errors.New("no authorized Codex account")
		}
		sort.Strings(pool)
		sort.Strings(ids)
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s", provider, model, vkID, strings.Join(pool, "\x00"))))
		return fmt.Sprintf("%x", sum), ids, nil
	}
}

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
