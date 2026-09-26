package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fasthttp/router"
	"github.com/golang-jwt/jwt/v5"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// A TLS IdP with real RSA signatures, discovery, JWKS and a PKCE-enforcing
// token endpoint. All credentials here are disposable test fixtures.
type dashboardIdPFixture struct {
	h          *SessionHandler
	server     *httptest.Server
	key        *rsa.PrivateKey
	signingKey *rsa.PrivateKey
	claims     jwt.MapClaims
	verifier   string
	exchanges  atomic.Int32
}

func newDashboardIdPFixture(t *testing.T) *dashboardIdPFixture {
	t.Helper()
	SetLogger(&mockLogger{})
	encrypt.Init("oidc-test-encryption-key-not-production", &mockLogger{})
	t.Cleanup(func() { encrypt.Init("", &mockLogger{}) })
	f := &dashboardIdPFixture{}
	var err error
	f.key, err = rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "jwks_uri": f.server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes())}}})
		case "/token":
			f.exchanges.Add(1)
			id, secret, ok := r.BasicAuth()
			if !ok || id != "test-client" || secret != "test-secret" || r.ParseForm() != nil || r.Form.Get("code_verifier") != f.verifier || r.Form.Get("code") != "test-code" || r.Form.Get("redirect_uri") != "https://gateway.example/api/session/oidc/callback" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, f.claims)
			token.Header["kid"] = "test"
			key := f.key
			if f.signingKey != nil {
				key = f.signingKey
			}
			signed, err := token.SignedString(key)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "never-store-upstream-access", "refresh_token": "never-store-upstream-refresh", "token_type": "Bearer", "id_token": signed})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	for key, value := range map[string]string{"ISSUER": f.server.URL, "CLIENT_ID": "test-client", "CLIENT_SECRET": "test-secret", "REDIRECT_URL": "https://gateway.example/api/session/oidc/callback", "ALLOWED_SUBJECTS": "12345"} {
		t.Setenv("BIFROST_OIDC_"+key, value)
	}
	store := newRealOAuth2Store(t)
	hash, err := encrypt.Hash("recovery-password")
	require.NoError(t, err)
	require.NoError(t, store.UpdateAuthConfig(context.Background(), &configstore.AuthConfig{AdminUserName: schemas.NewSecretVar("admin"), AdminPassword: schemas.NewSecretVar(hash), IsEnabled: true}))
	f.h = NewSessionHandler(store, nil)
	require.NoError(t, f.h.ConfigureOIDCFromEnv())
	f.h.oidc.client = f.server.Client()
	f.h.oidc.client.Timeout = time.Second * 5
	f.h.oidc.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return f
}

func (f *dashboardIdPFixture) start(t *testing.T) (string, string) {
	t.Helper()
	ctx := formPostCtx("")
	ctx.Request.Header.Set("Origin", "https://gateway.example")
	f.h.oidcLogin(ctx)
	require.Equal(t, 303, ctx.Response.StatusCode())
	u, err := url.Parse(string(ctx.Response.Header.Peek("Location")))
	require.NoError(t, err)
	require.Equal(t, f.server.URL+"/authorize", u.Scheme+"://"+u.Host+u.Path)
	q := u.Query()
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.Equal(t, "openid", q.Get("scope"))
	cookie := &fasthttp.Cookie{}
	cookie.SetKey(oidcBrowserCookie)
	require.True(t, ctx.Response.Header.Cookie(cookie))
	require.True(t, cookie.Secure())
	require.True(t, cookie.HTTPOnly())
	require.Equal(t, fasthttp.CookieSameSiteLaxMode, cookie.SameSite())
	var row tables.OIDCLogin
	require.NoError(t, f.h.configStore.DB().First(&row).Error)
	require.NotContains(t, row.Secret, q.Get("nonce"))
	require.NotContains(t, row.Secret, "Verifier")
	plaintext, err := encrypt.Decrypt(row.Secret)
	require.NoError(t, err)
	var secret oidcLoginSecret
	require.NoError(t, json.Unmarshal([]byte(plaintext), &secret))
	f.verifier = secret.Verifier
	require.Equal(t, pkceChallenge(f.verifier), q.Get("code_challenge"))
	f.claims = jwt.MapClaims{"iss": f.server.URL, "sub": "12345", "aud": "test-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": q.Get("nonce")}
	return q.Get("state"), string(cookie.Value())
}

