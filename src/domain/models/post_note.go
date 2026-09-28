package models

import (
	"time"

	"github.com/uptrace/bun"
)

// PostNoteType is the closed vocabulary of note kinds. The set is
// expected to grow, so it is validated in Go against these consts rather than a
// DB CHECK constraint (mirroring how PostStatus is enforced in app code) — a new
// type is a code-only change, no migration.
type PostNoteType string

const (
	// PostNoteTypeDraftThesis is the bullet-point thesis the content-plan flow
	// captures instead of writing it into the post body. Pinned to
	// the top of a post's note list.
	PostNoteTypeDraftThesis PostNoteType = "draft_thesis"
	// PostNoteTypeImagePrompt is an image-generation prompt (e.g. a Nano Banana
	// prompt) produced by the post assistant. Stored as text only — it does
	// not trigger image generation (that is CON-105, a separate consumer).
	PostNoteTypeImagePrompt PostNoteType = "image_prompt"
	// PostNoteTypeNote is a free-form side note.
	PostNoteTypeNote PostNoteType = "note"
)

// Valid reports whether t is a known note type.
func (t PostNoteType) Valid() bool {
	switch t {
	case PostNoteTypeDraftThesis, PostNoteTypeImagePrompt, PostNoteTypeNote:
		return true
	default:
		return false
	}
}

// PostNoteOrigin records how a note came to exist, so the UI can distinguish
// AI-authored notes from hand-written ones. CreatedBy always points
// at a real user regardless of origin.
type PostNoteOrigin string

const (
	// PostNoteOriginManual is a note created by a user through the REST CRUD.
	PostNoteOriginManual PostNoteOrigin = "manual"
	// PostNoteOriginAssistant is a note created by the post assistant's
	// createNote tool.
	PostNoteOriginAssistant PostNoteOrigin = "assistant"
	// PostNoteOriginContentPlan is a draft_thesis note created by the
	// content-plan generation flow.
	PostNoteOriginContentPlan PostNoteOrigin = "content_plan"
)

// Valid reports whether o is a known origin.
func (o PostNoteOrigin) Valid() bool {
	switch o {
	case PostNoteOriginManual, PostNoteOriginAssistant, PostNoteOriginContentPlan:
		return true
	default:
		return false
	}
}

// PostNote is a small standalone record attached to a Post: a draft
// thesis, an image prompt, or a free-form note. Ancillary content lives here
// instead of in the post body.
type PostNote struct {
	bun.BaseModel `bun:"table:post_notes,alias:pn" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID        string         `bun:"id,pk"                                        json:"id"`
	PostID    string         `bun:"post_id,notnull"                              json:"post_id"`
	Type      PostNoteType   `bun:"type,notnull"                                 json:"type"`
	Title     string         `bun:"title,notnull,default:''"                     json:"title"`
	Body      string         `bun:"body,notnull"                                 json:"body"`
	Origin    PostNoteOrigin `bun:"origin,notnull,default:'manual'"              json:"origin"`
	CreatedBy string         `bun:"created_by,notnull"                           json:"created_by"`
	CreatedAt time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time      `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
