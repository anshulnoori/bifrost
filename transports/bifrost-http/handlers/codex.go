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
	r.POST("/api/codex/connections", lib.ChainMiddlewares(h.start, middleware...))
	r.GET("/api/codex/connections/current", lib.ChainMiddlewares(h.current, middleware...))
	r.POST("/api/codex/connections/{id}/poll", lib.ChainMiddlewares(h.poll, middleware...))
	r.DELETE("/api/codex/connections/{id}", lib.ChainMiddlewares(h.disconnect, middleware...))
}

func (h *CodexHandler) authorize(ctx *fasthttp.RequestCtx) (*codex.Store, string, bool) {
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("Pragma", "no-cache")
	// An upstream JWT in Authorization is never an onboarding identity. Requiring
	// this custom header also prevents ambient dashboard cookies from granting access.
	owner, err := lib.CodexOwner(ctx, h.configStore, string(ctx.Request.Header.Peek("x-bf-vk")))
	if err != nil {
		SendError(ctx, 401, "An active Bifrost virtual key allowing Codex is required in x-bf-vk")
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