func dashboardCallback(state, browser string) *fasthttp.RequestCtx {
	ctx := getCtx("https://gateway.example" + oidcCallbackPath + "?code=test-code&state=" + url.QueryEscape(state))
	ctx.Request.Header.SetCookie(oidcBrowserCookie, browser)
	return ctx
}

func TestDashboardOIDCSessionLifecycle(t *testing.T) {
	f := newDashboardIdPFixture(t)
	state, browser := f.start(t)
	ctx := dashboardCallback(state, browser)
	f.h.oidcCallback(ctx)
	require.Equal(t, "/workspace", string(ctx.Response.Header.Peek("Location")))
	cookie := &fasthttp.Cookie{}
	cookie.SetKey("token")
	require.True(t, ctx.Response.Header.Cookie(cookie))
	require.True(t, cookie.Secure())
	require.True(t, cookie.HTTPOnly())
	value := string(cookie.Value())
	require.True(t, validateSession(bgCtx(), f.h.configStore, value))
	// Exercise the real admission middleware: verified session, not an identity
	// header or upstream token, is what confers local-administrator permission.
	am, err := InitAuthMiddleware(f.h.configStore, nil, nil, "")
	require.NoError(t, err)
	r := router.New()
	f.h.RegisterRoutes(r, am.APIMiddleware())
	r.GET("/api/protected", am.APIMiddleware()(func(ctx *fasthttp.RequestCtx) {
		require.Equal(t, true, ctx.UserValue(schemas.IsLocalAdminContextKey))
		require.Equal(t, value, ctx.UserValue(schemas.BifrostContextKeySessionToken))
		ctx.SetStatusCode(204)
	}))
	protected := getCtx("/api/protected")
	protected.Request.Header.SetCookie("token", value)
	r.Handler(protected)
	require.Equal(t, 204, protected.Response.StatusCode())
	bearer := getCtx("/api/protected")
	bearer.Request.Header.Set("Authorization", "Bearer "+value)
	r.Handler(bearer)
	require.Equal(t, 204, bearer.Response.StatusCode())
	spoofed := getCtx("/api/protected")
	spoofed.Request.Header.Set("X-Forwarded-User", "12345")
	spoofed.Request.Header.Set("Authorization", "Bearer never-store-upstream-access")
	r.Handler(spoofed)
	require.Equal(t, 401, spoofed.Response.StatusCode())
	var stored struct {
		Token, OIDCIssuer, OIDCSubject string
		ExpiresAt                      time.Time
	}
	require.NoError(t, f.h.configStore.DB().Table("sessions").First(&stored).Error)
	require.NotEqual(t, value, stored.Token)
	require.NotContains(t, stored.Token, "never-store-upstream")
	require.Equal(t, f.server.URL, stored.OIDCIssuer)
	require.Equal(t, "12345", stored.OIDCSubject)
	require.WithinDuration(t, time.Unix(f.claims["exp"].(int64), 0), stored.ExpiresAt, time.Second)
	f.h.oidcCallback(dashboardCallback(state, browser))
	require.EqualValues(t, 1, f.exchanges.Load(), "state replay must not call token endpoint")
	t.Setenv("BIFROST_OIDC_ALLOWED_SUBJECTS", "someone-else")
	require.False(t, validateSession(bgCtx(), f.h.configStore, value))
	t.Setenv("BIFROST_OIDC_ALLOWED_SUBJECTS", "12345")
	logout := formPostCtx("")
	logout.Request.Header.SetCookie("token", value)
	f.h.logout(logout)
	require.Equal(t, 200, logout.Response.StatusCode())
	require.False(t, validateSession(bgCtx(), f.h.configStore, value))
	login := formPostCtx(`{"username":"admin","password":"recovery-password"}`)
	f.h.login(login)
	require.Equal(t, 403, login.Response.StatusCode())
	require.Empty(t, login.Response.Header.Peek("Set-Cookie"))
}

