// Package chatgpt implements ChatGPT subscription OAuth credential storage.
package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNotFound        = errors.New("ChatGPT connection not found")
	ErrBusy            = errors.New("ChatGPT credential operation in progress")
	ErrReconnect       = errors.New("ChatGPT authorization requires reconnect")
	ErrInvalidCallback = errors.New("invalid ChatGPT OAuth callback")
)

type Connection struct {
	ID             string    `gorm:"primaryKey;size:36" json:"id"`
	Owner          string    `gorm:"uniqueIndex;not null" json:"-"`
	State          string    `gorm:"not null" json:"state"`
	Email          string    `json:"email,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	Subject        string    `json:"-"`
	ClientID       string    `json:"-"`
	Secret         string    `gorm:"type:text" json:"-"`
	Version        uint64    `json:"-"`
	OperationUntil time.Time `json:"-"`
	CreatedAt      time.Time `json:"-"`
	UpdatedAt      time.Time `json:"-"`
}

func (Connection) TableName() string { return "chatgpt_connections" }

type ConnectionMetadata struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	Email     string    `json:"email,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type attempt struct {
	ID             string `gorm:"primaryKey;size:36"`
	Owner          string `gorm:"uniqueIndex;not null"`
	State          string
	Secret         string `gorm:"type:text"`
	ExpiresAt      time.Time
	Version        uint64
	OperationUntil time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (attempt) TableName() string { return "chatgpt_oauth_attempts" }

type host struct {
	ID     uint   `gorm:"primaryKey"`
	HostID string `gorm:"not null"`
}

func (host) TableName() string { return "chatgpt_oauth_hosts" }

// MigrationModels returns all private persistence models owned by this package.
func MigrationModels() []any { return []any{&Connection{}, &attempt{}, &host{}} }

type attemptSecret struct{ ID, Owner, State, Nonce, Verifier, ClientID, Subject string }
type credentialSecret struct {
	ID, Owner    string
	AccessToken  string
	RefreshToken string
	IDToken      string
	Scope        string
}

type Store struct {
	db     func() *gorm.DB
	client *client
}

func lockOwner(tx *gorm.DB, owner string) error {
	keyID, ok := strings.CutPrefix(owner, "provider:chatgpt:")
	if !ok {
		return nil
	}
	var provider tables.TableProvider
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("name = ?", "chatgpt").First(&provider).Error; err != nil {
		return err
	}
	var key tables.TableKey
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("provider_id = ? AND key_id = ?", provider.ID, keyID).First(&key).Error
}

func NewStore(db func() *gorm.DB) (*Store, error) {
	if db == nil || db() == nil {
		return nil, errors.New("ChatGPT credential database unavailable")
	}
	if !encrypt.IsEnabled() {
		return nil, encrypt.ErrEncryptionKeyNotInitialized
	}
	return &Store{db: db, client: newClient()}, nil
}

func seal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", errors.New("could not encrypt ChatGPT credentials")
	}
	s, err := encrypt.Encrypt(string(b))
	if err != nil {
		return "", errors.New("could not encrypt ChatGPT credentials")
	}
	return s, nil
}
func open(raw string, out any) error {
	s, err := encrypt.Decrypt(raw)
	if err != nil || json.Unmarshal([]byte(s), out) != nil {
		return errors.New("could not decrypt ChatGPT credentials")
	}
	return nil
}
func randomValue(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type Login struct {
	Connection
	AuthorizationURL string `json:"authorization_url"`
}

func (s *Store) Start(ctx context.Context, owner string) (*Login, error) {
	if owner == "" {
		return nil, ErrNotFound
	}
	state, err := randomValue(32)
	if err != nil {
		return nil, errors.New("could not start ChatGPT authorization")
	}
	nonce, err := randomValue(32)
	if err != nil {
		return nil, errors.New("could not start ChatGPT authorization")
	}
	verifier, err := randomValue(64)
	if err != nil {
		return nil, errors.New("could not start ChatGPT authorization")
	}
	now := time.Now().UTC()
	a := attempt{ID: uuid.NewString(), Owner: owner, State: state, ExpiresAt: now.Add(15 * time.Minute)}
	authorization := attemptSecret{ID: a.ID, Owner: owner, State: state, Nonce: nonce, Verifier: verifier}
	var hostID, loginHint, idTokenHint string
	err = s.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOwner(tx, owner); err != nil {
			return err
		}
		var registration Connection
		if err := tx.Select("id", "client_id", "subject", "email", "secret").Where("owner = ?", owner).First(&registration).Error; err == nil {
			authorization.ClientID, authorization.Subject = registration.ClientID, registration.Subject
			loginHint = registration.Email
			if registration.Secret != "" {
				var credentials credentialSecret
				if open(registration.Secret, &credentials) == nil && credentials.ID == registration.ID && credentials.Owner == owner {
					idTokenHint = credentials.IDToken
				}
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		a.Secret, err = seal(authorization)
		if err != nil {
			return err
		}
		var h host
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&h, 1).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			h = host{ID: 1, HostID: "urn:uuid:" + uuid.NewString()}
			if err = tx.Create(&h).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		hostID = h.HostID
		if err := tx.Where("owner = ?", owner).Delete(&attempt{}).Error; err != nil {
			return err
		}
		return tx.Create(&a).Error
	})
	if err != nil {
		return nil, errors.New("could not persist ChatGPT authorization")
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {"dynamic_agent_client"}, "agent_name_hint": {"Bifrost"}, "ext_agent_host_id": {hostID}, "response_type": {"code"}, "redirect_uri": {redirectURI}, "resource": {resource}, "scope": {"openid profile email offline_access chatgpt.tokens.use.direct resource.invoke"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "state": {state}, "nonce": {nonce}}
	if authorization.ClientID != "" {
		q.Set("client_id", authorization.ClientID)
		q.Del("agent_name_hint")
		if loginHint != "" {
			q.Set("login_hint", loginHint)
		}
		if idTokenHint != "" {
			q.Set("id_token_hint", idTokenHint)
		}
	}
	return &Login{Connection: Connection{ID: a.ID, State: "pending", ExpiresAt: a.ExpiresAt}, AuthorizationURL: s.client.issuer + "/api/accounts/authorize?" + q.Encode()}, nil
}

func (s *Store) Current(ctx context.Context, owner string) (Connection, error) {
	var row Connection
	err := s.db().WithContext(ctx).Select("id", "owner", "state", "email", "expires_at", "subject", "client_id", "version").Where("owner = ?", owner).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		var a attempt
		if attemptErr := s.db().WithContext(ctx).Select("id", "owner", "expires_at").Where("owner = ?", owner).First(&a).Error; attemptErr == nil {
			state := "pending"
			if !a.ExpiresAt.After(time.Now()) {
				state = "expired"
			}
			return Connection{ID: a.ID, Owner: a.Owner, State: state, ExpiresAt: a.ExpiresAt}, nil
		} else if !errors.Is(attemptErr, gorm.ErrRecordNotFound) {
			return row, errors.New("ChatGPT credential store unavailable")
		}
		return row, ErrNotFound
	}
	if err != nil {
		return row, errors.New("ChatGPT credential store unavailable")
	}
	return row, nil
}
func (s *Store) CurrentMetadata(ctx context.Context, owner string) (ConnectionMetadata, error) {
	var m ConnectionMetadata
	err := s.db().WithContext(ctx).Model(&Connection{}).Select("id", "state", "email", "expires_at").Where("owner = ?", owner).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		current, currentErr := s.Current(ctx, owner)
		if currentErr != nil {
			return m, currentErr
		}
		return ConnectionMetadata{ID: current.ID, State: current.State, Email: current.Email, ExpiresAt: current.ExpiresAt}, nil
	}
	if err != nil {
		return m, errors.New("ChatGPT credential store unavailable")
	}
	return m, nil
}

