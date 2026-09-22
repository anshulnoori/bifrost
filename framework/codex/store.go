package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/encrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Connection contains no exportable credentials. All queries are owner-scoped;
// callers must derive owner from authenticated gateway identity, never a header
// naming an upstream account. A new login creates a new connection ID.
type Connection struct {
	ID             string    `gorm:"primaryKey;size:36" json:"id"`
	Owner          string    `gorm:"uniqueIndex;not null" json:"-"`
	State          string    `json:"state"`
	Secret         string    `gorm:"type:text" json:"-"`
	Version        uint64    `json:"-"`
	ExpiresAt      time.Time `json:"expires_at"`
	NextPoll       time.Time `json:"-"`
	OperationUntil time.Time `json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (Connection) TableName() string { return "codex_connections" }

// envelope binds encrypted content to its row and owner. Copying ciphertext
// between rows must not transfer credentials even inside the same deployment.
type envelope struct {
	ID       string        `json:"id"`
	Owner    string        `json:"owner"`
	Device   device        `json:"device"`
	Interval time.Duration `json:"interval"`
	Tokens   tokens        `json:"tokens"`
}

// Store obtains the current config-store pool for every operation, so pool
// rotation does not leave it holding a closed connection. Migration is separate
// and runs under the config-store migration lock.
type Store struct {
	db     func() *gorm.DB
	client *Client
}

func NewStore(db func() *gorm.DB) (*Store, error) {
	if !encrypt.IsEnabled() {
		return nil, errors.New("codex requires BIFROST_ENCRYPTION_KEY")
	}
	if db == nil {
		return nil, errors.New("codex requires a database config store")
	}
	return &Store{db: db, client: NewClient()}, nil
}

func seal(e envelope) (string, error) {
	if !encrypt.IsEnabled() {
		return "", encrypt.ErrEncryptionKeyNotInitialized
	}
	b, err := json.Marshal(e)
	if err != nil {
		return "", errors.New("could not encode codex credentials")
	}
	s, err := encrypt.Encrypt(string(b))
	if err != nil {
		return "", errors.New("could not encrypt codex credentials")
	}
	return s, nil
}

func unseal(row Connection) (envelope, error) {
	var e envelope
	s, err := encrypt.Decrypt(row.Secret)
	if err != nil || json.Unmarshal([]byte(s), &e) != nil || e.ID != row.ID || e.Owner != row.Owner {
		return envelope{}, errors.New("could not decrypt codex credentials")
	}
	return e, nil
}

type Login struct {
	Connection
	VerificationURL string `json:"verification_url"`
	UserCode        string `json:"user_code"`
	IntervalSeconds int    `json:"interval_seconds"`
}

func (s *Store) Start(ctx context.Context, owner string) (*Login, error) {
	if owner == "" {
		return nil, ErrNotFound
	}
	d, interval, err := s.client.start(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := Connection{ID: uuid.NewString(), Owner: owner, State: "pending", ExpiresAt: now.Add(15 * time.Minute), NextPoll: now.Add(interval)}
	row.Secret, err = seal(envelope{ID: row.ID, Owner: owner, Device: d, Interval: interval})
	if err != nil {
		return nil, err
	}
	// Reconnect replaces the owner's previous connection atomically. The new
	// random ID fences exchanges still running against the old connection.
	if s.db().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if keyID, providerOwned := strings.CutPrefix(owner, "provider:codex:"); providerOwned {
			// Use the same lock order as config-store deletion. A login started
			// before account deletion cannot resurrect its credential record.
			var provider tables.TableProvider
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("name = ?", "codex").First(&provider).Error; err != nil {
				return err
			}
			var key tables.TableKey
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("provider_id = ? AND key_id = ?", provider.ID, keyID).First(&key).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("owner = ?", owner).Delete(&Connection{}).Error; err != nil {
			return err
		}
		return tx.Create(&row).Error
	}) != nil {
		return nil, errors.New("could not persist codex authorization")
	}
	return &Login{Connection: row, VerificationURL: Issuer + "/codex/device", UserCode: d.Code, IntervalSeconds: int(interval / time.Second)}, nil
}

// Current identifies the owner's single current connection without exposing secrets.
func (s *Store) Current(ctx context.Context, owner string) (Connection, error) {
	if owner == "" {
		return Connection{}, ErrNotFound
	}
	var row Connection
	err := s.db().WithContext(ctx).Select("id").Where("owner = ?", owner).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, ErrNotFound
	}
	if err != nil {
		return row, errors.New("codex credential store unavailable")
	}
	return s.Status(ctx, owner, row.ID)
}

func (s *Store) load(ctx context.Context, owner, id string) (Connection, error) {
	var row Connection
	if owner == "" || id == "" {
		return row, ErrNotFound
	}
	err := s.db().WithContext(ctx).Where("id = ? AND owner = ?", id, owner).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return row, ErrNotFound
	}
	if err != nil {
		return row, errors.New("codex credential store unavailable")
	}
	return row, nil
}

// change is a durable compare-and-swap, not a process mutex or an expiring
// distributed lock. A dead refresh owner is never replaced: its refresh token
// may already have rotated upstream. A stale operation requires reconnect.
func (s *Store) change(ctx context.Context, row Connection, values map[string]any) error {
	values["version"] = row.Version + 1
	result := s.db().WithContext(ctx).Model(&Connection{}).
		Where("id = ? AND owner = ? AND version = ? AND state = ?", row.ID, row.Owner, row.Version, row.State).Updates(values)
	if result.Error != nil {
		return errors.New("codex credential store unavailable")
	}
	if result.RowsAffected != 1 {
		return ErrBusy
	}
	return nil
}

func (s *Store) Status(ctx context.Context, owner, id string) (Connection, error) {
	row, err := s.load(ctx, owner, id)
	if err != nil {
		return row, err
	}
	if (row.State == "pending" && !row.ExpiresAt.After(time.Now())) ||
		((row.State == "polling" || row.State == "refreshing") && !row.OperationUntil.After(time.Now())) {
		state := "reconnect_required"
		if row.State == "pending" {
			state = "expired"
		}
		if err := s.change(ctx, row, map[string]any{"state": state, "secret": ""}); err != nil {
			return row, err
		}
		row.State, row.Secret = state, ""
		row.Version++
	}
	return row, nil
}

// Disconnect also cancels pending login. In-flight exchanges cannot resurrect
// the row: deletion invalidates their CAS. It does not claim upstream revocation.
func (s *Store) Disconnect(ctx context.Context, owner, id string) error {
	if owner == "" || id == "" {
		return ErrNotFound
	}
	result := s.db().WithContext(ctx).Where("id = ? AND owner = ?", id, owner).Delete(&Connection{})
	if result.Error != nil {
		return errors.New("could not disconnect codex")
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) Poll(ctx context.Context, owner, id string) (Connection, error) {
	row, err := s.Status(ctx, owner, id)
	if err != nil {
		return row, err
	}
	if row.State == "connected" {
		return row, nil
	}
	if row.State == "polling" {
		return row, ErrBusy
	}
	if row.State != "pending" {
		return row, ErrReconnect
	}
	if time.Now().Before(row.NextPoll) {
		return row, ErrPending
	}
	e, err := unseal(row)
	if err != nil {
		return row, err
	}
	if err = s.change(ctx, row, map[string]any{"state": "polling", "operation_until": time.Now().Add(time.Minute)}); err != nil {
		return row, err
	}
	row.State = "polling"
	row.Version++
	t, callErr := s.client.poll(ctx, e.Device)
	// Persist results even if the HTTP caller disconnected during token exchange.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if errors.Is(callErr, ErrPending) {
		err = s.change(saveCtx, row, map[string]any{"state": "pending", "next_poll": time.Now().Add(e.Interval)})
		if err != nil {
			return row, err
		}
		row.State = "pending"
		return row, ErrPending
	}
	if callErr != nil {
		_ = s.change(saveCtx, row, map[string]any{"state": "reconnect_required", "secret": ""})
		return row, ErrReconnect
	}
	if err = s.saveTokens(saveCtx, row, t); err != nil {
		return row, err
	}
	return s.Status(saveCtx, owner, id)
}

func (s *Store) saveTokens(ctx context.Context, row Connection, t tokens) error {
	secret, err := seal(envelope{ID: row.ID, Owner: row.Owner, Tokens: t})
	if err != nil {
		return err
	}
	return s.change(ctx, row, map[string]any{"state": "connected", "secret": secret, "expires_at": t.ExpiresAt})
}

// Credential returns secrets only to the provider resolver, never to handlers.
// No token cache is retained between calls, so replicas observe disconnects.
func (s *Store) Credential(ctx context.Context, owner, id string) (access, account string, err error) {
	// A follower waits for the database winner instead of failing a valid
	// inference request. It never takes over or reuses an uncertain refresh.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		access, account, err = s.credential(ctx, owner, id)
		if !errors.Is(err, ErrBusy) {
			return access, account, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", "", ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) credential(ctx context.Context, owner, id string) (access, account string, err error) {
	row, err := s.Status(ctx, owner, id)
	if err != nil {
		return "", "", err
	}
	if row.State == "refreshing" {
		return "", "", ErrBusy
	}
	if row.State != "connected" {
		return "", "", ErrReconnect
	}
	e, err := unseal(row)
	if err != nil {
		return "", "", err
	}
	if row.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return e.Tokens.Access, e.Tokens.Account, nil
	}
	if err = s.change(ctx, row, map[string]any{"state": "refreshing", "operation_until": time.Now().Add(time.Minute)}); err != nil {
		return "", "", err
	}
	row.State = "refreshing"
	row.Version++
	t, callErr := s.client.refresh(ctx, e.Tokens)
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if callErr != nil {
		_ = s.change(saveCtx, row, map[string]any{"state": "reconnect_required", "secret": ""})
		return "", "", ErrReconnect
	}
	if err = s.saveTokens(saveCtx, row, t); err != nil {
		return "", "", err
	}
	return t.Access, t.Account, nil
}
