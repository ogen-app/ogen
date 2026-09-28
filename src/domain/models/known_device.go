package models

import (
	"time"

	"github.com/uptrace/bun"
)

// KnownDeviceCookieMaxAge is how long the device cookie lives: 400 days, the
// most browsers allow. It is re-issued on every login, so an active browser
// never ages out.
const KnownDeviceCookieMaxAge = 400 * 24 * time.Hour

// KnownDevice is a browser an account has signed in from. The browser holds a
// random token in a long-lived cookie; only its hash is stored. Rows are per
// account, so one shared browser is a separate device for each account.
type KnownDevice struct {
	bun.BaseModel `bun:"table:account_known_devices,alias:akd" swaggerignore:"true"`

	ID          string    `bun:"id,pk"                                           json:"id"`
	AccountID   string    `bun:"account_id,notnull"                              json:"account_id"`
	DeviceHash  string    `bun:"device_hash,notnull"                             json:"-"`
	DeviceLabel string    `bun:"device_label,notnull"                            json:"device_label"`
	UserAgent   string    `bun:"user_agent,notnull"                              json:"user_agent"`
	LastIP      string    `bun:"last_ip,notnull"                                 json:"last_ip"`
	FirstSeenAt time.Time `bun:"first_seen_at,notnull,default:current_timestamp" json:"first_seen_at"`
	LastSeenAt  time.Time `bun:"last_seen_at,notnull,default:current_timestamp"  json:"last_seen_at"`
}

// NewDeviceToken returns a fresh device-cookie value and its storage hash.
func NewDeviceToken() (token, hash string, err error) {
	return newHashedToken("device token")
}

// HashDeviceToken returns the storage hash of a device-cookie value.
func HashDeviceToken(token string) string {
	return hashToken(token)
}

// ValidDeviceToken reports whether a cookie value has the shape NewDeviceToken
// produces. Anything else is treated as no cookie and replaced.
func ValidDeviceToken(token string) bool {
	return wellFormedToken(token)
}
