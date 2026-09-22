package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/encrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type quietLogger struct{ schemas.Logger }

func (quietLogger) Warn(string, ...any) {}

func fixtureToken(account string) string {
	payload := fmt.Sprintf(`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, time.Now().Add(time.Hour).Unix(), account)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".fixture"
}

func testStore(t *testing.T, handler http.HandlerFunc) (*Store, *gorm.DB) {
	t.Helper()
	encrypt.Init("codex-test-only-encryption-key-32-bytes", quietLogger{})
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "credentials.db")+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err = db.AutoMigrate(&Connection{}); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(func() *gorm.DB { return db })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	s.client.issuer = server.URL
	return s, db
}

func insertConnected(t *testing.T, db *gorm.DB, owner, id string) Connection {
	t.Helper()
	row := Connection{ID: id, Owner: owner, State: "connected", ExpiresAt: time.Now().Add(-time.Minute)}
	var err error
	row.Secret, err = seal(envelope{ID: id, Owner: owner, Tokens: tokens{Access: "old-access", Refresh: "old-refresh", Account: "account-A"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	return row
}

func TestDeviceLifecycle(t *testing.T) {
	var polls atomic.Int32
	expectedAccess := fixtureToken("account-A")
	s, db := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			fmt.Fprint(w, `{"device_auth_id":"secret-device","user_code":"ABCD-EFGH","interval":"2"}`)
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["device_auth_id"] != "secret-device" || body["user_code"] != "ABCD-EFGH" {
				t.Error("wrong poll credentials")
			}
			if polls.Add(1) == 1 {
				w.WriteHeader(403)
				return
			}
			fmt.Fprint(w, `{"authorization_code":"one-use-code","code_verifier":"verifier"}`)
		case "/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code_verifier") != "verifier" || r.Form.Get("client_id") != ClientID {
				t.Error("wrong code exchange")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": expectedAccess, "refresh_token": "secret-refresh"})
		default:
			t.Error("unexpected auth request")
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()
	login, err := s.Start(ctx, "user:A")
	if err != nil {
		t.Fatal(err)
	}
	if login.IntervalSeconds != 2 || login.VerificationURL != Issuer+"/codex/device" {
		t.Fatal("invalid onboarding")
	}
	if _, err = s.Poll(ctx, "user:B", login.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-owner poll allowed", err)
	}
	if _, err = s.Poll(ctx, "user:A", login.ID); !errors.Is(err, ErrPending) || polls.Load() != 0 {
		t.Fatal("poll interval ignored")
	}
	for i := 0; i < 2; i++ {
		if err = db.Model(&Connection{}).Where("id = ?", login.ID).Update("next_poll", time.Now().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		row, pollErr := s.Poll(ctx, "user:A", login.ID)
		if i == 0 && !errors.Is(pollErr, ErrPending) {
			t.Fatal(pollErr)
		}
		if i == 1 && (pollErr != nil || row.State != "connected") {
			t.Fatal(row.State, pollErr)
		}
	}
	row, err := s.load(ctx, "user:A", login.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Secret, "secret-refresh") || strings.Contains(row.Secret, "account-A") {
		t.Fatal("plaintext storage")
	}
	public, _ := json.Marshal(row)
	if strings.Contains(string(public), row.Secret) || strings.Contains(string(public), "user:A") {
		t.Fatal("secret in API response")
	}
	access, account, err := s.Credential(ctx, "user:A", login.ID)
	if err != nil || access != expectedAccess || account != "account-A" {
		t.Fatal("invalid credential", err)
	}
	if err = s.Disconnect(ctx, "user:B", login.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-owner disconnect allowed")
	}
	if err = s.Disconnect(ctx, "user:A", login.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Credential(ctx, "user:A", login.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("disconnected credential usable")
	}
}

func TestRefreshReplicaExclusionAndDisconnect(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprintf("disconnect=%t", disconnect), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			s, db := testStore(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_ = r.ParseForm()
				if r.Form.Get("refresh_token") != "old-refresh" {
					t.Error("wrong refresh token")
				}
				close(entered)
				<-release
				_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fixtureToken("account-A"), "refresh_token": "rotated-refresh"})
			})
			insertConnected(t, db, "user:A", "connection-A")
			// A second Store has no shared in-memory lock with the first.
			replica, err := NewStore(s.db)
			if err != nil {
				t.Fatal(err)
			}
			replica.client = s.client
			done := make(chan error, 1)
			go func() { _, _, err := s.Credential(context.Background(), "user:A", "connection-A"); done <- err }()
			<-entered
			_, _, err = replica.Credential(context.Background(), "user:A", "connection-A")
			if !errors.Is(err, ErrBusy) {
				t.Error("replica not excluded", err)
			}
			if disconnect {
				if err = replica.Disconnect(context.Background(), "user:A", "connection-A"); err != nil {
					t.Error(err)
				}
			}
			close(release)
			err = <-done
			if disconnect {
				if !errors.Is(err, ErrBusy) {
					t.Fatal("refresh resurrected deleted row", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				row, err := replica.load(context.Background(), "user:A", "connection-A")
				if err != nil {
					t.Fatal(err)
				}
				e, err := unseal(row)
				if err != nil || e.Tokens.Refresh != "rotated-refresh" {
					t.Fatal("rotation not persisted", err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("duplicate token refresh")
			}
		})
	}
}

func TestRefreshFailureAndCrashedOwnerRequireReconnect(t *testing.T) {
	s, db := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":"secret-refresh"}`)
	})
	insertConnected(t, db, "user:A", "failed")
	_, _, err := s.Credential(context.Background(), "user:A", "failed")
	if !errors.Is(err, ErrReconnect) || strings.Contains(err.Error(), "secret-refresh") {
		t.Fatal(err)
	}
	row, _ := s.Status(context.Background(), "user:A", "failed")
	if row.State != "reconnect_required" || row.Secret != "" {
		t.Fatal("failed refresh retained usable secret")
	}
	insertConnected(t, db, "user:A", "crashed")
	if err = db.Model(&Connection{}).Where("id = ?", "crashed").Updates(map[string]any{"state": "refreshing", "operation_until": time.Now().Add(-time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	row, err = s.Status(context.Background(), "user:A", "crashed")
	if err != nil || row.State != "reconnect_required" || row.Secret != "" {
		t.Fatal("unsafe stale refresh takeover", err)
	}
}

func TestCiphertextCannotMoveBetweenOwners(t *testing.T) {
	_, db := testStore(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected network") })
	row := insertConnected(t, db, "user:A", "row-A")
	row.Owner = "user:B"
	if _, err := unseal(row); err == nil {
		t.Fatal("ciphertext owner swap accepted")
	}
	row.Owner, row.ID = "user:A", "row-B"
	if _, err := unseal(row); err == nil {
		t.Fatal("ciphertext row swap accepted")
	}
}

func TestExpiryAndCancellation(t *testing.T) {
	s, db := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/deviceauth/usercode" {
			t.Error("expired flow reached upstream")
		}
		fmt.Fprint(w, `{"device_auth_id":"device","user_code":"CODE","interval":5}`)
	})
	ctx := context.Background()
	login, err := s.Start(ctx, "user:A")
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&Connection{}).Where("id = ?", login.ID).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	row, err := s.Poll(ctx, "user:A", login.ID)
	if !errors.Is(err, ErrReconnect) || row.State != "expired" || row.Secret != "" {
		t.Fatal("expired flow not erased", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.Start(cancelled, "user:A"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not propagated", err)
	}
}

func TestRefreshAccountChangeRejected(t *testing.T) {
	s, db := testStore(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": fixtureToken("account-B"), "refresh_token": "different-account-refresh"})
	})
	insertConnected(t, db, "user:A", "account-change")
	if _, _, err := s.Credential(context.Background(), "user:A", "account-change"); !errors.Is(err, ErrReconnect) {
		t.Fatal("account change accepted", err)
	}
}

func TestEncryptionRequired(t *testing.T) {
	encrypt.Init("", quietLogger{})
	if _, err := NewStore(func() *gorm.DB { return nil }); err == nil {
		t.Fatal("plaintext credential store allowed")
	}
	if _, err := seal(envelope{Owner: "user:A"}); err == nil {
		t.Fatal("plaintext seal allowed")
	}
}
