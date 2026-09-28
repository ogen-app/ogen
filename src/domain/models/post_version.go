package models

import (
	"time"

	"github.com/uptrace/bun"
)

type PostVersion struct {
	bun.BaseModel `bun:"table:post_versions,alias:pv" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID            string    `bun:"id,pk"                                        json:"id"`
	PostID        string    `bun:"post_id,notnull"                              json:"post_id"`
	VersionNumber int       `bun:"version_number,notnull"                       json:"version_number"`
	Content       string    `bun:"content,notnull"                              json:"content"`
	Note          string    `bun:"note,notnull"                                 json:"note"`
	Creator       string    `bun:"creator,notnull"                              json:"creator"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// Creator values for a PostVersion (matched by the post_versions_creator_check
// constraint). "system" marks the CON-251 "what went out" snapshots, distinct
// from human ("user") and assistant content edits.
const (
	PostVersionCreatorUser      = "user"
	PostVersionCreatorAssistant = "assistant"
	PostVersionCreatorSystem    = "system"
)

// Notes carried by the CON-251 system snapshots: the content is captured once
// when it is submitted to the publisher and again when publication is
// confirmed, so a published post keeps a durable record of what actually left
// Ogen rather than an assumption.
const (
	PostVersionNoteSubmitted = "Submitted for scheduled publishing"
	PostVersionNotePublished = "Published"
)

// IsSystemSnapshot reports whether v is a CON-251 system-authored "what went
// out" record, as opposed to a user/assistant content edit. Snapshot creation
// dedups against it (together with a note + content match) so the record is
// written once per submit/publish and never suppressed by a matching-content
// user edit sitting at the head.
func (v *PostVersion) IsSystemSnapshot() bool {
	return v != nil && v.Creator == PostVersionCreatorSystem
}
