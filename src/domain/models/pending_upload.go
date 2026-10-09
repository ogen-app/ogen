package models

import (
	"time"

	"github.com/uptrace/bun"
)

// PendingUpload is a presigned direct-to-storage upload that has not been
// finalized. Finalize consumes the row; one left past ExpiresAt is an
// abandoned upload whose object the sweep deletes. PostID is nil once the
// post is deleted: the object still needs sweeping.
type PendingUpload struct {
	bun.BaseModel `bun:"table:pending_uploads,alias:pu" swaggerignore:"true"`
	TenantScoped

	ID        string    `bun:"id,pk"`
	PostID    *string   `bun:"post_id"`
	S3Key     string    `bun:"s3_key,notnull"`
	SizeBytes int64     `bun:"size_bytes,notnull"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp"`
	ExpiresAt time.Time `bun:"expires_at,notnull"`
}
