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

	errUnavailable = errors.New("ChatGPT credential store unavailable")
)

const (
	attemptLifetime  = 15 * time.Minute
	operationLease   = 30 * time.Second
	operationTimeout = 20 * time.Second
	refreshMargin    = time.Minute
	dynamicClientID  = "dynamic_agent_client"
	requestedScopes  = "openid profile email offline_access chatgpt.tokens.use.direct resource.invoke"
)

// Connection states: connected, refreshing, revoking, reconnect_required,
// disconnected. Pending and expired are reported from attempts only.
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

// attempt is one in-progress authorization. State holds the OAuth state
// parameter, not a lifecycle state.
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

// host holds the deployment's single stable agent host identifier.
type host struct {
	ID     uint   `gorm:"primaryKey"`
	HostID string `gorm:"not null"`
}

func (host) TableName() string { return "chatgpt_oauth_hosts" }

// MigrationModels returns all private persistence models owned by this package.
func MigrationModels() []any { return []any{&Connection{}, &attempt{}, &host{}} }

// Sealed payloads repeat their row ID and owner so ciphertext copied to another
// row does not decrypt into valid credentials there.
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

func NewStore(db func() *gorm.DB) (*Store, error) {
	if db == nil || db() == nil {
		return nil, errors.New("ChatGPT credential database unavailable")
	}
	if !encrypt.IsEnabled() {
		return nil, encrypt.ErrEncryptionKeyNotInitialized
	}
	return &Store{db: db, client: newClient()}, nil
}

// lockOwner locks a provider-owned key row in the same order as config-store
// deletion, so an authorization cannot outlive deletion of its account.
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
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("provider_id = ? AND key_id = ?", provider.ID, keyID).First(&key).Error
}

// transition is the durable compare-and-swap behind every connection state
// change. It succeeds only for the caller that observed version and state.
func transition(db *gorm.DB, row Connection, version uint64, state string, values map[string]any) (bool, error) {
	values["version"] = version + 1
	result := db.Model(&Connection{}).
		Where("id = ? AND owner = ? AND version = ? AND state = ?", row.ID, row.Owner, version, state).
		Updates(values)
	return result.RowsAffected == 1, result.Error
}

// failOperation abandons a claimed refresh or revocation. The caller must
// reconnect because the upstream token may already have rotated.
func (s *Store) failOperation(ctx context.Context, row Connection, state string) error {
	_, err := transition(s.db().WithContext(context.WithoutCancel(ctx)), row, row.Version+1, state,
		map[string]any{"state": "reconnect_required", "operation_until": nil})
	return err
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

// Start records a PKCE authorization attempt. A previous registration keeps
// its dynamic client ID and subject so reauthorization cannot switch identity.
func (s *Store) Start(ctx context.Context, owner string) (*Login, error) {
	if owner == "" {
		return nil, ErrNotFound
	}
	state, stateErr := randomValue(32)
	nonce, nonceErr := randomValue(32)
	verifier, verifierErr := randomValue(64)
	if errors.Join(stateErr, nonceErr, verifierErr) != nil {
		return nil, errors.New("could not start ChatGPT authorization")
	}
	a := attempt{ID: uuid.NewString(), Owner: owner, State: state, ExpiresAt: time.Now().UTC().Add(attemptLifetime)}
	secret := attemptSecret{ID: a.ID, Owner: owner, State: state, Nonce: nonce, Verifier: verifier}
	var hostID, loginHint, idTokenHint string
	err := s.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockOwner(tx, owner); err != nil {
			return err
		}
		var registration Connection
		err := tx.Select("id", "client_id", "subject", "email", "secret").Where("owner = ?", owner).First(&registration).Error
		switch {
		case err == nil:
			secret.ClientID, secret.Subject = registration.ClientID, registration.Subject
			loginHint = registration.Email
			var credentials credentialSecret
			if registration.Secret != "" && open(registration.Secret, &credentials) == nil &&
				credentials.ID == registration.ID && credentials.Owner == owner {
				idTokenHint = credentials.IDToken
			}
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}
		if a.Secret, err = seal(secret); err != nil {
			return err
		}
		if hostID, err = loadHostID(tx); err != nil {
			return err
		}
		if err := tx.Where("owner = ?", owner).Delete(&attempt{}).Error; err != nil {
			return err
		}
		return tx.Create(&a).Error
	})
	if err != nil {
		return nil, errors.New("could not persist ChatGPT authorization")
	}
	return &Login{
		Connection:       Connection{ID: a.ID, State: "pending", ExpiresAt: a.ExpiresAt},
		AuthorizationURL: s.client.issuer + "/api/accounts/authorize?" + authorizationQuery(secret, hostID, loginHint, idTokenHint).Encode(),
	}, nil
}

