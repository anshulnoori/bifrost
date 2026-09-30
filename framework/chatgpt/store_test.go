package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	encrypt.Init("chatgpt-store-test-encryption-key", bifrost.NewDefaultLogger(schemas.LogLevelInfo))
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(MigrationModels()...))
	store, err := NewStore(func() *gorm.DB { return db })
	require.NoError(t, err)
	return store, db
}

func TestStartBuildsPKCEAuthorizationAndReusesHost(t *testing.T) {
	store, _ := testStore(t)
	first, err := store.Start(context.Background(), "owner-one")
	require.NoError(t, err)
	second, err := store.Start(context.Background(), "owner-two")
	require.NoError(t, err)

	firstURL, err := url.Parse(first.AuthorizationURL)
	require.NoError(t, err)
	secondURL, err := url.Parse(second.AuthorizationURL)
	require.NoError(t, err)
	q := firstURL.Query()
	require.Equal(t, "/api/accounts/authorize", firstURL.Path)
	require.Equal(t, "S256", q.Get("code_challenge_method"))
	require.NotEmpty(t, q.Get("code_challenge"))
	require.Equal(t, redirectURI, q.Get("redirect_uri"))
	require.Equal(t, resource, q.Get("resource"))
	require.ElementsMatch(t, []string{"openid", "profile", "email", "offline_access", "chatgpt.tokens.use.direct", "resource.invoke"}, splitScope(q.Get("scope")))
	require.Equal(t, q.Get("ext_agent_host_id"), secondURL.Query().Get("ext_agent_host_id"))
}

func TestCallbackValuesRejectsAmbiguousOrNonCallbackURLs(t *testing.T) {
	valid, err := callbackValues(redirectURI + "?code=a&state=b&client_id=c")
	require.NoError(t, err)
	require.Equal(t, "a", valid.Get("code"))

	invalid := []string{
		"http://user@127.0.0.1:1455/auth/callback?code=a",
		redirectURI + "?code=a#fragment",
		redirectURI + "?code=a&code=b",
		"http://localhost:1455/auth/callback?code=a",
	}
	for _, raw := range invalid {
		_, err := callbackValues(raw)
		require.ErrorIs(t, err, ErrInvalidCallback, raw)
	}
}

func TestRequiredPlanScopes(t *testing.T) {
	require.True(t, hasRequiredScopes("openid resource.invoke chatgpt.tokens.use.direct"))
	require.False(t, hasRequiredScopes("openid resource.invoke"))
	require.False(t, hasRequiredScopes("openid chatgpt.tokens.use.direct"))
}

func splitScope(scope string) []string {
	return strings.Fields(scope)
}

type oauthFixture struct {
	store          *Store
	server         *httptest.Server
	key            *rsa.PrivateKey
	nonce, scopes  string
	tokenCalls     atomic.Int32
	refreshStarted chan struct{}
	refreshRelease chan struct{}
}

func newOAuthFixture(t *testing.T) *oauthFixture {
	t.Helper()
	store, _ := testStore(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &oauthFixture{store: store, key: key, scopes: "openid resource.invoke chatgpt.tokens.use.direct"}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]string{"jwks_uri": f.server.URL + "/jwks", "revocation_endpoint": f.server.URL + "/revoke"})
		case "/jwks":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, Use: "sig", Algorithm: "RS256"}}})
		case "/api/accounts/oauth/token":
			require.NoError(t, r.ParseForm())
			require.Equal(t, resource, r.Form.Get("resource"))
			require.Equal(t, "oaiapp_test", r.Form.Get("client_id"))
			f.tokenCalls.Add(1)
			if r.Form.Get("grant_type") == "refresh_token" && f.refreshStarted != nil {
				close(f.refreshStarted)
				<-f.refreshRelease
			}
			json.NewEncoder(w).Encode(tokenSet{AccessToken: "new-access", RefreshToken: "new-refresh", IDToken: f.signed(t, map[string]any{"iss": f.server.URL, "sub": "subscriber", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "nonce": f.nonce, "email": "owner@example.com"}), Scope: f.scopes, ExpiresIn: 3600})
		case "/revoke":
			require.NoError(t, r.ParseForm())
			require.Equal(t, "refresh_token", r.Form.Get("token_type_hint"))
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	store.client.issuer = f.server.URL
	t.Cleanup(f.server.Close)
	return f
}

func (f *oauthFixture) signed(t *testing.T, claims map[string]any) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims)).SignedString(f.key)
	require.NoError(t, err)
	return token
}

func (f *oauthFixture) start(t *testing.T, owner string) (*Login, string) {
	t.Helper()
	login, err := f.store.Start(context.Background(), owner)
	require.NoError(t, err)
	u, err := url.Parse(login.AuthorizationURL)
	require.NoError(t, err)
	f.nonce = u.Query().Get("nonce")
	callback := redirectURI + "?" + url.Values{"state": {u.Query().Get("state")}, "code": {"one-time-code"}, "client_id": {"oaiapp_test"}}.Encode()
	return login, callback
}

