package handlers

import (
	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/claude"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

type ClaudeHandler struct{ configStore configstore.ConfigStore }

func NewClaudeHandler(store configstore.ConfigStore) *ClaudeHandler {
	return &ClaudeHandler{configStore: store}
}

func (h *ClaudeHandler) RegisterRoutes(r *router.Router, middleware ...schemas.BifrostHTTPMiddleware) {
	middleware = append(middleware, h.managementAuth)
	r.GET("/api/claude/connections/current", lib.ChainMiddlewares(h.current, middleware...))
	r.GET("/api/claude/connections/usage", lib.ChainMiddlewares(h.usage, middleware...))
	r.POST("/api/claude/connections", lib.ChainMiddlewares(h.start, middleware...))
	r.POST("/api/claude/connections/code", lib.ChainMiddlewares(h.code, middleware...))
	r.DELETE("/api/claude/connections/current", lib.ChainMiddlewares(h.disconnect, middleware...))
}

func (h *ClaudeHandler) managementAuth(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		ctx.Response.Header.Set("Cache-Control", "no-store")
		ctx.Response.Header.Set("Pragma", "no-cache")
		if h.configStore == nil {
			SendError(ctx, 503, "Configure database storage before connecting Claude")
			return
		}
		config, err := h.configStore.GetAuthConfig(ctx)
		if err != nil || config == nil || !config.IsEnabled {
			SendError(ctx, 401, "Sign in to the dashboard to manage Claude accounts")
			return
		}
		if string(ctx.Request.Header.Peek("Sec-Fetch-Site")) == "cross-site" || len(ctx.Request.Header.Peek("x-bf-claude-key")) == 0 {
			SendError(ctx, 403, "A same-origin Claude account request is required")
			return
		}
		auth := &AuthMiddleware{store: h.configStore}
		auth.authConfig.Store(config)
		ctx.SetUserValue(schemas.IsAPIKeyAuthContextKey, false)
		auth.middleware(func(*configstore.AuthConfig, string) bool { return false }, false)(next)(ctx)
	}
}

func (h *ClaudeHandler) client(ctx *fasthttp.RequestCtx) (*claude.Client, string, bool) {
	id := string(ctx.Request.Header.Peek("x-bf-claude-key"))
	key, err := h.configStore.GetProviderKey(ctx, schemas.Claude, id)
	if err != nil || key == nil || key.ID != id {
		SendError(ctx, 404, "Claude account not found")
		return nil, "", false
	}
	config, err := h.configStore.GetProvider(ctx, schemas.Claude)
	if err != nil || config == nil {
		SendError(ctx, 404, "Claude provider not found")
		return nil, "", false
	}
	base := ""
	if config.NetworkConfig != nil {
		base = config.NetworkConfig.BaseURL
	}
	client, err := claude.NewClient(base)
	if err != nil {
		SendError(ctx, 503, "Claude account service is not configured")
		return nil, "", false
	}
	return client, id, true
}

func (h *ClaudeHandler) action(ctx *fasthttp.RequestCtx, method, action string) {
	client, id, ok := h.client(ctx)
	if !ok {
		return
	}
	if len(ctx.PostBody()) > 8192 {
		SendError(ctx, 413, "Claude account request too large")
		return
	}
	result, status, err := client.Do(ctx, id, method, action, ctx.PostBody())
	if err != nil {
		SendError(ctx, status, err.Error())
		return
	}
	SendJSON(ctx, result)
}

func (h *ClaudeHandler) usage(ctx *fasthttp.RequestCtx) {
	client, id, ok := h.client(ctx)
	if !ok {
		return
	}
	result, status, err := client.Usage(ctx, id)
	if err != nil {
		SendError(ctx, status, err.Error())
		return
	}
	SendJSON(ctx, result)
}

func (h *ClaudeHandler) current(ctx *fasthttp.RequestCtx)    { h.action(ctx, "GET", "") }
func (h *ClaudeHandler) start(ctx *fasthttp.RequestCtx)      { h.action(ctx, "POST", "/start") }
func (h *ClaudeHandler) code(ctx *fasthttp.RequestCtx)       { h.action(ctx, "POST", "/code") }
func (h *ClaudeHandler) disconnect(ctx *fasthttp.RequestCtx) { h.action(ctx, "DELETE", "") }

func disconnectClaudeAccount(ctx *fasthttp.RequestCtx, network *schemas.NetworkConfig, id string) error {
	base := ""
	if network != nil {
		base = network.BaseURL
	}
	client, err := claude.NewClient(base)
	if err != nil {
		return err
	}
	_, _, err = client.Do(ctx, id, "DELETE", "", nil)
	return err
}
