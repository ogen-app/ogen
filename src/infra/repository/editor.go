package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// setEditor records ctx's member as the last editor of the rows q changes,
// for an update that sets its columns; a job's update leaves it as it was.
func setEditor(ctx context.Context, q *bun.UpdateQuery) *bun.UpdateQuery {
	if by, ok := models.EditorOf(ctx); ok {
		q = q.Set("updated_by = ?", by)
	}
	return q
}

// editedColumns is columns plus updated_by when ctx's member makes the
// write, for an update of a model's named columns.
func editedColumns(ctx context.Context, columns ...string) []string {
	if _, ok := models.EditorOf(ctx); ok {
		return append(columns, "updated_by")
	}
	return columns
}
