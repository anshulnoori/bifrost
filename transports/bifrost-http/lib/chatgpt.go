package lib

import (
	"context"
	"errors"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/chatgpt"
	"github.com/maximhq/bifrost/framework/configstore"
)

func ChatGPTOwner(ctx context.Context, store configstore.ConfigStore, id string) (string, error) {
	if store == nil || id == "" {
		return "", errors.New("a configured ChatGPT account is required")
	}
	key, err := store.GetProviderKey(ctx, schemas.ChatGPT, id)
	if err != nil || key == nil || key.ID != id {
		return "", errors.New("ChatGPT account not found")
	}
	return "provider:chatgpt:" + id, nil
}

func chatgptCredential(store configstore.ConfigStore) func(*schemas.BifrostContext, schemas.Key) (string, error) {
	return func(ctx *schemas.BifrostContext, selected schemas.Key) (string, error) {
		if store == nil || selected.ID == "" || ctx.Grant() == nil {
			return "", errors.New("ChatGPT account unavailable")
		}
		if identity := ctx.Grant().Identity(); identity != nil && identity.VirtualKey() != nil {
			vk, err := store.GetVirtualKey(ctx, identity.VirtualKey().ID)
			if err != nil || vk == nil || !vk.IsActiveValue() || vk.IsExpiredAt(time.Now()) {
				return "", errors.New("virtual key no longer active")
			}
		}
		key, err := store.GetProviderKey(ctx, schemas.ChatGPT, selected.ID)
		if err != nil || key == nil || key.ID != selected.ID || key.Enabled != nil && !*key.Enabled {
			return "", errors.New("ChatGPT account no longer active")
		}
		s, err := chatgpt.NewStore(store.DB)
		if err != nil {
			return "", err
		}
		row, err := s.Current(ctx, "provider:chatgpt:"+key.ID)
		if err != nil {
			return "", err
		}
		return s.Credential(ctx, "provider:chatgpt:"+key.ID, row.ID)
	}
}
