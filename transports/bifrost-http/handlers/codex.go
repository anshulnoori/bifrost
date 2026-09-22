package handlers

import (
	"context"
	"errors"
	"time"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/codex"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

type CodexHandler struct{ configStore configstore.ConfigStore }

func NewCodexHandler(store configstore.ConfigStore) *CodexHandler {
	return &CodexHandler{configStore: store}
}

func (h *CodexHandler) RegisterRoutes(r *router.Router, middleware ...schemas.BifrostHTTPMiddleware) {
	middleware = append(middleware, h.managementAuth)
	r.POST("/api/codex/connections", lib.ChainMiddlewares(h.start, middleware...))
	r.GET("/api/codex/connections/current", lib.ChainMiddlewares(h.current, middleware...))
	r.GET("/api/codex/connections/usage", lib.ChainMiddlewares(h.usage, middleware...))
	r.POST("/api/codex/connections/{id}/poll", lib.ChainMiddlewares(h.poll, middleware...))
	r.DELETE("/api/codex/connections/{id}", lib.ChainMiddlewares(h.disconnect, middleware...))
}

// OAuth credentials are provider configuration. Require a genuine dashboard
// login even if a deployment disabled management auth or whitelisted this path.
func (h *CodexHandler) managementAuth(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Cache-Control", "no-store")
		if h.configStore == nil {
			SendError(ctx, 503, "Configure encrypted database storage before connecting Codex")
			return
		}
		config, err := h.configStore.GetAuthConfig(ctx)
		if err != nil || config == nil || !config.IsEnabled {
			SendError(ctx, 401, "Sign in to the dashboard to manage Codex accounts")
			return
		}
		if string(ctx.Request.Header.Peek("Sec-Fetch-Site")) == "cross-site" || len(ctx.Request.Header.Peek("x-bf-codex-key")) == 0 {
			SendError(ctx, 403, "A same-origin Codex account request is required")
			return
		}
		auth := &AuthMiddleware{store: h.configStore}
		auth.authConfig.Store(config)
		// Never accept upstream JWTs, inference keys, or temporary tokens here.
		ctx.SetUserValue(schemas.IsAPIKeyAuthContextKey, false)
		auth.middleware(func(*configstore.AuthConfig, string) bool { return false }, false)(next)(ctx)
	}
}

func (h *CodexHandler) authorize(ctx *fasthttp.RequestCtx) (*codex.Store, string, bool) {
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("Pragma", "no-cache")
	owner, err := lib.CodexOwner(ctx, h.configStore, string(ctx.Request.Header.Peek("x-bf-codex-key")))
	if err != nil {
		SendError(ctx, 404, "Codex account not found")
		return nil, "", false
	}
	store, err := codex.NewStore(h.configStore.DB)
	if err != nil {
		SendError(ctx, 503, err.Error())
		return nil, "", false
	}
	return store, owner, true
}

func codexError(ctx *fasthttp.RequestCtx, err error) {
	switch {
	case errors.Is(err, codex.ErrNotFound):
		SendError(ctx, 404, err.Error())
	case errors.Is(err, codex.ErrBusy):
		SendError(ctx, 409, err.Error())
	case errors.Is(err, codex.ErrReconnect):
		SendError(ctx, 409, err.Error())
	default:
		SendError(ctx, 502, "Codex authorization could not complete; retry or reconnect")
	}
}

func (h *CodexHandler) usage(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	usage, err := store.Usage(requestCtx, owner)
	if err != nil {
		SendError(ctx, 502, "Subscription usage is unavailable")
		return
	}
	SendJSON(ctx, usage)
}

func (h *CodexHandler) start(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	login, err := store.Start(requestCtx, owner)
	if err != nil {
		codexError(ctx, err)
		return
	}
	logger.Info("codex connection started: id=%s owner=%s", login.ID, owner)
	SendJSON(ctx, login)
}

func (h *CodexHandler) current(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	row, err := store.Current(ctx, owner)
	if errors.Is(err, codex.ErrNotFound) {
		SendJSON(ctx, map[string]string{"state": "disconnected"})
		return
	}
	if err != nil {
		codexError(ctx, err)
		return
	}
	SendJSON(ctx, row)
}

func (h *CodexHandler) poll(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	id, _ := ctx.UserValue("id").(string)
	requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	row, err := store.Poll(requestCtx, owner, id)
	if err != nil && !errors.Is(err, codex.ErrPending) {
		codexError(ctx, err)
		return
	}
	SendJSON(ctx, row)
}

func (h *CodexHandler) disconnect(ctx *fasthttp.RequestCtx) {
	store, owner, ok := h.authorize(ctx)
	if !ok {
		return
	}
	id, _ := ctx.UserValue("id").(string)
	if err := store.Disconnect(ctx, owner, id); err != nil {
		codexError(ctx, err)
		return
	}
	logger.Info("codex connection disconnected: id=%s owner=%s", id, owner)
	SendJSON(ctx, map[string]string{"state": "disconnected"})
}
