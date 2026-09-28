package models

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

type Session struct {
	bun.BaseModel `bun:"table:sessions,alias:s" swaggerignore:"true"`

	ID string `bun:"id,pk" json:"id"`
	// AccountID is the login identity this session authenticates. The
	// session belongs to the account; UserID + TenantID are the active membership
	// / default workspace it currently resolves to.
	AccountID string    `bun:"account_id,notnull"                           json:"account_id"`
	UserID    string    `bun:"user_id,notnull"                              json:"user_id"`
	TenantID  string    `bun:"tenant_id,notnull"                            json:"tenant_id"`
	ExpiresAt time.Time `bun:"expires_at,notnull"                           json:"expires_at"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// NewSessionToken generates a cryptographically random 32-byte URL-safe token.
func NewSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
