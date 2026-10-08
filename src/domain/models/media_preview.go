package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Media preview sources: what a preview copy was rendered from.
const (
	MediaPreviewSourceImage  = "image"
	MediaPreviewSourcePoster = "poster"
)

// MediaPreview is a stored JPEG/PNG copy of an image or video poster, scaled
// to fit MaxLongEdge, for clients that can't take the original (the Figma
// plugin). SourceRef is the image's checksum_sha256 or the poster's storage
// key.
type MediaPreview struct {
	bun.BaseModel `bun:"table:media_previews,alias:mp" swaggerignore:"true"`
	TenantScoped

	Source      string    `bun:"source,pk"                                    json:"source"`
	SourceRef   string    `bun:"source_ref,pk"                                json:"source_ref"`
	MaxLongEdge int       `bun:"max_long_edge,pk"                             json:"max_long_edge"`
	S3Key       string    `bun:"s3_key,notnull"                               json:"s3_key"`
	MimeType    string    `bun:"mime_type,notnull"                            json:"mime_type"`
	Width       int       `bun:"width,notnull"                                json:"width"`
	Height      int       `bun:"height,notnull"                               json:"height"`
	SizeBytes   int64     `bun:"size_bytes,notnull"                           json:"size_bytes"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// MediaPreviewKey identifies one preview.
type MediaPreviewKey struct {
	Source    string
	SourceRef string
}
