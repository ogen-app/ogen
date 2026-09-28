package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Role is a user's authority within their workspace (tenant). CON-26 introduces
// a minimal two-tier model — owner > member — which CON-147 keeps unchanged (no
// `admin` or `viewer` tier). The workspace creator is owner; a workspace always
// keeps at least one owner.
type Role = string

const (
	// RoleOwner may invite/remove teammates, change roles, and manage the
	// workspace; RoleMember is a full content collaborator that cannot manage
	// the team. See CON-26 §7 for the enforced capability matrix.
	RoleOwner  Role = "owner"
	RoleMember Role = "member"
)

// IsValidRole reports whether r is one of the roles CON-26 recognises.
func IsValidRole(r string) bool { return r == RoleOwner || r == RoleMember }

// User is a per-(account, workspace) MEMBERSHIP — one row per account per
// tenant, carrying that account's role in that tenant. The credential lives on
// Account (AccountID); this row is what `user_id` FKs (sessions, authorship,
// activity) resolve to: "this account, in this workspace." Email/Name are
// denormalised from the account for existing responses; email is not unique
// (one account can be a member of several workspaces).
type User struct {
	bun.BaseModel `bun:"table:users,alias:u" swaggerignore:"true"`

	ID string `bun:"id,pk"             json:"id"`
	// AccountID is the login identity this membership belongs to.
	AccountID string `bun:"account_id,notnull" json:"-"`
	TenantID  string `bun:"tenant_id,notnull"  json:"-"`
	Name      string `bun:"name,notnull"       json:"name"`
	Email     string `bun:"email,notnull"      json:"email"`
	// Role is the account's authority in this tenant. Users that predate
	// invitations are 'owner' (each was the sole member of its tenant).
	Role      string    `bun:"role,notnull,default:'member'" json:"role"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// Tenant is loaded only on demand (e.g. GET /api/current_user via
	// UserRepository.GetByIDWithTenant) and omitted otherwise, so ordinary
	// user responses stay unchanged. CON-97.
	Tenant *Tenant `bun:"rel:belongs-to,join:tenant_id=id" json:"tenant,omitempty"`
}
