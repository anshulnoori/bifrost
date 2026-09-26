package lib

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/codex"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/encrypt"
	"github.com/maximhq/bifrost/framework/grant"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type codexOwnerStore struct{ configstore.ConfigStore }

func (codexOwnerStore) GetProviderKey(_ context.Context, provider schemas.ModelProvider, id string) (*schemas.Key, error) {
	if provider != schemas.Codex || id != "account-a" && id != "account-b" {
		return nil, nil
	}
	return &schemas.Key{ID: id}, nil
}

func TestCodexOwnerUsesConfiguredProviderAccount(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"", ""}, {"eyJ.upstream.jwt", ""}, {"missing", ""},
		{"account-a", "provider:codex:account-a"}, {"account-b", "provider:codex:account-b"},
	} {
		owner, err := CodexOwner(context.Background(), codexOwnerStore{}, tc.id)
		if owner != tc.want || (err == nil) != (tc.want != "") {
			t.Fatalf("id=%s owner=%s err=%v", tc.id, owner, err)
		}
	}
}

type codexScopeStore struct {
	configstore.ConfigStore
	db *gorm.DB
}

func (s codexScopeStore) DB() *gorm.DB { return s.db }

type codexTestLogger struct{ schemas.Logger }

func (codexTestLogger) Warn(string, ...any) {}

func newCodexScopeStore(t *testing.T) (codexScopeStore, *gorm.DB) {
	t.Helper()
	encrypt.Init("codex-scope-test-encryption-key-32", codexTestLogger{})
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "scope.db")), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, statement := range []string{
		`CREATE TABLE config_providers (id integer primary key, name text not null)`,
		`CREATE TABLE config_keys (id integer primary key, provider_id integer, key_id text, name text, models_json text, blacklisted_models_json text, enabled boolean, codex_reserve_percent real, value text)`,
		`CREATE TABLE governance_virtual_keys (id text primary key, is_active boolean, expires_at datetime)`,
		`CREATE TABLE governance_virtual_key_provider_configs (id integer primary key, virtual_key_id text, provider text, allowed_models text, blacklisted_models text, allow_all_keys boolean)`,
		`CREATE TABLE governance_virtual_key_provider_config_keys (table_virtual_key_provider_config_id integer, table_key_id integer)`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&codex.Connection{}); err != nil {
		t.Fatal(err)
	}
	return codexScopeStore{db: db}, db
}

func codexScopeContext(vkID string, keyIDs ...string) *schemas.BifrostContext {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	g := grant.New()
	permit := grant.NewPermit(grant.PermitVirtualKey, vkID, "test key", true, false, []schemas.ProviderPermit{{
		Provider: string(schemas.Codex), AllowedModels: schemas.WhiteList{"*"}, KeyIDs: keyIDs,
	}}, nil)
	g.SetAccess(grant.NewAccess([]schemas.Permit{permit}, nil, "", nil))
	if vkID != "" {
		g.SetIdentity(grant.NewIdentity(grant.NewCredential(grant.CredentialVirtualKey, "invalid-secret"), nil, &schemas.EntityRef{ID: vkID}, nil, nil, nil, nil))
	}
	ctx.SetGrant(g)
	return ctx
}

