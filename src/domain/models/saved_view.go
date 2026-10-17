package models

import (
	"time"

	"github.com/uptrace/bun"
)

// SavedView is a named address of a filterable list (a smart group): the
// list's query string in its own parameter names, canonical, without paging.
// Surface names the list. It stores a filter, never a list of rows, so a view
// keeps working, and matching nothing for that value, after an asset or
// campaign it names is gone.
//
// A private view is its creator's alone; a shared one every member's.
// CreatedBy goes nil when the creator leaves the workspace, which deletes
// their private views and leaves their shared ones to the owners.
type SavedView struct {
	bun.BaseModel `bun:"table:saved_views,alias:sv" swaggerignore:"true"`
	TenantScoped

	ID      string `bun:"id,pk"           json:"id"`
	Surface string `bun:"surface,notnull" json:"surface"`
	Name    string `bun:"name,notnull"    json:"name"`
	// Query is the canonical query string: parameters by name, values sorted
	// and comma-joined.
	Query string `bun:"query,notnull" json:"query"`
	// QueryVersion is the surface's query format Query was written in.
	QueryVersion int       `bun:"query_version,notnull"                        json:"-"`
	Shared       bool      `bun:"shared,notnull"                               json:"shared"`
	CreatedBy    *string   `bun:"created_by"                                   json:"created_by" extensions:"x-nullable"`
	CreatedAt    time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt    time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// Valid is false when the stored query no longer parses for its surface.
	Valid bool `bun:"-" json:"valid"`
	// CanEdit is whether the caller may rename, change, share or delete it.
	CanEdit bool `bun:"-" json:"can_edit"`
	// Counts holds the counts the list asked for, by name; absent when it
	// asked for none or the view is not valid.
	Counts map[string]int `bun:"-" json:"counts,omitempty"`
}
