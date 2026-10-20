package models

import (
	"time"

	"github.com/uptrace/bun"
)

// IdentityProviderGoogle is the only external sign-in provider so far.
const IdentityProviderGoogle = "google"

// AccountIdentity links an account to an external sign-in provider. It is keyed
// by the provider's stable subject, not by email, so either side can change its
// address without breaking sign-in. An account has at most one identity per
// provider. Like Account, it is not tenant-scoped.
type AccountIdentity struct {
	bun.BaseModel `bun:"table:account_identities,alias:ai" swaggerignore:"true"`

	ID         string     `bun:"id,pk"                                       json:"-"`
	AccountID  string     `bun:"account_id,notnull"                          json:"-"`
	Provider   string     `bun:"provider,notnull"                            json:"provider"`
	Subject    string     `bun:"subject,notnull"                             json:"-"`
	Email      string     `bun:"email,notnull"                               json:"email"`
	LinkedAt   time.Time  `bun:"linked_at,notnull,default:current_timestamp" json:"linked_at"`
	LastUsedAt *time.Time `bun:"last_used_at"                                json:"last_used_at,omitempty"`
}