func TestDashboardOIDCRejectsPasswordCredentials(t *testing.T) {
	f := newDashboardIdPFixture(t)
	passwordSession := &tables.SessionsTable{Token: "old-password-session", ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, f.h.configStore.CreateSession(context.Background(), passwordSession))
	am, err := InitAuthMiddleware(f.h.configStore, nil, nil, "")
	require.NoError(t, err)
	protected := am.APIMiddleware()(func(ctx *fasthttp.RequestCtx) { ctx.SetStatusCode(204) })
	credentials := base64.StdEncoding.EncodeToString([]byte("admin:recovery-password"))
	for _, configured := range []bool{true, false} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			if !configured {
				for _, key := range []string{"ISSUER", "CLIENT_ID", "CLIENT_SECRET", "REDIRECT_URL", "ALLOWED_SUBJECTS"} {
					t.Setenv("BIFROST_OIDC_"+key, "")
				}
				require.NoError(t, f.h.ConfigureOIDCFromEnv())
			}
			want := 204
			if configured {
				want = 401
			}
			for _, authorization := range []string{"Basic " + credentials, "Bearer " + credentials, "Bearer old-password-session", ""} {
				ctx := getCtx("/api/protected")
				ctx.Request.Header.Set("Authorization", authorization)
				if authorization == "" {
					ctx.Request.Header.SetCookie("token", "old-password-session")
				}
				protected(ctx)
				require.Equal(t, want, ctx.Response.StatusCode(), authorization)
			}
			login := formPostCtx(`{"username":"admin","password":"recovery-password"}`)
			f.h.login(login)
			if configured {
				require.Equal(t, 403, login.Response.StatusCode())
			} else {
				require.Equal(t, 200, login.Response.StatusCode())
			}
		})
	}
}

