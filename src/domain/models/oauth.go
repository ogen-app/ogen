package models

import (
	"time"

	"github.com/uptrace/bun"
)

// What a provider sign-in was started for.
const (
	OAuthIntentSignin = "signin"
	OAuthIntentLink   = "link"
	OAuthIntentInvite = "invite"
)

// OAuthStateTTL bounds the time between starting a sign-in and the provider
// redirecting back.
const OAuthStateTTL = 10 * time.Minute

// OAuthPendingSignupTTL bounds the time a verified newcomer has to name their
// workspace.
const OAuthPendingSignupTTL = 15 * time.Minute

// OAuthState is one started provider sign-in. Only the hash of the state is
// stored; the raw value travels in the browser cookie and the provider
// redirect, and the callback spends the row once.
type OAuthState struct {
	bun.BaseModel `bun:"table:oauth_states,alias:os" swaggerignore:"true"`

	StateHash    string     `bun:"state_hash,pk"`
	Provider     string     `bun:"provider,notnull"`
	Intent       string     `bun:"intent,notnull"`
	Nonce        string     `bun:"nonce,notnull"`
	PKCEVerifier string     `bun:"pkce_verifier,notnull"`
	ReturnTo     string     `bun:"return_to,notnull"`
	AccountID    string     `bun:"account_id,nullzero"`
	InviteHash   string     `bun:"invite_hash,nullzero"`
	ExpiresAt    time.Time  `bun:"expires_at,notnull"`
	ConsumedAt   *time.Time `bun:"consumed_at"`
	CreatedAt    time.Time  `bun:"created_at,notnull,default:current_timestamp"`
}

// OAuthPendingSignup holds a verified provider identity that has no account
// yet, until the newcomer names their workspace. Only the token hash is stored.
type OAuthPendingSignup struct {
	bun.BaseModel `bun:"table:oauth_pending_signups,alias:ops" swaggerignore:"true"`

	TokenHash  string     `bun:"token_hash,pk"`
	Provider   string     `bun:"provider,notnull"`
	Subject    string     `bun:"subject,notnull"`
	Email      string     `bun:"email,notnull"`
	Name       string     `bun:"name,notnull"`
	ReturnTo   string     `bun:"return_to,notnull"`
	ExpiresAt  time.Time  `bun:"expires_at,notnull"`
	ConsumedAt *time.Time `bun:"consumed_at"`
	CreatedAt  time.Time  `bun:"created_at,notnull,default:current_timestamp"`
}

// NewOAuthToken returns a fresh opaque token (state or pending-signup) and its
// storage hash.
func NewOAuthToken() (token, hash string, err error) {
	return newHashedToken("oauth token")
}

// HashOAuthToken returns the storage hash of an opaque OAuth token.
func HashOAuthToken(token string) string {
	return hashToken(token)
}

// ValidOAuthToken reports whether a token has the shape NewOAuthToken produces,
// so malformed input is rejected before any query.
func ValidOAuthToken(token string) bool {
	return wellFormedToken(token)
}
