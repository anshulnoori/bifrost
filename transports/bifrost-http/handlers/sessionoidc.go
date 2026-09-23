package handlers

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/valyala/fasthttp"
	"golang.org/x/oauth2"
)

const oidcCallbackPath = "/api/session/oidc/callback"
const oidcBrowserCookie = "__Host-bifrost_oidc"

type dashboardOIDC struct {
	issuer, clientID, clientSecret, redirectURL string
	subjects                                    []string
	client                                      *http.Client
	mu                                          sync.Mutex
	provider                                    *oidc.Provider
}

func loadDashboardOIDC() (*dashboardOIDC, error) {
	c := &dashboardOIDC{
		issuer: os.Getenv("BIFROST_OIDC_ISSUER"), clientID: os.Getenv("BIFROST_OIDC_CLIENT_ID"),
		clientSecret: os.Getenv("BIFROST_OIDC_CLIENT_SECRET"), redirectURL: os.Getenv("BIFROST_OIDC_REDIRECT_URL"),
	}
	allowed := os.Getenv("BIFROST_OIDC_ALLOWED_SUBJECTS")
	if c.issuer+c.clientID+c.clientSecret+c.redirectURL+allowed == "" {
		return nil, nil
	}
	if c.clientID == "" || c.clientSecret == "" || allowed == "" {
		return nil, errors.New("OIDC requires client ID, client secret and explicit allowed subjects")
	}
	issuer, err := url.Parse(c.issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return nil, errors.New("OIDC issuer must be an absolute HTTPS URL without credentials, query or fragment")
	}
	redirect, err := url.Parse(c.redirectURL)
	if err != nil || redirect.Scheme != "https" || redirect.Host == "" || redirect.User != nil || redirect.RawQuery != "" || redirect.Fragment != "" || redirect.Path != oidcCallbackPath || redirect.RawPath != "" {
		return nil, errors.New("OIDC redirect URL must be the exact public HTTPS /api/session/oidc/callback URL")
	}
	for _, subject := range strings.Split(allowed, ",") {
		subject = strings.TrimSpace(subject)
		if subject == "" || subject == "*" {
			return nil, errors.New("OIDC requires explicit nonempty subjects; wildcards are not allowed")
		}
		c.subjects = append(c.subjects, subject)
	}
	c.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("OIDC redirects are not allowed") }}
	return c, nil
}

// ConfigureOIDCFromEnv is called at startup. Invalid partial configuration fails
// startup rather than enabling a permissive or half-configured login method.
func (h *SessionHandler) ConfigureOIDCFromEnv() error {
	c, err := loadDashboardOIDC()
	if err != nil {
		return err
	}
	if c != nil && (h.configStore == nil || !encrypt.IsEnabled()) {
		return errors.New("OIDC requires an encrypted database config store")
	}
	h.oidc = c
	return nil
}

// Recheck the current allowlist for every OIDC session, including after restart
// or removing OIDC configuration. Password recovery sessions are independent.
func oidcSessionAllowed(session *tables.SessionsTable) bool {
	if session.OIDCIssuer == "" && session.OIDCSubject == "" {
		return true
	}
	c, err := loadDashboardOIDC()
	return err == nil && c != nil && session.OIDCIssuer == c.issuer && slices.Contains(c.subjects, session.OIDCSubject)
}

func (c *dashboardOIDC) discover(ctx context.Context) (*oidc.Provider, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider != nil {
		return c.provider, nil
	}
	p, err := oidc.NewProvider(oidc.ClientContext(ctx, c.client), c.issuer)
	if err != nil {
		return nil, errors.New("OIDC discovery unavailable")
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := p.Claims(&metadata); err != nil {
		return nil, err
	}
	issuer, _ := url.Parse(c.issuer)
	for _, endpoint := range []string{p.Endpoint().AuthURL, p.Endpoint().TokenURL, metadata.JWKS} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Host != issuer.Host || u.User != nil || u.Fragment != "" {
			return nil, errors.New("OIDC endpoints must use the configured issuer origin")
		}
	}
	c.provider = p
	return p, nil
}

