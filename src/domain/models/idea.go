package models

import (
	"time"

	"github.com/uptrace/bun"
)

// IdeaVerdict is the triage answer on an idea (CON-315). It is a stable, closed
// set, mirrored by a DB CHECK. A nil verdict means the idea is waiting in the
// inbox.
type IdeaVerdict string

const (
	IdeaVerdictYes   IdeaVerdict = "yes"
	IdeaVerdictLater IdeaVerdict = "later"
	IdeaVerdictNo    IdeaVerdict = "no"
)

// Valid reports whether v is a known verdict.
func (v IdeaVerdict) Valid() bool {
	switch v {
	case IdeaVerdictYes, IdeaVerdictLater, IdeaVerdictNo:
		return true
	default:
		return false
	}
}

// Idea is one entry in a workspace's shared Ideas backlog (CON-315). It is
// workspace-wide when CampaignID is nil and attached to that campaign otherwise;
// moving between the two keeps the row, its verdict, and its author.
//
// A "woken" idea (verdict later, RemindAt <= now) is still stored as later —
// the UI derives the woken state at read time and no job rewrites it.
type Idea struct {
	bun.BaseModel `bun:"table:ideas,alias:i" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks (CON-97)

	ID         string       `bun:"id,pk"                                        json:"id"`
	Title      string       `bun:"title,notnull"                                json:"title"`
	Note       string       `bun:"note,notnull,default:''"                      json:"note"`
	CampaignID *string      `bun:"campaign_id"                                  json:"campaign_id"`
	Verdict    *IdeaVerdict `bun:"verdict"                                      json:"verdict"      swaggertype:"string" enums:"yes,later,no" extensions:"x-nullable"`
	RemindAt   *time.Time   `bun:"remind_at"                                    json:"remind_at"`
	DecidedAt  *time.Time   `bun:"decided_at"                                   json:"decided_at"`
	DecidedBy  *string      `bun:"decided_by"                                   json:"decided_by"`
	// CreatedBy is stamped from the session on insert and never changes. It goes
	// null only when the author's user row is hard-deleted; CreatedByName is the
	// capture-time snapshot that keeps the author after that.
	CreatedBy     *string   `bun:"created_by"                                   json:"created_by"`
	CreatedByName string    `bun:"created_by_name,notnull"                      json:"created_by_name"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
