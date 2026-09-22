package handlers

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

func TestHeadroomDashboardSession(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer s.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	h := newHeadroomHandler(strings.Repeat("x", 32), port)
	for _, tc := range []struct {
		name                   string
		admin, bypass, session bool
		want                   int
	}{
		{"session", true, false, true, 200},
		{"bypassed", true, true, true, 401},
		{"no session", true, false, false, 401},
		{"non admin", false, false, true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ctx fasthttp.RequestCtx
			ctx.SetUserValue(schemas.IsLocalAdminContextKey, tc.admin)
			ctx.SetUserValue(schemas.BifrostContextKeyAuthBypassed, tc.bypass)
			if tc.session {
				ctx.SetUserValue(schemas.BifrostContextKeySessionToken, "verified-session")
			}
			h.events(&ctx)
			if ctx.Response.StatusCode() != tc.want {
				t.Fatalf("got %d want %d", ctx.Response.StatusCode(), tc.want)
			}
		})
	}
}

func TestHeadroomHandlerAuthAndFixedDestination(t *testing.T) {
	token := strings.Repeat("x", 32)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong target or auth")
		}
		w.Write([]byte(`{"schema_version":1,"events":[]}`))
	}))
	defer s.Close()
	_, portText, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
	port, _ := strconv.Atoi(portText)
	h := newHeadroomHandler(token, port)
	for _, provided := range []string{"", "wrong", token} {
		var ctx fasthttp.RequestCtx
		ctx.Request.SetRequestURI("/api/headroom/events?url=https://attacker.invalid/secret")
		ctx.Request.Header.Set("X-Headroom-Admin-Token", provided)
		h.events(&ctx)
		want := 401
		if provided == token {
			want = 200
		}
		if ctx.Response.StatusCode() != want {
			t.Fatal(ctx.Response.StatusCode())
		}
	}
	var ctx fasthttp.RequestCtx
	newHeadroomHandler("", port).events(&ctx)
	if ctx.Response.StatusCode() != 503 {
		t.Fatal("missing admin token did not fail closed")
	}
}

func TestHeadroomHandlerRedirectTimeoutAndMalformed(t *testing.T) {
	for _, kind := range []string{"redirect", "timeout", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					http.Redirect(w, r, "http://169.254.169.254/", 302)
				case "timeout":
					<-r.Context().Done()
				case "malformed":
					w.Write([]byte("not json"))
				}
			}))
			defer s.Close()
			_, portText, _ := net.SplitHostPort(strings.TrimPrefix(s.URL, "http://"))
			port, _ := strconv.Atoi(portText)
			token := strings.Repeat("t", 32)
			h := newHeadroomHandler(token, port)
			h.client.Timeout = 20 * time.Millisecond
			var ctx fasthttp.RequestCtx
			ctx.Request.Header.Set("X-Headroom-Admin-Token", token)
			h.events(&ctx)
			want := 503
			if kind == "malformed" {
				want = 502
			}
			if ctx.Response.StatusCode() != want {
				t.Fatal(ctx.Response.StatusCode())
			}
		})
	}
}
