package handlers

import (
	"testing"

	"github.com/fasthttp/router"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestChatGPTManagementRequiresDashboardAuthentication(t *testing.T) {
	r := router.New()
	NewChatGPTHandler(deniedCodexStore{}).RegisterRoutes(r)
	for _, route := range []struct{ method, path string }{{"GET", "/api/chatgpt/connections/current"}, {"POST", "/api/chatgpt/connections"}, {"POST", "/api/chatgpt/connections/victim/complete"}, {"DELETE", "/api/chatgpt/connections/victim"}} {
		var ctx fasthttp.RequestCtx
		ctx.Request.SetRequestURI(route.path)
		ctx.Request.Header.SetMethod(route.method)
		ctx.Request.Header.Set("Authorization", "Bearer upstream-private-token")
		ctx.Request.Header.Set("x-bf-chatgpt-key", "victim")
		r.Handler(&ctx)
		require.Equal(t, 401, ctx.Response.StatusCode())
		require.Equal(t, "no-store", string(ctx.Response.Header.Peek("Cache-Control")))
		require.NotContains(t, string(ctx.Response.Body()), "upstream-private-token")
	}
}

func TestChatGPTManagementAcceptsDashboardSessionAndRejectsCrossSite(t *testing.T) {
	f := newDashboardIdPFixture(t)
	state, browser := f.start(t)
	callback := dashboardCallback(state, browser)
	f.h.oidcCallback(callback)
	cookie := &fasthttp.Cookie{}
	cookie.SetKey("token")
	require.True(t, callback.Response.Header.Cookie(cookie))
	h := NewChatGPTHandler(f.h.configStore)
	for _, site := range []string{"same-origin", "cross-site"} {
		ctx := getCtx("/api/chatgpt/connections")
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.Header.SetCookie("token", string(cookie.Value()))
		ctx.Request.Header.Set("Sec-Fetch-Site", site)
		h.managementAuth(func(ctx *fasthttp.RequestCtx) { ctx.SetStatusCode(204) })(ctx)
		if site == "same-origin" {
			require.Equal(t, 204, ctx.Response.StatusCode())
		} else {
			require.Equal(t, 403, ctx.Response.StatusCode())
		}
	}
}