// loadHostID returns the deployment's agent host ID, creating it once.
func loadHostID(tx *gorm.DB) (string, error) {
	var h host
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&h, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		h = host{ID: 1, HostID: "urn:uuid:" + uuid.NewString()}
		err = tx.Create(&h).Error
	}
	return h.HostID, err
}

// authorizationQuery requests dynamic client registration on first sign-in
// and reuses the registered client afterwards.
func authorizationQuery(secret attemptSecret, hostID, loginHint, idTokenHint string) url.Values {
	challenge := sha256.Sum256([]byte(secret.Verifier))
	q := url.Values{
		"client_id":             {dynamicClientID},
		"agent_name_hint":       {"Bifrost"},
		"ext_agent_host_id":     {hostID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"resource":              {resource},
		"scope":                 {requestedScopes},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"state":                 {secret.State},
		"nonce":                 {secret.Nonce},
	}
	if secret.ClientID == "" {
		return q
	}
	q.Set("client_id", secret.ClientID)
	q.Del("agent_name_hint")
	if loginHint != "" {
		q.Set("login_hint", loginHint)
	}
	if idTokenHint != "" {
		q.Set("id_token_hint", idTokenHint)
	}
	return q
}

// Current returns the owner's connection, or its pending attempt when no
// connection exists yet.
func (s *Store) Current(ctx context.Context, owner string) (Connection, error) {
	db := s.db().WithContext(ctx)
	var row Connection
	err := db.Select("id", "owner", "state", "email", "expires_at", "subject", "client_id", "version").
		Where("owner = ?", owner).First(&row).Error
	if err == nil {
		return row, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return row, errUnavailable
	}
	var a attempt
	err = db.Select("id", "owner", "expires_at").Where("owner = ?", owner).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, ErrNotFound
	}
	if err != nil {
		return row, errUnavailable
	}
	state := "pending"
	if !a.ExpiresAt.After(time.Now()) {
		state = "expired"
	}
	return Connection{ID: a.ID, Owner: a.Owner, State: state, ExpiresAt: a.ExpiresAt}, nil
}

func (s *Store) CurrentMetadata(ctx context.Context, owner string) (ConnectionMetadata, error) {
	var m ConnectionMetadata
	err := s.db().WithContext(ctx).Model(&Connection{}).Select("id", "state", "email", "expires_at").
		Where("owner = ?", owner).Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		current, err := s.Current(ctx, owner)
		if err != nil {
			return m, err
		}
		return ConnectionMetadata{ID: current.ID, State: current.State, Email: current.Email, ExpiresAt: current.ExpiresAt}, nil
	}
	if err != nil {
		return m, errUnavailable
	}
	return m, nil
}

// callbackValues accepts only the registered loopback redirect with
// single-valued parameters.
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

// Complete exchanges the pasted callback URL for tokens. One caller claims the
// attempt; a failed exchange discards it so the code cannot be retried.
func (s *Store) Complete(ctx context.Context, owner, id, callbackURL string) (Connection, error) {
	q, err := callbackValues(callbackURL)
	if err != nil {
		return Connection{}, err
	}
	db := s.db().WithContext(ctx)
	var a attempt
	err = db.Where("id = ? AND owner = ?", id, owner).First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Connection{}, ErrNotFound
	}
	if err != nil {
		return Connection{}, errUnavailable
	}
	if !a.ExpiresAt.After(time.Now()) {
		_ = db.Where("id = ? AND owner = ?", id, owner).Delete(&attempt{}).Error
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
	if code == "" || clientID == "" || clientID == dynamicClientID {
		return Connection{}, ErrInvalidCallback
	}
	if a.Version != 0 {
		return Connection{}, ErrReconnect
	}

	claimed := db.Model(&attempt{}).
		Where("id = ? AND owner = ? AND state = ? AND version = ? AND operation_until < ?", id, owner, a.State, a.Version, time.Now().UTC()).
		Updates(map[string]any{"version": a.Version + 1, "operation_until": time.Now().UTC().Add(operationLease)})
	if claimed.Error != nil {
		return Connection{}, errUnavailable
	}
	if claimed.RowsAffected != 1 {
		return Connection{}, ErrBusy
	}
	discard := func() {
		_ = s.db().WithContext(context.WithoutCancel(ctx)).
			Where("id = ? AND owner = ? AND version = ?", id, owner, a.Version+1).Delete(&attempt{}).Error
	}

	opctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	t, err := s.client.exchange(opctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {as.Verifier},
		"redirect_uri":  {redirectURI},
		"resource":      {resource},
	})
	if err != nil {
		discard()
		return Connection{}, err
	}
	if !hasRequiredScopes(t.Scope) {
		discard()
		return Connection{}, ErrReconnect
	}
	ident, err := s.client.verifyIDToken(opctx, t.IDToken, clientID, as.Nonce, as.Subject)
	if err != nil {
		discard()
		return Connection{}, err
	}
	secret, err := seal(credentialSecret{ID: id, Owner: owner, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, IDToken: t.IDToken, Scope: t.Scope})
	if err != nil {
		return Connection{}, err
	}
	row := Connection{
		ID: id, Owner: owner, State: "connected", Email: ident.Email,
		ExpiresAt: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second),
		Subject:   ident.Subject, ClientID: clientID, Secret: secret,
	}
	// The connection replaces any previous one only while this caller still
	// holds the claimed attempt and no revocation is running.
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

