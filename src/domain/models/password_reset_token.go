package models

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// PasswordResetTokenTTL is the single-use reset token's lifetime. The
// UI's success copy states "It expires in an hour" — change one, change both.
const PasswordResetTokenTTL = time.Hour

// PasswordResetToken is a single-use capability to set a new password without
// logging in. Only the token's hash is persisted (TokenHash); the
// plaintext lives only in the emailed link, so a DB leak can't hand over live
// reset links. ConsumedAt NULL means unspent — the confirm path flips it
// atomically so a double-submitted form can't spend it twice.
type PasswordResetToken struct {
	bun.BaseModel `bun:"table:password_reset_tokens,alias:prt" swaggerignore:"true"`

	ID         string     `bun:"id,pk"                                        json:"id"`
	UserID     string     `bun:"user_id,notnull"                              json:"user_id"`
	TenantID   string     `bun:"tenant_id,notnull"                            json:"tenant_id"`
	TokenHash  string     `bun:"token_hash,notnull"                           json:"-"`
	ExpiresAt  time.Time  `bun:"expires_at,notnull"                           json:"expires_at"`
	ConsumedAt *time.Time `bun:"consumed_at"                                  json:"consumed_at,omitempty"`
	CreatedAt  time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// NewResetToken returns a cryptographically random, URL-safe reset token (32
// bytes of entropy) together with its hash for storage. The plaintext is
// returned once — for the emailed link — and never persisted.
func NewResetToken() (token, hash string, err error) {
	return newHashedToken("reset token")
}

// HashResetToken returns the hex-encoded sha256 of a reset token.
func HashResetToken(token string) string {
	return hashToken(token)
}

// newHashedToken returns 32 random bytes as a URL-safe string plus its storage
// hash. kind names the token in the error.
func newHashedToken(kind string) (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate %s: %w", kind, err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

// hashToken is the hex-encoded sha256 of an opaque token. The tokens are
// high-entropy random, so a plain (unsalted) hash is sufficient — unlike a
// low-entropy password, there is nothing to brute-force from the hash.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// opaqueTokenLen is the encoded length of 32 random bytes in unpadded base64url.
const opaqueTokenLen = 43

// wellFormedToken reports whether s could have come from newHashedToken, so a
// garbage value is rejected without a database lookup.
func wellFormedToken(s string) bool {
	if len(s) != opaqueTokenLen {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}
