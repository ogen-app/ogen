package models

import (
	"context"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// EditorOf is the member a write in ctx records as the last to change what
// it writes; ok is false for a job, whose write leaves the last editor as it
// was.
func EditorOf(ctx context.Context) (string, bool) {
	return logging.UserIDFrom(ctx)
}

// MarkEdited records ctx's member as the post's last editor, before a write
// of the whole row.
func (p *Post) MarkEdited(ctx context.Context) {
	if by, ok := EditorOf(ctx); ok {
		p.UpdatedBy = by
	}
}

// MarkEdited records ctx's member as the asset's last editor, before a
// write of the whole row.
func (a *Asset) MarkEdited(ctx context.Context) {
	if by, ok := EditorOf(ctx); ok {
		a.UpdatedBy = by
	}
}

// ChangedBy is a member who changed Count of the things a change counts.
type ChangedBy struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
}
