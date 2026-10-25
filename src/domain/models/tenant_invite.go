package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Tester invite lifecycle. "Expired" is not stored: a pending invite whose
// ExpiresAt has passed reads as expired.
const (
	TenantInvitePending  = "pending"
	TenantInviteRedeemed = "redeemed"
	TenantInviteRevoked  = "revoked"
	// TenantInviteExpired is the derived status the operator list filters on
	// and reports.
	TenantInviteExpired = "expired"
)

// Tester invite lifetime, in hours.
const (
	TenantInviteDefaultTTLHours = 14 * 24
	TenantInviteMinTTLHours     = 1
	TenantInviteMaxTTLHours     = 30 * 24
)

// TenantInviteTextMax bounds the personal message and the operator note.
const TenantInviteTextMax = 1000

// TenantInvite is an operator-issued, single-use capability to create a new
// complimentary workspace: never billed, on TierID, in the invite's groups.
// It is bound to Email. Only the token's hash is stored (TokenHash); the
// plaintext lives in the emailed link.
type TenantInvite struct {
	bun.BaseModel `bun:"table:tenant_invites,alias:ti" swaggerignore:"true"`

	ID        string  `bun:"id,pk"`
	Email     string  `bun:"email,notnull"`
	TokenHash string  `bun:"token_hash,notnull"`
	TierID    *string `bun:"tier_id"`
	Message   *string `bun:"message"`
	Note      *string `bun:"note"`
	// CreatedBy is the operator identity Harbor passes; the gRPC surface
	// authenticates with one shared token, so it cannot be derived.
	CreatedBy         string     `bun:"created_by,notnull"`
	Status            string     `bun:"status,notnull,default:'pending'"`
	TTLHours          int        `bun:"ttl_hours,notnull"`
	ExpiresAt         time.Time  `bun:"expires_at,notnull"`
	SentCount         int        `bun:"sent_count,notnull,default:1"`
	LastSentAt        time.Time  `bun:"last_sent_at,notnull"`
	RedeemedAt        *time.Time `bun:"redeemed_at"`
	RedeemedTenantID  *string    `bun:"redeemed_tenant_id"`
	RedeemedAccountID *string    `bun:"redeemed_account_id"`
	RevokedAt         *time.Time `bun:"revoked_at"`
	CreatedAt         time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt         time.Time  `bun:"updated_at,notnull,default:current_timestamp"`
}

// DerivedStatus is Status with expiry applied: a pending invite past its
// ExpiresAt reads as expired.
func (i *TenantInvite) DerivedStatus(now time.Time) string {
	if i.Status == TenantInvitePending && !now.Before(i.ExpiresAt) {
		return TenantInviteExpired
	}
	return i.Status
}

// TenantInviteGroup puts the workspace an invite creates into a tenant group.
type TenantInviteGroup struct {
	bun.BaseModel `bun:"table:tenant_invite_groups,alias:tig" swaggerignore:"true"`

	InviteID string `bun:"invite_id,pk"`
	GroupID  string `bun:"group_id,pk"`
}

// TenantInviteView is an invite with the names and delivery state the
// operator console shows beside it.
type TenantInviteView struct {
	TenantInvite

	TierName        string
	TierPurchasable bool
	Groups          []TenantGroup
	// EmailStatus is the latest send of the invite email: its last delivery
	// event (delivered, opened, bounced, ...) or, without one, the send
	// outcome (sent, skipped_suppressed, failed, ...). Empty before the
	// worker ran.
	EmailStatus      string
	EmailLastEventAt *time.Time
	// RedeemedTenantName and RedeemedTenantComplimentary describe the
	// workspace the invite became.
	RedeemedTenantName          string
	RedeemedTenantComplimentary bool
}
