package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type deniedCodexStore struct{ configstore.ConfigStore }

func (deniedCodexStore) GetAuthConfig(context.Context) (*configstore.AuthConfig, error) {
	return nil, nil
}

func (deniedCodexStore) GetVirtualKeyByValue(context.Context, string) (*tables.TableVirtualKey, error) {
	return nil, nil
}

func TestCodexManagementAcceptsDashboardOIDCSession(t *testing.T) {
	f := newDashboardIdPFixture(t)
	state, browser := f.start(t)
	callback := dashboardCallback(state, browser)
	f.h.oidcCallback(callback)
	require.Equal(t, "/workspace", string(callback.Response.Header.Peek("Location")))
	cookie := &fasthttp.Cookie{}
	cookie.SetKey("token")
	require.True(t, callback.Response.Header.Cookie(cookie))
	auth, err := InitAuthMiddleware(f.h.configStore, nil, nil, "")
	require.NoError(t, err)
	h := NewCodexHandler(f.h.configStore)
	for _, method := range []string{"GET", "POST"} {
		t.Run(method, func(t *testing.T) {
			ctx := getCtx("/api/codex/connections")
			ctx.Request.Header.SetMethod(method)
			ctx.Request.Header.SetCookie("token", string(cookie.Value()))
			ctx.Request.Header.Set("x-bf-codex-key", "configured-account-id")
			ctx.Request.Header.Set("Sec-Fetch-Site", "same-origin")
			auth.APIMiddleware()(h.managementAuth(func(ctx *fasthttp.RequestCtx) {
				ctx.SetStatusCode(204)
			}))(ctx)
			require.Equal(t, 204, ctx.Response.StatusCode(), string(ctx.Response.Body()))
		})
	}
}

func TestCodexRoutesDoNotTrustUpstreamBearerOrOwner(t *testing.T) {
	r := router.New()
	NewCodexHandler(deniedCodexStore{}).RegisterRoutes(r)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/codex/connections/current"},
		{"POST", "/api/codex/connections"},
		{"POST", "/api/codex/connections/victim/poll"},
		{"DELETE", "/api/codex/connections/victim"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.Request.SetRequestURI(route.path)
			ctx.Request.Header.SetMethod(route.method)
			ctx.Request.Header.Set("Authorization", "Bearer upstream-secret")
			ctx.Request.Header.Set("ChatGPT-Account-ID", "victim")
			ctx.Request.Header.Set("x-bf-vk", "not-a-gateway-key")
			r.Handler(&ctx)
			if ctx.Response.StatusCode() != 401 {
				t.Fatalf("status=%d body=%s", ctx.Response.StatusCode(), ctx.Response.Body())
			}
			if string(ctx.Response.Header.Peek("Cache-Control")) != "no-store" {
				t.Fatal("auth response cacheable")
			}
			if strings.Contains(string(ctx.Response.Body()), "upstream-secret") {
				t.Fatal("credential reflected")
			}
		})
	}
}