func callbackValues(raw string) (url.Values, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host != "127.0.0.1:1455" || u.Path != "/auth/callback" || u.Fragment != "" || u.User != nil {
		return nil, ErrInvalidCallback
	}
	q := u.Query()
	for _, v := range q {
		if len(v) != 1 {
			return nil, ErrInvalidCallback
		}
	}
	return q, nil
}

func (s *Store) Complete(ctx context.Context, owner, id, callbackURL string) (Connection, error) {
	q, err := callbackValues(callbackURL)
	if err != nil {
		return Connection{}, err
	}
	var a attempt
	err = s.db().WithContext(ctx).Where("id = ? AND owner = ?", id, owner).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Connection{}, ErrNotFound
	}
	if err != nil {
		return Connection{}, errors.New("ChatGPT credential store unavailable")
	}
	if !a.ExpiresAt.After(time.Now()) {
		_ = s.db().WithContext(ctx).Where("id = ? AND owner = ?", id, owner).Delete(&attempt{}).Error
		return Connection{}, ErrReconnect
	}
	var as attemptSecret
	if err = open(a.Secret, &as); err != nil || as.ID != id || as.Owner != owner || as.State != a.State {
		return Connection{}, ErrInvalidCallback
	}
	if q.Get("state") != as.State {
		return Connection{}, ErrInvalidCallback
	}
	if q.Get("error") != "" {
		return Connection{}, ErrReconnect
	}
	code, clientID := q.Get("code"), q.Get("client_id")
	if as.ClientID != "" {
		if clientID != "" && clientID != as.ClientID {
			return Connection{}, ErrInvalidCallback
		}
		clientID = as.ClientID
	}
	if code == "" || clientID == "" || clientID == "dynamic_agent_client" {
		return Connection{}, ErrInvalidCallback
	}
	if a.Version != 0 {
		return Connection{}, ErrReconnect
	}
	leaseUntil := time.Now().UTC().Add(30 * time.Second)
	claimed := s.db().WithContext(ctx).Model(&attempt{}).Where("id = ? AND owner = ? AND state = ? AND version = ? AND operation_until < ?", id, owner, a.State, a.Version, time.Now().UTC()).Updates(map[string]any{"version": a.Version + 1, "operation_until": leaseUntil})
	if claimed.Error != nil {
		return Connection{}, errors.New("ChatGPT credential store unavailable")
	}
	if claimed.RowsAffected != 1 {
		return Connection{}, ErrBusy
	}
	opctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := s.client.exchange(opctx, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {as.Verifier}, "redirect_uri": {redirectURI}, "resource": {resource}})
	if err != nil {
		_ = s.db().WithContext(context.WithoutCancel(ctx)).Where("id = ? AND owner = ? AND version = ?", id, owner, a.Version+1).Delete(&attempt{}).Error
		return Connection{}, err
	}
	if !hasRequiredScopes(t.Scope) {
		_ = s.db().WithContext(context.WithoutCancel(ctx)).Where("id = ? AND owner = ? AND version = ?", id, owner, a.Version+1).Delete(&attempt{}).Error
		return Connection{}, ErrReconnect
	}
	ident, err := s.client.verifyIDToken(opctx, t.IDToken, clientID, as.Nonce, as.Subject)
	if err != nil {
		_ = s.db().WithContext(context.WithoutCancel(ctx)).Where("id = ? AND owner = ? AND version = ?", id, owner, a.Version+1).Delete(&attempt{}).Error
		return Connection{}, err
	}
	secret, err := seal(credentialSecret{ID: id, Owner: owner, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, IDToken: t.IDToken, Scope: t.Scope})
	if err != nil {
		return Connection{}, err
	}
	row := Connection{ID: id, Owner: owner, State: "connected", Email: ident.Email, ExpiresAt: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second), Subject: ident.Subject, ClientID: clientID, Secret: secret}
	err = s.db().WithContext(context.WithoutCancel(ctx)).Transaction(func(tx *gorm.DB) error {
		if err := lockOwner(tx, owner); err != nil {
			return err
		}
		var existing Connection
		if err := tx.Where("owner = ?", owner).First(&existing).Error; err == nil && existing.State == "revoking" {
			return ErrBusy
		} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var current attempt
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND owner = ?", id, owner).First(&current).Error; err != nil {
			return ErrBusy
		}
		if current.State != a.State || current.Version != a.Version+1 {
			return ErrBusy
		}
		if err := tx.Where("owner = ?", owner).Delete(&Connection{}).Error; err != nil {
			return err
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Delete(&current).Error
	})
	if err != nil {
		return Connection{}, err
	}
	row.Secret = ""
	return row, nil
}