func (c *dashboardOIDC) oauth(p *oidc.Provider) oauth2.Config {
	endpoint := p.Endpoint()
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	return oauth2.Config{ClientID: c.clientID, ClientSecret: c.clientSecret, RedirectURL: c.redirectURL, Endpoint: endpoint, Scopes: []string{oidc.ScopeOpenID}}
}

type oidcLoginSecret struct {
	StateHash, BrowserHash, Nonce, Verifier, ConfigHash string
	ExpiresAt                                           time.Time
}

func (c *dashboardOIDC) binding() string {
	return encrypt.HashSHA256(c.issuer + "\x00" + c.clientID + "\x00" + c.clientSecret + "\x00" + c.redirectURL)
}

func oidcCookie(ctx *fasthttp.RequestCtx, value string, expires time.Time) {
	cookie := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(cookie)
	cookie.SetKey(oidcBrowserCookie)
	cookie.SetValue(value)
	cookie.SetExpire(expires)
	cookie.SetPath("/")
	cookie.SetSecure(true)
	cookie.SetHTTPOnly(true)
	cookie.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	ctx.Response.Header.SetCookie(cookie)
}

func oidcError(ctx *fasthttp.RequestCtx, reason string) {
	// Keep local redirects relative: RequestCtx.Redirect builds an absolute URL
	// from the untrusted Host and the backend's (often HTTP) connection scheme.
	ctx.Response.Header.Set("Location", "/login?oidc_error="+reason)
	ctx.SetStatusCode(fasthttp.StatusSeeOther)
}

func (h *SessionHandler) oidcReady(ctx *fasthttp.RequestCtx) bool {
	ctx.Response.Header.Set("Cache-Control", "no-store")
	ctx.Response.Header.Set("Referrer-Policy", "no-referrer")
	if h.oidc == nil || h.configStore == nil || !encrypt.IsEnabled() {
		return false
	}
	config, err := h.configStore.GetAuthConfig(ctx)
	return err == nil && config != nil && config.IsEnabled
}