func seedCodexScope(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO config_providers (id,name) VALUES (1,'codex')`, nil},
		{`INSERT INTO config_keys (id,provider_id,key_id,name,models_json,blacklisted_models_json,enabled,codex_reserve_percent,value) VALUES (1,1,'account-a','alpha','["gpt-5"]','[]',1,100,'invalid-encrypted-provider-secret'), (2,1,'account-b','beta','["*"]','["blocked"]',1,NULL,'invalid-encrypted-provider-secret'), (3,1,'disabled','off','["*"]','[]',0,NULL,'invalid-encrypted-provider-secret')`, nil},
		{`INSERT INTO governance_virtual_keys (id,is_active,expires_at) VALUES ('vk-1',1,?)`, []any{now.Add(time.Hour)}},
		{`INSERT INTO governance_virtual_key_provider_configs (id,virtual_key_id,provider,allowed_models,blacklisted_models,allow_all_keys) VALUES (10,'vk-1','codex','["*"]','[]',0)`, nil},
		{`INSERT INTO governance_virtual_key_provider_config_keys VALUES (10,2)`, nil},
		{`INSERT INTO codex_connections (id,owner,state,secret,expires_at,created_at,updated_at) VALUES ('conn-a','provider:codex:account-a','connected','not-ciphertext',?,?,?), ('conn-b','provider:codex:account-b','connected','not-ciphertext',?,?,?), ('conn-off','provider:codex:disabled','connected','not-ciphertext',?,?,?)`, []any{now.Add(-time.Hour), now, now, now.Add(-time.Hour), now, now, now.Add(-time.Hour), now, now}},
	}
	for _, statement := range statements {
		if err := db.Exec(statement.query, statement.args...).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexCacheScopeUsesFreshDatabaseAuthorizationMetadata(t *testing.T) {
	store, db := newCodexScopeStore(t)
	seedCodexScope(t, db)
	resolve := CodexCacheScopeResolver(store)
	ctx := codexScopeContext("vk-1", "account-a", "account-b", "disabled") // stale grant still includes A.

	scope, ids, err := resolve(ctx, schemas.Codex, "gpt-5")
	if err != nil || scope == "" || len(ids) != 1 || ids[0] != "account-b" {
		t.Fatalf("fresh VK/key intersection = scope %q ids %v err %v", scope, ids, err)
	}
	// Reserve policy is deliberately irrelevant to cache authorization; account-a
	// becomes eligible as soon as the current VK permits it despite a 100%% reserve.
	if err := db.Exec(`INSERT INTO governance_virtual_key_provider_config_keys VALUES (10,1)`).Error; err != nil {
		t.Fatal(err)
	}
	_, ids, err = resolve(ctx, schemas.Codex, "gpt-5")
	if err != nil || len(ids) != 2 {
		t.Fatalf("reserve policy affected scope: ids %v err %v", ids, err)
	}

	for _, tc := range []struct {
		name, model, pinKey, pinName, want string
	}{
		{"key id pin", "gpt-5", "account-a", "", "account-a"},
		{"key name pin", "gpt-5", "", "beta", "account-b"},
		{"key id overrides name", "gpt-5", "account-a", "beta", "account-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pinned := codexScopeContext("vk-1", "account-a", "account-b")
			pinned.SetValue(schemas.BifrostContextKeyAPIKeyID, tc.pinKey)
			pinned.SetValue(schemas.BifrostContextKeyAPIKeyName, tc.pinName)
			_, got, err := resolve(pinned, schemas.Codex, tc.model)
			if err != nil || len(got) != 1 || got[0] != tc.want {
				t.Fatalf("pin selected %v: %v", got, err)
			}
		})
	}
	if _, _, err := resolve(ctx, schemas.Codex, "blocked"); err == nil {
		t.Fatal("fresh pool admitted a model blocked by every currently usable key")
	}
}

func TestCodexCacheScopeChangesAcrossDisconnectAndReconnect(t *testing.T) {
	store, db := newCodexScopeStore(t)
	seedCodexScope(t, db)
	resolve := CodexCacheScopeResolver(store)
	ctx := codexScopeContext("vk-1", "account-b")
	first, _, err := resolve(ctx, schemas.Codex, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`DELETE FROM codex_connections WHERE owner = 'provider:codex:account-b'`).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(ctx, schemas.Codex, "gpt-5"); err == nil {
		t.Fatal("disconnected account retained a cache scope")
	}
	now := time.Now()
	if err := db.Exec(`INSERT INTO codex_connections (id,owner,state,secret,expires_at,created_at,updated_at) VALUES ('conn-b-new','provider:codex:account-b','connected','still-not-ciphertext',?,?,?)`, now.Add(-time.Hour), now, now).Error; err != nil {
		t.Fatal(err)
	}
	second, _, err := resolve(ctx, schemas.Codex, "gpt-5")
	if err != nil || first == second {
		t.Fatalf("reconnect did not rotate scope: before %q after %q err %v", first, second, err)
	}
}

func TestCodexCacheScopeFailsClosedWithoutIdentity(t *testing.T) {
	store, db := newCodexScopeStore(t)
	seedCodexScope(t, db)
	if _, _, err := CodexCacheScopeResolver(store)(codexScopeContext("", "account-a"), schemas.Codex, "gpt-5"); err == nil {
		t.Fatal("resolved cache authorization without an identity")
	}
}