func (s *Store) Credential(ctx context.Context, owner, id string) (string, error) {
	for {
		access, err := s.credential(ctx, owner, id)
		if !errors.Is(err, ErrBusy) {
			return access, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func (s *Store) credential(ctx context.Context, owner, id string) (string, error) {
	var row Connection
	err := s.db().WithContext(ctx).Where("id = ? AND owner = ?", id, owner).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", errors.New("ChatGPT credential store unavailable")
	}
	if row.State == "refreshing" || row.State == "revoking" {
		if row.OperationUntil.After(time.Now()) {
			return "", ErrBusy
		}
		res := s.db().WithContext(ctx).Model(&Connection{}).Where("id = ? AND owner = ? AND version = ? AND state = ?", id, owner, row.Version, row.State).Updates(map[string]any{"state": "reconnect_required", "version": row.Version + 1, "operation_until": nil})
		if res.Error != nil {
			return "", errors.New("ChatGPT credential store unavailable")
		}
		if res.RowsAffected == 0 {
			return "", ErrBusy
		}
		return "", ErrReconnect
	}
	if row.State != "connected" {
		return "", ErrReconnect
	}
	var old credentialSecret
	if open(row.Secret, &old) != nil || old.ID != id || old.Owner != owner {
		return "", ErrReconnect
	}
	if row.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return old.AccessToken, nil
	}
	leaseUntil := time.Now().UTC().Add(30 * time.Second)
	res := s.db().WithContext(ctx).Model(&Connection{}).Where("id = ? AND owner = ? AND version = ? AND state = ?", id, owner, row.Version, "connected").Updates(map[string]any{"state": "refreshing", "version": row.Version + 1, "operation_until": leaseUntil})
	if res.Error != nil {
		return "", errors.New("ChatGPT credential store unavailable")
	}
	if res.RowsAffected != 1 {
		return "", ErrBusy
	}
	opctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := s.client.exchange(opctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {row.ClientID}, "refresh_token": {old.RefreshToken}, "resource": {resource}})
	if err != nil {
		_ = s.failOperation(ctx, row, "refreshing")
		return "", ErrReconnect
	}
	if !hasRequiredScopes(t.Scope) {
		_ = s.failOperation(ctx, row, "refreshing")
		return "", ErrReconnect
	}
	ident, err := s.client.verifyIDToken(opctx, t.IDToken, row.ClientID, "", row.Subject)
	if err != nil {
		_ = s.failOperation(ctx, row, "refreshing")
		return "", ErrReconnect
	}
	secret, err := seal(credentialSecret{ID: id, Owner: owner, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, IDToken: t.IDToken, Scope: t.Scope})
	if err != nil {
		_ = s.failOperation(ctx, row, "refreshing")
		return "", err
	}
	save := s.db().WithContext(context.WithoutCancel(ctx)).Model(&Connection{}).Where("id = ? AND owner = ? AND version = ? AND state = ?", id, owner, row.Version+1, "refreshing").Updates(map[string]any{"state": "connected", "version": row.Version + 2, "operation_until": nil, "secret": secret, "email": ident.Email, "expires_at": time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)})
	if save.Error != nil || save.RowsAffected != 1 {
		return "", ErrBusy
	}
	return t.AccessToken, nil
}
func (s *Store) failOperation(ctx context.Context, row Connection, state string) error {
	return s.db().WithContext(context.WithoutCancel(ctx)).Model(&Connection{}).Where("id = ? AND owner = ? AND version = ? AND state = ?", row.ID, row.Owner, row.Version+1, state).Updates(map[string]any{"state": "reconnect_required", "version": row.Version + 2, "operation_until": nil}).Error
}

