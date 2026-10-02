package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

const (
	issuer      = "https://auth.openai.com"
	resource    = "https://api.openai.com/v1"
	redirectURI = "http://127.0.0.1:1455/auth/callback"
)

type client struct {
	http   *http.Client
	issuer string
}

type tokenSet struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}

type identity struct{ Subject, Email string }

type discovery struct {
	JWKSURI            string `json:"jwks_uri"`
	RevocationEndpoint string `json:"revocation_endpoint"`
}

func newClient() *client {
	return &client{http: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, issuer: issuer}
}

func (c *client) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("invalid ChatGPT OAuth request")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("ChatGPT OAuth service unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ChatGPT OAuth service returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, out) != nil {
		return errors.New("invalid ChatGPT OAuth response")
	}
	return nil
}

func (c *client) discover(ctx context.Context) (discovery, error) {
	var d discovery
	err := c.getJSON(ctx, c.issuer+"/.well-known/openid-configuration", &d)
	if err == nil && d.JWKSURI == "" {
		err = errors.New("invalid ChatGPT OAuth discovery response")
	}
	return d, err
}

func (c *client) postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("invalid ChatGPT OAuth request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("ChatGPT OAuth service unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ChatGPT OAuth service returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, out) != nil {
		return errors.New("invalid ChatGPT OAuth response")
	}
	return nil
}

func (c *client) exchange(ctx context.Context, form url.Values) (tokenSet, error) {
	var t tokenSet
	err := c.postForm(ctx, c.issuer+"/api/accounts/oauth/token", form, &t)
	if err == nil && (t.AccessToken == "" || t.RefreshToken == "" || t.IDToken == "") {
		err = errors.New("incomplete ChatGPT OAuth token response")
	}
	return t, err
}

func hasRequiredScopes(scope string) bool {
	seen := map[string]bool{}
	for _, s := range strings.Fields(scope) {
		seen[s] = true
	}
	return seen["chatgpt.tokens.use.direct"] && seen["resource.invoke"]
}

func (c *client) verifyIDToken(ctx context.Context, raw, audience, nonce, subject string) (identity, error) {
	d, err := c.discover(ctx)
	if err != nil {
		return identity{}, err
	}
	var keys jose.JSONWebKeySet
	if err = c.getJSON(ctx, d.JWKSURI, &keys); err != nil {
		return identity{}, err
	}
	var claims struct {
		jwt.RegisteredClaims
		Email string `json:"email"`
		Nonce string `json:"nonce"`
	}
	verified := false
	for _, key := range keys.Keys {
		_, err = jwt.ParseWithClaims(raw, &claims, func(*jwt.Token) (any, error) {
			return key.Key, nil
		}, jwt.WithValidMethods([]string{"RS256", "ES256"}), jwt.WithIssuer(c.issuer), jwt.WithAudience(audience), jwt.WithExpirationRequired())
		if err == nil {
			verified = true
			break
		}
	}
	if !verified || claims.Subject == "" {
		return identity{}, errors.New("invalid ChatGPT identity token")
	}
	if nonce != "" && claims.Nonce != nonce {
		return identity{}, errors.New("invalid ChatGPT identity nonce")
	}
	if subject != "" && claims.Subject != subject {
		return identity{}, errors.New("ChatGPT identity changed")
	}
	return identity{Subject: claims.Subject, Email: claims.Email}, nil
}

func (c *client) revoke(ctx context.Context, refresh, clientID string) error {
	d, err := c.discover(ctx)
	if err != nil {
		return err
	}
	if d.RevocationEndpoint == "" {
		return errors.New("ChatGPT OAuth revocation endpoint unavailable")
	}
	return c.postForm(ctx, d.RevocationEndpoint, url.Values{"token": {refresh}, "token_type_hint": {"refresh_token"}, "client_id": {clientID}}, nil)
}