// Credential returns a current access token, refreshing it when close to
// expiry. Callers wait while another replica holds the refresh lease.
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
	db := s.db().WithContext(ctx)
	var row Connection
	err := db.Where("id = ? AND owner = ?", id, owner).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", errUnavailable
	}
	if row.State == "refreshing" || row.State == "revoking" {
		if row.OperationUntil.After(time.Now()) {
			return "", ErrBusy
		}
		// The lease holder died mid-operation; its refresh token may have rotated.
		won, err := transition(db, row, row.Version, row.State, map[string]any{"state": "reconnect_required", "operation_until": nil})
		if err != nil {
			return "", errUnavailable
		}
		if !won {
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
	if row.ExpiresAt.After(time.Now().Add(refreshMargin)) {
		return old.AccessToken, nil
	}

	won, err := transition(db, row, row.Version, "connected", map[string]any{"state": "refreshing", "operation_until": time.Now().UTC().Add(operationLease)})
	if err != nil {
		return "", errUnavailable
	}
	if !won {
		return "", ErrBusy
	}
	opctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	t, err := s.client.exchange(opctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {row.ClientID},
		"refresh_token": {old.RefreshToken},
		"resource":      {resource},
	})
	if err != nil || !hasRequiredScopes(t.Scope) {
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
	won, err = transition(s.db().WithContext(context.WithoutCancel(ctx)), row, row.Version+1, "refreshing", map[string]any{
		"state":           "connected",
		"operation_until": nil,
		"secret":          secret,
		"email":           ident.Email,
		"expires_at":      time.Now().Add(time.Duration(t.ExpiresIn) * time.Second),
	})
	if err != nil || !won {
		return "", ErrBusy
	}
	return t.AccessToken, nil
}

// Disconnect cancels a pending attempt, or revokes the refresh token upstream
// and clears stored tokens. The registration is kept for the next sign-in.
func (s *Store) Disconnect(ctx context.Context, owner, id string) error {
	db := s.db().WithContext(ctx)
	var row Connection
	err := db.Where("id = ? AND owner = ?", id, owner).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		result := db.Where("id = ? AND owner = ?", id, owner).Delete(&attempt{})
		if result.Error != nil {
			return errors.New("could not disconnect ChatGPT")
		}
		if result.RowsAffected != 1 {
			return ErrNotFound
		}
		return nil
	}
	if err != nil {
		return errUnavailable
	}
	if row.State != "connected" && row.State != "reconnect_required" {
		return ErrBusy
	}
	var secret credentialSecret
	if open(row.Secret, &secret) != nil {
		return ErrReconnect
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockOwner(tx, owner); err != nil {
			return err
		}
		won, err := transition(tx, row, row.Version, row.State, map[string]any{"state": "revoking", "operation_until": time.Now().UTC().Add(operationLease)})
		if err == nil && !won {
			err = ErrBusy
		}
		return err
	})
	if errors.Is(err, ErrBusy) {
		return err
	}
	if err != nil {
		return errUnavailable
	}

	opctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err = s.client.revoke(opctx, secret.RefreshToken, row.ClientID); err != nil {
		_ = s.failOperation(ctx, row, "revoking")
		return err
	}
	err = s.db().WithContext(context.WithoutCancel(ctx)).Transaction(func(tx *gorm.DB) error {
		if err := lockOwner(tx, owner); err != nil {
			return err
		}
		won, err := transition(tx, row, row.Version+1, "revoking", map[string]any{"state": "disconnected", "secret": "", "operation_until": nil})
		if err != nil {
			return err
		}
		if !won {
			return ErrBusy
		}
		return tx.Where("owner = ?", owner).Delete(&attempt{}).Error
	})
	if errors.Is(err, ErrBusy) {
		return err
	}
	if err != nil {
		return errors.New("could not disconnect ChatGPT")
	}
	return nil
}