func (s *Store) Disconnect(ctx context.Context, owner, id string) error {
	var row Connection
	err := s.db().WithContext(ctx).Where("id = ? AND owner = ?", id, owner).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		result := s.db().WithContext(ctx).Where("id = ? AND owner = ?", id, owner).Delete(&attempt{})
		if result.Error != nil {
			return errors.New("could not disconnect ChatGPT")
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}
		return nil
	}
	if err != nil {
		return errors.New("ChatGPT credential store unavailable")
	}
	if row.State != "connected" && row.State != "reconnect_required" {
		return ErrBusy
	}
	var secret credentialSecret
	if open(row.Secret, &secret) != nil {
		return ErrReconnect
	}
	leaseUntil := time.Now().UTC().Add(30 * time.Second)
	err = s.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOwner(tx, owner); err != nil {
			return err
		}
		claim := tx.Model(&Connection{}).Where("id = ? AND owner = ? AND version = ? AND state = ?", id, owner, row.Version, row.State).Updates(map[string]any{"state": "revoking", "version": row.Version + 1, "operation_until": leaseUntil})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected != 1 {
			return ErrBusy
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return err
		}
		return errors.New("ChatGPT credential store unavailable")
	}
	opctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err = s.client.revoke(opctx, secret.RefreshToken, row.ClientID); err != nil {
		_ = s.failOperation(ctx, row, "revoking")
		return err
	}
	err = s.db().WithContext(context.WithoutCancel(ctx)).Transaction(func(tx *gorm.DB) error {
		if err := lockOwner(tx, owner); err != nil {
			return err
		}
		res := tx.Model(&Connection{}).Where("id = ? AND owner = ? AND version = ? AND state = ?", id, owner, row.Version+1, "revoking").Updates(map[string]any{"state": "disconnected", "secret": "", "version": row.Version + 2, "operation_until": nil})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return ErrBusy
		}
		return tx.Where("owner = ?", owner).Delete(&attempt{}).Error
	})
	if err != nil {
		if errors.Is(err, ErrBusy) {
			return err
		}
		return errors.New("could not disconnect ChatGPT")
	}
	return nil
}
