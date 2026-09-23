package tables

import "time"

// OIDCLogin contains no plaintext authorization code, browser secret, or PKCE
// verifier. Secret is an authenticated encrypted envelope; callbacks consume
// the row atomically before exchanging the code, including across replicas.
type OIDCLogin struct {
	StateHash   string    `gorm:"primaryKey;type:varchar(64)"`
	BrowserHash string    `gorm:"type:varchar(64);not null"`
	Secret      string    `gorm:"type:text;not null"`
	ExpiresAt   time.Time `gorm:"index;not null"`
}

func (OIDCLogin) TableName() string { return "dashboard_oidc_logins" }
