package models

import (
	"time"

	"github.com/uptrace/bun"
)

// AssetImage stores one page image mirrored into object storage for a
// URL-scraped Asset. Unlike asset_files (one-to-one, for a PDF's
// original blob), an asset has many images, so this is a separate one-to-many
// table keyed by (asset_id, idx). The scraped Markdown's image links are
// rewritten to point at S3Key's public URL.
type AssetImage struct {
	bun.BaseModel `bun:"table:asset_images,alias:aimg" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID        string    `bun:"id,pk"                                        json:"id"`
	AssetID   string    `bun:"asset_id,notnull"                             json:"asset_id"`
	Idx       int       `bun:"idx,notnull"                                  json:"idx"`
	SourceURL string    `bun:"source_url,notnull"                           json:"source_url"`
	S3Key     string    `bun:"s3_key,notnull"                               json:"s3_key"`
	MimeType  string    `bun:"mime_type,notnull,default:''"                 json:"mime_type"`
	SizeBytes int64     `bun:"size_bytes,notnull,default:0"                 json:"size_bytes"`
	Alt       *string   `bun:"alt"                                          json:"alt,omitempty"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	// URL is a transient public URL rendered from S3Key by the handler layer
	// before serialization. Not persisted.
	URL *string `bun:"-" json:"url,omitempty"`
}