func TestDashboardOIDCRejectsInvalidIdentity(t *testing.T) {
	f := newDashboardIdPFixture(t)
	for _, tc := range []struct {
		name, claim string
		value       any
		reason      string
	}{
		{"issuer", "iss", "https://attacker.example", "invalid"},
		{"audience", "aud", "other-client", "invalid"},
		{"nonce", "nonce", "attacker-nonce", "invalid"},
		{"subject", "sub", "67890", "denied"},
		{"expired", "exp", time.Now().Add(-time.Hour).Unix(), "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, browser := f.start(t)
			f.claims[tc.claim] = tc.value
			ctx := dashboardCallback(state, browser)
			f.h.oidcCallback(ctx)
			require.Equal(t, "/login?oidc_error="+tc.reason, string(ctx.Response.Header.Peek("Location")))
			require.Empty(t, ctx.Response.Header.Peek("Authorization"))
			var count int64
			require.NoError(t, f.h.configStore.DB().Model(&tables.SessionsTable{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestDashboardOIDCStateBoundary(t *testing.T) {
	f := newDashboardIdPFixture(t)
	for _, kind := range []string{"cookie", "state", "expired", "cancelled", "config-changed"} {
		t.Run(kind, func(t *testing.T) {
			state, browser := f.start(t)
			ctx := dashboardCallback(state, browser)
			reason := "invalid"
			switch kind {
			case "cookie":
				ctx.Request.Header.SetCookie(oidcBrowserCookie, strings.Repeat("a", 43))
			case "state":
				ctx.QueryArgs().Set("state", strings.Repeat("b", 43))
			case "expired":
				require.NoError(t, f.h.configStore.DB().Model(&tables.OIDCLogin{}).Where("state_hash = ?", encrypt.HashSHA256(state)).Update("expires_at", time.Now().Add(-time.Minute)).Error)
			case "cancelled":
				ctx.QueryArgs().Set("error", "access_denied")
				reason = "cancelled"
			case "config-changed":
				f.h.oidc.clientSecret = "rotated"
				defer func() { f.h.oidc.clientSecret = "test-secret" }()
			}
			f.h.oidcCallback(ctx)
			require.Equal(t, "/login?oidc_error="+reason, string(ctx.Response.Header.Peek("Location")))
			require.Zero(t, f.exchanges.Load())
			require.NoError(t, f.h.configStore.DB().Where("1 = 1").Delete(&tables.OIDCLogin{}).Error)
		})
	}
	for _, origin := range []string{"", "https://attacker.example", "https://gateway.example.attacker.example"} {
		ctx := formPostCtx("")
		ctx.Request.Header.Set("Origin", origin)
		ctx.Request.Header.Set("Host", "gateway.example")
		f.h.oidcLogin(ctx)
		require.Equal(t, "/login?oidc_error=invalid", string(ctx.Response.Header.Peek("Location")))
	}
}

func TestDashboardOIDCConcurrentReplicaCallbacks(t *testing.T) {
	f := newDashboardIdPFixture(t)
	state, browser := f.start(t)
	other := NewSessionHandler(f.h.configStore, nil)
	require.NoError(t, other.ConfigureOIDCFromEnv())
	other.oidc.client = f.h.oidc.client
	var wg sync.WaitGroup
	for _, h := range []*SessionHandler{f.h, other} {
		wg.Add(1)
		go func(h *SessionHandler) { defer wg.Done(); h.oidcCallback(dashboardCallback(state, browser)) }(h)
	}
	wg.Wait()
	require.EqualValues(t, 1, f.exchanges.Load())
	var count int64
	require.NoError(t, f.h.configStore.DB().Model(&tables.SessionsTable{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestDashboardOIDCConfiguration(t *testing.T) {
	f := newDashboardIdPFixture(t)
	for key, values := range map[string][]string{
		"ISSUER":           {"", "http://idp.example", "https://user:pass@idp.example", "https://idp.example?x=y"},
		"REDIRECT_URL":     {"https://gateway.example/other", "http://gateway.example/api/session/oidc/callback", "https://gateway.example/api/session/oidc/callback?next=evil"},
		"ALLOWED_SUBJECTS": {"", "*", "12345,"}, "CLIENT_SECRET": {""}, "CLIENT_ID": {""},
	} {
		for _, value := range values {
			t.Run(key+value, func(t *testing.T) {
				t.Setenv("BIFROST_OIDC_"+key, value)
				_, err := loadDashboardOIDC()
				require.Error(t, err)
				require.False(t, passwordAuthAllowed(), "invalid OIDC must not allow password authentication")
				require.False(t, oidcSessionAllowed(&tables.SessionsTable{}))
			})
		}
	}
	encrypt.Init("", &mockLogger{})
	require.Error(t, f.h.ConfigureOIDCFromEnv())
}

func TestDashboardOIDCSignatureAndTokenFailures(t *testing.T) {
	f := newDashboardIdPFixture(t)
	for _, kind := range []string{"signature", "pkce", "missing-nonce"} {
		t.Run(kind, func(t *testing.T) {
			state, browser := f.start(t)
			switch kind {
			case "signature":
				key, err := rsa.GenerateKey(rand.Reader, 2048)
				require.NoError(t, err)
				f.signingKey = key
				defer func() { f.signingKey = nil }()
			case "pkce":
				f.verifier = "wrong-verifier"
			case "missing-nonce":
				delete(f.claims, "nonce")
			}
			ctx := dashboardCallback(state, browser)
			f.h.oidcCallback(ctx)
			require.Equal(t, "/login?oidc_error=invalid", string(ctx.Response.Header.Peek("Location")))
			var count int64
			require.NoError(t, f.h.configStore.DB().Model(&tables.SessionsTable{}).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestDashboardOIDCDiscoveryRejectsCrossOrigin(t *testing.T) {
	for _, field := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri"} {
		t.Run(field, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadata := map[string]any{"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token", "jwks_uri": server.URL + "/jwks"}
				metadata[field] = "https://attacker.example/steal"
				_ = json.NewEncoder(w).Encode(metadata)
			}))
			defer server.Close()
			c := &dashboardOIDC{issuer: server.URL, client: server.Client()}
			_, err := c.discover(context.Background())
			require.ErrorContains(t, err, "configured issuer origin")
			require.Nil(t, c.provider)
		})
	}
}
