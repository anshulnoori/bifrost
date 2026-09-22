package handlers

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/fasthttp/router"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/bifrost-http/lib"
	"github.com/valyala/fasthttp"
)

// HeadroomHandler exposes metadata only. The explicit token is required even
// when OSS dashboard authentication is disabled. The browser never selects a URL.
type HeadroomHandler struct {
	token    string
	endpoint string
	client   *http.Client
}

func NewHeadroomHandler() *HeadroomHandler {
	port := 9909
	if value := os.Getenv("BIFROST_HEADROOM_METRICS_PORT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 65535 {
			return &HeadroomHandler{}
		}
		port = parsed
	}
	return newHeadroomHandler(os.Getenv("HEADROOM_METRICS_TOKEN"), port)
}

func newHeadroomHandler(token string, port int) *HeadroomHandler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &HeadroomHandler{token: token, endpoint: "http://127.0.0.1:" + strconv.Itoa(port) + "/v1/events", client: &http.Client{
		Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (h *HeadroomHandler) RegisterRoutes(r *router.Router, middlewares ...schemas.BifrostHTTPMiddleware) {
	r.GET("/api/headroom/events", lib.ChainMiddlewares(h.events, middlewares...))
}

func (h *HeadroomHandler) events(ctx *fasthttp.RequestCtx) {
	ctx.Response.Header.Set("Cache-Control", "no-store")
	if len(h.token) < 32 || h.client == nil {
		ctx.Error("Headroom monitoring is not configured", 503)
		return
	}
	if subtle.ConstantTimeCompare(ctx.Request.Header.Peek("X-Headroom-Admin-Token"), []byte(h.token)) != 1 {
		ctx.Error("unauthorized", 401)
		return
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, h.endpoint, nil)
	if err != nil {
		ctx.Error("Headroom monitoring unavailable", 503)
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.client.Do(req)
	if err != nil {
		ctx.Error("Headroom monitoring unavailable", 503)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		ctx.Error("Headroom monitoring unavailable", 503)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 || !json.Valid(data) {
		ctx.Error("invalid Headroom monitoring response", 502)
		return
	}
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(200)
	ctx.SetBody(data)
}
