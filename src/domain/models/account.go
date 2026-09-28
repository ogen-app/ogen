package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Account is the login identity. It holds the email + password and
// spans workspaces: one account can hold a membership (a users row) in many
// tenants and switch between them without re-authenticating.
//
// Split out of users in CON-147 PR1 — before that a users row WAS the identity
// (it carried password_hash and a globally unique email). Like users and
// sessions, Account is deliberately NOT TenantScoped: authentication resolves an
// account before any tenant is known.
type Account struct {
	bun.BaseModel `bun:"table:accounts,alias:a" swaggerignore:"true"`

	ID           string    `bun:"id,pk"                                        json:"id"`
	Email        string    `bun:"email,notnull,unique"                         json:"email"`
	PasswordHash string    `bun:"password_hash,notnull"                        json:"-"`
	Name         string    `bun:"name,notnull"                                 json:"name"`
	CreatedAt    time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt    time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