func TestCompleteValidatesIdentityScopesAndOwnerBeforePersisting(t *testing.T) {
	f := newOAuthFixture(t)
	login, callback := f.start(t, "owner")
	_, err := f.store.Complete(context.Background(), "different-owner", login.ID, callback)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.store.Complete(context.Background(), "owner", login.ID, strings.Replace(callback, "one-time-code", "one-time-code&state=wrong", 1))
	require.ErrorIs(t, err, ErrInvalidCallback)
	require.Equal(t, int32(0), f.tokenCalls.Load())
	row, err := f.store.Complete(context.Background(), "owner", login.ID, callback)
	require.NoError(t, err)
	require.Equal(t, "connected", row.State)
	require.Equal(t, "owner@example.com", row.Email)
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "new-access")
	require.NotContains(t, string(encoded), "new-refresh")
	_, err = f.store.Complete(context.Background(), "owner", login.ID, callback)
	require.Error(t, err)
	require.Equal(t, int32(1), f.tokenCalls.Load())
	_, err = f.store.Credential(context.Background(), "different-owner", row.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestSignedIdentityValidationRejectsMissingOrWrongClaims(t *testing.T) {
	f := newOAuthFixture(t)
	for _, field := range []string{"iss", "aud", "nonce", "sub", "exp"} {
		claims := map[string]any{"iss": f.server.URL, "sub": "subscriber", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"}
		if field == "exp" {
			delete(claims, field)
		} else {
			claims[field] = "wrong"
		}
		_, err := f.store.client.verifyIDToken(context.Background(), f.signed(t, claims), "oaiapp_test", "nonce", "subscriber")
		require.Error(t, err, field)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	original := f.key
	f.key = otherKey
	token := f.signed(t, map[string]any{"iss": f.server.URL, "sub": "subscriber", "aud": "oaiapp_test", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce"})
	f.key = original
	_, err = f.store.client.verifyIDToken(context.Background(), token, "oaiapp_test", "nonce", "subscriber")
	require.Error(t, err)
}

func TestFailedReauthorizationPreservesWorkingRegistration(t *testing.T) {
	f := newOAuthFixture(t)
	first, callback := f.start(t, "owner")
	connected, err := f.store.Complete(context.Background(), "owner", first.ID, callback)
	require.NoError(t, err)
	second, callback := f.start(t, "owner")
	authorizationURL, err := url.Parse(second.AuthorizationURL)
	require.NoError(t, err)
	require.Equal(t, "oaiapp_test", authorizationURL.Query().Get("client_id"))
	require.Equal(t, "owner@example.com", authorizationURL.Query().Get("login_hint"))
	require.NotEmpty(t, authorizationURL.Query().Get("id_token_hint"))
	require.Empty(t, authorizationURL.Query().Get("agent_name_hint"))
	f.scopes = "openid resource.invoke"
	_, err = f.store.Complete(context.Background(), "owner", second.ID, callback)
	require.ErrorIs(t, err, ErrReconnect)
	current, err := f.store.Current(context.Background(), "owner")
	require.NoError(t, err)
	require.Equal(t, connected.ID, current.ID)
	access, err := f.store.Credential(context.Background(), "owner", connected.ID)
	require.NoError(t, err)
	require.Equal(t, "new-access", access)
}

func TestRefreshHasOneWinnerAndExpiredLeaseRequiresReconnect(t *testing.T) {
	f := newOAuthFixture(t)
	login, callback := f.start(t, "owner")
	row, err := f.store.Complete(context.Background(), "owner", login.ID, callback)
	require.NoError(t, err)
	require.NoError(t, f.store.db().Model(&Connection{}).Where("id = ?", row.ID).Update("expires_at", time.Now().Add(-time.Hour)).Error)
	f.refreshStarted, f.refreshRelease = make(chan struct{}), make(chan struct{})
	results := make(chan error, 2)
	go func() { _, err := f.store.Credential(context.Background(), "owner", row.ID); results <- err }()
	<-f.refreshStarted
	go func() { _, err := f.store.Credential(context.Background(), "owner", row.ID); results <- err }()
	close(f.refreshRelease)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, int32(2), f.tokenCalls.Load())
	require.NoError(t, f.store.db().Model(&Connection{}).Where("id = ?", row.ID).Updates(map[string]any{"state": "refreshing", "operation_until": time.Now().Add(-time.Minute)}).Error)
	_, err = f.store.Credential(context.Background(), "owner", row.ID)
	require.ErrorIs(t, err, ErrReconnect)
	require.Equal(t, int32(2), f.tokenCalls.Load())
}

func TestDisconnectClearsTokensButRetainsRegistrationForNextSignIn(t *testing.T) {
	f := newOAuthFixture(t)
	login, callback := f.start(t, "owner")
	connected, err := f.store.Complete(context.Background(), "owner", login.ID, callback)
	require.NoError(t, err)
	require.NoError(t, f.store.Disconnect(context.Background(), "owner", connected.ID))
	var retained Connection
	require.NoError(t, f.store.db().Where("owner = ?", "owner").First(&retained).Error)
	require.Equal(t, "disconnected", retained.State)
	require.Empty(t, retained.Secret)
	require.Equal(t, "oaiapp_test", retained.ClientID)
	require.Equal(t, "subscriber", retained.Subject)
	_, err = f.store.Credential(context.Background(), "owner", connected.ID)
	require.ErrorIs(t, err, ErrReconnect)
	next, callback := f.start(t, "owner")
	authorizationURL, err := url.Parse(next.AuthorizationURL)
	require.NoError(t, err)
	require.Equal(t, "oaiapp_test", authorizationURL.Query().Get("client_id"))
	require.Empty(t, authorizationURL.Query().Get("id_token_hint"))
	reconnected, err := f.store.Complete(context.Background(), "owner", next.ID, callback)
	require.NoError(t, err)
	require.Equal(t, "connected", reconnected.State)
}
