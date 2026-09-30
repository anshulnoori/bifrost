package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/chatgpt"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

type ChatGPTHandler struct{ configStore configstore.ConfigStore }

func NewChatGPTHandler(store configstore.ConfigStore) *ChatGPTHandler {
	return &ChatGPTHandler{configStore: store}
}

func (h *ChatGPTHandler) RegisterRoutes(r *router.Router, middleware ...schemas.BifrostHTTPMiddleware) {
	middleware = append(middleware, h.managementAuth)
	r.POST("/api/chatgpt/connections", lib.ChainMiddlewares(h.start, middleware...))
	r.GET("/api/chatgpt/connections/current", lib.ChainMiddlewares(h.current, middleware...))
	r.POST("/api/chatgpt/connections/{id}/complete", lib.ChainMiddlewares(h.complete, middleware...))
	r.DELETE("/api/chatgpt/connections/{id}", lib.ChainMiddlewares(h.disconnect, middleware...))
}

func (h *ChatGPTHandler) managementAuth(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Cache-Control", "no-store")
		ctx.Response.Header.Set("Pragma", "no-cache")
		if h.configStore == nil {
			SendError(ctx, 503, "database storage required")
			return
		}
		config, err := h.configStore.GetAuthConfig(ctx)
		if err != nil || config == nil || !config.IsEnabled {
			SendError(ctx, 401, "dashboard authentication required")
			return
		}
		if string(ctx.Request.Header.Peek("Sec-Fetch-Site")) == "cross-site" {
			SendError(ctx, 403, "same-origin dashboard authentication required")
			return
		}
		auth := &AuthMiddleware{store: h.configStore}
		auth.authConfig.Store(config)
		ctx.SetUserValue(schemas.IsAPIKeyAuthContextKey, false)
		auth.middleware(func(*configstore.AuthConfig, string) bool { return false }, false)(next)(ctx)
	}
}

func (h *ChatGPTHandler) authorize(ctx *fasthttp.RequestCtx) (*chatgpt.Store, string, bool) {
	owner, err := lib.ChatGPTOwner(ctx, h.configStore, string(ctx.Request.Header.Peek("x-bf-chatgpt-key")))
	if err != nil {
		SendError(ctx, 404, "configured ChatGPT account required")
		return nil, "", false
	}
	store, err := chatgpt.NewStore(h.configStore.DB)
	if err != nil {
		SendError(ctx, 503, "encrypted database storage required")
		return nil, "", false
	}
	return store, owner, true
}

func chatgptError(ctx *fasthttp.RequestCtx, err error) {
	switch {
	case errors.Is(err, chatgpt.ErrNotFound):
		SendError(ctx, 404, "ChatGPT connection not found")
	case errors.Is(err, chatgpt.ErrInvalidCallback):
		SendError(ctx, 400, "invalid ChatGPT authorization callback")
	case errors.Is(err, chatgpt.ErrBusy):
		SendError(ctx, 409, "ChatGPT connection operation in progress")
	case errors.Is(err, chatgpt.ErrReconnect):
		SendError(ctx, 409, "ChatGPT connection requires reconnect")
	default:
		SendError(ctx, 502, "ChatGPT authorization unavailable")
	}
}

func (h *ChatGPTHandler) start(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	login, err := store.Start(requestCtx, owner)
	if err != nil {
		chatgptError(ctx, err)
		return
	}
	SendJSON(ctx, login)
}

func (h *ChatGPTHandler) current(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	row, err := store.Current(ctx, owner)
	if errors.Is(err, chatgpt.ErrNotFound) {
		SendJSON(ctx, map[string]string{"state": "disconnected"})
		return
	}
	if err != nil {
		chatgptError(ctx, err)
		return
	}
	SendJSON(ctx, row)
}

func (h *ChatGPTHandler) complete(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	var body struct {
		CallbackURL string `json:"callback_url"`
	}
	if len(ctx.PostBody()) > 16<<10 || json.Unmarshal(ctx.PostBody(), &body) != nil || body.CallbackURL == "" {
		SendError(ctx, 400, "callback_url required")
		return
	}
	id, _ := ctx.UserValue("id").(string)
	requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	row, err := store.Complete(requestCtx, owner, id, body.CallbackURL)
	if err != nil {
		chatgptError(ctx, err)
		return
	}
	SendJSON(ctx, row)
}

func (h *ChatGPTHandler) disconnect(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	id, _ := ctx.UserValue("id").(string)
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := store.Disconnect(requestCtx, owner, id); err != nil {
		chatgptError(ctx, err)
		return
	}
	SendJSON(ctx, map[string]string{"state": "disconnected"})
}
