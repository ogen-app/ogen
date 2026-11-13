package models

import (
	"time"

	"github.com/uptrace/bun"
)

// AssistantMessage is one turn in a universal-assistant thread. A thread is a
// scope: the workspace, a campaign or a post. A user turn carries the screen
// the user was on (ViewContext, the raw versioned JSON the client sent), so a
// later turn can resolve "those" against what was selected then.
type AssistantMessage struct {
	bun.BaseModel `bun:"table:assistant_messages,alias:asm" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID        string             `bun:"id,pk"                   json:"id"`
	ScopeKind AssistantScopeKind `bun:"scope_kind,notnull"      json:"scope_kind"`
	ScopeID   string             `bun:"scope_id,notnull"        json:"scope_id"`
	Role      string             `bun:"role,notnull"            json:"role"`
	Content   string             `bun:"content,notnull"         json:"content"`
	// ViewContext is nil on model turns, and on user turns sent without one.
	ViewContext JSONMap   `bun:"view_context,type:jsonb"                       json:"view_context,omitempty"`
	CreatedBy   string    `bun:"created_by,notnull"                            json:"created_by,omitempty"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	// Offers are, on a model turn, the offers it made or answered, as they
	// stand now; filled when the thread is read, not stored.
	Offers []AssistantOfferState `bun:"-" json:"offers,omitempty"`
}