// Start is a same-origin form POST, never a caller-supplied issuer or redirect.
func (h *SessionHandler) oidcLogin(ctx *fasthttp.RequestCtx) {
	if !h.oidcReady(ctx) {
		oidcError(ctx, "unavailable")
		return
	}
	c := h.oidc
	u, _ := url.Parse(c.redirectURL)
	if string(ctx.Request.Header.Peek("Origin")) != u.Scheme+"://"+u.Host {
		oidcError(ctx, "invalid")
		return
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, err := c.discover(requestCtx)
	if err != nil {
		oidcError(ctx, "unavailable")
		return
	}
	state, browser := oauth2.GenerateVerifier(), oauth2.GenerateVerifier()
	expires := time.Now().Add(5 * time.Minute)
	secret := oidcLoginSecret{StateHash: encrypt.HashSHA256(state), BrowserHash: encrypt.HashSHA256(browser), Nonce: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier(), ConfigHash: c.binding(), ExpiresAt: expires}
	data, err := json.Marshal(secret)
	if err != nil {
		oidcError(ctx, "unavailable")
		return
	}
	ciphertext, err := encrypt.Encrypt(string(data))
	if err != nil {
		oidcError(ctx, "unavailable")
		return
	}
	db := h.configStore.DB().WithContext(requestCtx)
	// Bound abandoned state lifetime; starting again cancels this browser's old flow.
	if err := db.Where("expires_at <= ? OR browser_hash = ?", time.Now(), encrypt.HashSHA256(string(ctx.Request.Header.Cookie(oidcBrowserCookie)))).Delete(&tables.OIDCLogin{}).Error; err != nil {
		oidcError(ctx, "unavailable")
		return
	}
	if err := db.Create(&tables.OIDCLogin{StateHash: secret.StateHash, BrowserHash: secret.BrowserHash, Secret: ciphertext, ExpiresAt: expires}).Error; err != nil {
		oidcError(ctx, "unavailable")
		return
	}
	oidcCookie(ctx, browser, expires)
	oauth := c.oauth(p)
	ctx.Redirect(oauth.AuthCodeURL(state, oidc.Nonce(secret.Nonce), oauth2.S256ChallengeOption(secret.Verifier)), fasthttp.StatusSeeOther)
}

func (h *SessionHandler) oidcCallback(ctx *fasthttp.RequestCtx) {
	if !h.oidcReady(ctx) {
		oidcError(ctx, "unavailable")
		return
	}
	c := h.oidc
	state, browser := string(ctx.QueryArgs().Peek("state")), string(ctx.Request.Header.Cookie(oidcBrowserCookie))
	if len(state) != 43 || len(browser) != 43 {
		oidcError(ctx, "invalid")
		return
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := h.configStore.DB().WithContext(requestCtx)
	var row tables.OIDCLogin
	if err := db.Where("state_hash = ? AND browser_hash = ? AND expires_at > ?", encrypt.HashSHA256(state), encrypt.HashSHA256(browser), time.Now()).First(&row).Error; err != nil {
		oidcError(ctx, "invalid")
		return
	}
	// Compare-and-delete is the cross-replica one-use boundary. A losing callback
	// cannot exchange a code or mint a session, even after reading the same row.
	consumed := db.Where("state_hash = ? AND browser_hash = ? AND expires_at > ?", row.StateHash, row.BrowserHash, time.Now()).Delete(&tables.OIDCLogin{})
	if consumed.Error != nil || consumed.RowsAffected != 1 {
		oidcError(ctx, "invalid")
		return
	}
	oidcCookie(ctx, "", time.Unix(1, 0))
	plaintext, err := encrypt.Decrypt(row.Secret)
	var secret oidcLoginSecret
	if err != nil || json.Unmarshal([]byte(plaintext), &secret) != nil || secret.StateHash != row.StateHash || secret.BrowserHash != row.BrowserHash || secret.ConfigHash != c.binding() || !secret.ExpiresAt.After(time.Now()) {
		oidcError(ctx, "invalid")
		return
	}
	if len(ctx.QueryArgs().Peek("error")) != 0 {
		oidcError(ctx, "cancelled")
		return
	}
	code := string(ctx.QueryArgs().Peek("code"))
	if code == "" || len(code) > 8192 {
		oidcError(ctx, "invalid")
		return
	}
	p, err := c.discover(requestCtx)
	if err != nil {
		oidcError(ctx, "unavailable")
		return
	}
	oauth := c.oauth(p)
	token, err := oauth.Exchange(oidc.ClientContext(requestCtx, c.client), code, oauth2.VerifierOption(secret.Verifier))
	if err != nil {
		oidcError(ctx, "invalid")
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		oidcError(ctx, "invalid")
		return
	}
	id, err := p.Verifier(&oidc.Config{ClientID: c.clientID}).Verify(oidc.ClientContext(requestCtx, c.client), raw)
	if err != nil || subtle.ConstantTimeCompare([]byte(id.Nonce), []byte(secret.Nonce)) != 1 {
		oidcError(ctx, "invalid")
		return
	}
	if !slices.Contains(c.subjects, id.Subject) {
		oidcError(ctx, "denied")
		return
	}
	// The IdP's access/refresh/ID tokens are never persisted or forwarded. Mint
	// an independent, revocable local session, no longer than the ID-token TTL.
	expires := time.Now().Add(12 * time.Hour)
	if id.Expiry.Before(expires) {
		expires = id.Expiry
	}
	value := oauth2.GenerateVerifier()
	session := &tables.SessionsTable{Token: value, ExpiresAt: expires, OIDCIssuer: c.issuer, OIDCSubject: id.Subject}
	if err := h.configStore.CreateSession(requestCtx, session); err != nil {
		oidcError(ctx, "unavailable")
		return
	}
	cookie := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(cookie)
	cookie.SetKey("token")
	cookie.SetValue(value)
	cookie.SetExpire(expires)
	cookie.SetPath("/")
	cookie.SetSecure(true)
	cookie.SetHTTPOnly(true)
	cookie.SetSameSite(fasthttp.CookieSameSiteLaxMode)
	ctx.Response.Header.SetCookie(cookie)
	ctx.Response.Header.Set("Location", "/workspace")
	ctx.SetStatusCode(fasthttp.StatusSeeOther)
}
