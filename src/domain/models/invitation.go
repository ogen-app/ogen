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

// InvitationTTL is how long a workspace invitation stays acceptable.
// Longer than a password reset (an hour) because accepting an invite is a
// slower, more deliberate human action — the invitee often isn't watching their
// inbox. Expiry is derived from ExpiresAt, not a stored status.
const InvitationTTL = 7 * 24 * time.Hour

// InvitationStatus is the lifecycle of an invite. Expiry is NOT a status — an
// invite is "expired" iff it is still pending and ExpiresAt is in the past — so
// no sweeper is needed for correctness.
type InvitationStatus = string

const (
	InvitationPending  InvitationStatus = "pending"
	InvitationAccepted InvitationStatus = "accepted"
	InvitationRevoked  InvitationStatus = "revoked"
)

// Invitation is a single-use capability to join a workspace (tenant) with a
// preset role. Only the token's hash is persisted (TokenHash); the
// plaintext lives only in the emailed link, so a DB leak can't hand over live
// invites. Accepting it creates a new users row in TenantID — Ogen stays
// one-user-one-tenant (the multi-workspace account split is CON-147). The
// partial unique index on (tenant_id, email) WHERE status='pending' keeps at
// most one live invite per address per workspace.
type Invitation struct {
	bun.BaseModel `bun:"table:users_invitations,alias:ui" swaggerignore:"true"`

	ID         string     `bun:"id,pk"                                        json:"id"`
	TenantID   string     `bun:"tenant_id,notnull"                            json:"tenant_id"`
	Email      string     `bun:"email,notnull"                                json:"email"`
	Role       string     `bun:"role,notnull"                                 json:"role"`
	TokenHash  string     `bun:"token_hash,notnull"                           json:"-"`
	InvitedBy  string     `bun:"invited_by,notnull"                           json:"invited_by"`
	Status     string     `bun:"status,notnull,default:'pending'"             json:"status"`
	ExpiresAt  time.Time  `bun:"expires_at,notnull"                           json:"expires_at"`
	AcceptedAt *time.Time `bun:"accepted_at"                                  json:"accepted_at,omitempty"`
	CreatedAt  time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// NewInvitationToken returns a cryptographically random, URL-safe invitation
// token (32 bytes of entropy) together with its hash for storage. The plaintext
// is returned once — for the emailed link — and never persisted. Mirrors
// NewResetToken.
func NewInvitationToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate invitation token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashInvitationToken(token), nil
}

// HashInvitationToken returns the hex-encoded sha256 of an invitation token. The
// token is high-entropy random, so a plain (unsalted) hash is sufficient — there
// is nothing to brute-force from the hash.
func HashInvitationToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
