package models

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// PluginClientFigma is the only plugin client today.
const PluginClientFigma = "figma"

// PluginTokenPrefix marks a plugin token so a leaked one is recognisable to
// people and secret scanners.
const PluginTokenPrefix = "ogp_"

// PluginToken is a long-lived, revocable credential a design-tool plugin uses
// to act as one member in one workspace. Only the token's hash is persisted.
// Like Session it is not TenantScoped: the auth middleware finds it by hash
// before any tenant is known, so repository methods scope by tenant_id
// explicitly.
type PluginToken struct {
	bun.BaseModel `bun:"table:plugin_tokens,alias:pt" swaggerignore:"true"`

	ID         string     `bun:"id,pk"                                        json:"id"`
	TenantID   string     `bun:"tenant_id,notnull"                            json:"-"`
	AccountID  string     `bun:"account_id,notnull"                           json:"-"`
	UserID     string     `bun:"user_id,notnull"                              json:"user_id"`
	Client     string     `bun:"client,notnull"                               json:"client"`
	Label      string     `bun:"label,notnull"                                json:"label"`
	TokenHash  string     `bun:"token_hash,notnull"                           json:"-"`
	LastUsedAt *time.Time `bun:"last_used_at"                                 json:"last_used_at"`
	CreatedAt  time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	RevokedAt  *time.Time `bun:"revoked_at"                                   json:"-"`
}

// NewPluginToken returns a fresh plugin token (the prefix plus 32 random
// bytes, base64url) and the hash to store for it.
func NewPluginToken() (token, hash string, err error) {
	secret, err := newSecret()
	if err != nil {
		return "", "", fmt.Errorf("generate plugin token: %w", err)
	}
	token = PluginTokenPrefix + secret
	return token, HashPluginSecret(token), nil
}

// LooksLikePluginToken reports whether s has the plugin token shape, so the
// middleware can reject garbage without a database round-trip.
func LooksLikePluginToken(s string) bool {
	return strings.HasPrefix(s, PluginTokenPrefix) && len(s) > len(PluginTokenPrefix)
}

// HashPluginSecret returns the hex sha256 of a plugin token or pairing key.
// Both are 32 random bytes, so an unsalted hash leaves nothing to brute-force.
func HashPluginSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
