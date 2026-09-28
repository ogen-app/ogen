package models

import (
	"time"

	"github.com/uptrace/bun"
)

// LoginAlertTokenTTL is how long the "This wasn't me" link in a new-device
// alert stays usable. The email copy states 24 hours — change one, change both.
const LoginAlertTokenTTL = 24 * time.Hour

// Login alert token states reported by the preview endpoint.
const (
	LoginAlertPending = "pending"
	LoginAlertUsed    = "used"
	LoginAlertExpired = "expired"
)

// LoginAlertToken backs one new-device alert email. It snapshots what the email
// showed (IP, device, location, time) so the secure-account page can show the
// same thing, and it is the single-use capability to secure the account. Only
// the hash is stored; the plaintext lives in the emailed link.
type LoginAlertToken struct {
	bun.BaseModel `bun:"table:login_alert_tokens,alias:lat" swaggerignore:"true"`

	ID          string     `bun:"id,pk"                                        json:"id"`
	AccountID   string     `bun:"account_id,notnull"                           json:"account_id"`
	DeviceID    *string    `bun:"device_id"                                    json:"device_id,omitempty"`
	UserID      string     `bun:"user_id,notnull"                              json:"user_id"`
	TenantID    string     `bun:"tenant_id,notnull"                            json:"tenant_id"`
	TokenHash   string     `bun:"token_hash,notnull"                           json:"-"`
	IP          string     `bun:"ip,notnull"                                   json:"ip"`
	DeviceLabel string     `bun:"device_label,notnull"                         json:"device_label"`
	Location    string     `bun:"location,notnull"                             json:"location"`
	LoginAt     time.Time  `bun:"login_at,notnull"                             json:"login_at"`
	ExpiresAt   time.Time  `bun:"expires_at,notnull"                           json:"expires_at"`
	ConsumedAt  *time.Time `bun:"consumed_at"                                  json:"consumed_at,omitempty"`
	CreatedAt   time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// Status reports whether the token can still be used at now.
func (t *LoginAlertToken) Status(now time.Time) string {
	switch {
	case t.ConsumedAt != nil:
		return LoginAlertUsed
	case !now.Before(t.ExpiresAt):
		return LoginAlertExpired
	default:
		return LoginAlertPending
	}
}

// NewLoginAlertToken returns a fresh alert token and its storage hash.
func NewLoginAlertToken() (token, hash string, err error) {
	return newHashedToken("login alert token")
}

// HashLoginAlertToken returns the storage hash of an alert token.
func HashLoginAlertToken(token string) string {
	return hashToken(token)
}

// ValidLoginAlertToken reports whether a link token has the shape
// NewLoginAlertToken produces, so malformed input is rejected before any query.
func ValidLoginAlertToken(token string) bool {
	return wellFormedToken(token)
}
