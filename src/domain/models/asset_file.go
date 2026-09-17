package models

import (
	"time"

	"github.com/uptrace/bun"
)

// AssetFile stores metadata for file-backed Assets (currently PDF uploads).
// One-to-one with Asset via UNIQUE(asset_id).
type AssetFile struct {
	bun.BaseModel `bun:"table:asset_files,alias:af" swaggerignore:"true"`
	TenantScoped  // CON-97: tenant_id column + central scoping hooks

	ID             string  `bun:"id,pk"                                        json:"id"`
	AssetID        string  `bun:"asset_id,notnull"                             json:"asset_id"`
	OriginalName   string  `bun:"original_name,notnull"                        json:"original_name"`
	MimeType       string  `bun:"mime_type,notnull"                            json:"mime_type"`
	SizeBytes      int64   `bun:"size_bytes,notnull"                           json:"size_bytes"`
	S3Key          string  `bun:"s3_key,notnull"                               json:"s3_key"`
	ThumbnailS3Key *string `bun:"thumbnail_s3_key"                             json:"thumbnail_s3_key"`
	// NormalizedS3Key points at the browser-drawable derivative image-service
	// writes for every image (assets/{id}/normalized.png). It exists precisely so
	// formats a browser can't decode — HEIC/HEIF, TIFF (CON-281/CON-299) — can
	// still be shown: url is the original bytes, this is the copy an <img> renders.
	// Null for PDFs and for images whose extraction failed or is still pending.
	NormalizedS3Key *string `bun:"normalized_s3_key"                           json:"normalized_s3_key"`
	PageCount       *int    `bun:"page_count"                                   json:"page_count"`
	// Width, Height, IsAnimated and ChecksumSHA256 describe image files (CON-246).
	// Same names/types as post_attachments so the attach-to-post bridge is a
	// field copy. Zero/empty for PDFs. ChecksumSHA256 also backs upload dedupe.
	Width          int       `bun:"width,notnull,default:0"                      json:"width"`
	Height         int       `bun:"height,notnull,default:0"                     json:"height"`
	IsAnimated     bool      `bun:"is_animated,notnull,default:false"            json:"is_animated"`
	ChecksumSHA256 string    `bun:"checksum_sha256"                              json:"checksum_sha256,omitempty"`
	CreatedAt      time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt      time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`

	// URL, ThumbnailURL and NormalizedURL are transient public URLs rendered from
	// S3Key / ThumbnailS3Key / NormalizedS3Key by the handler layer before
	// serialization. URL is the original bytes (CON-246); NormalizedURL is the copy
	// a browser can draw (CON-299) — the full-size asset screen renders it for
	// HEIC/TIFF; ThumbnailURL is the small preview. Not persisted; minted per
	// response so a signed/public URL is never stored or cached client-side.
	URL           *string `bun:"-" json:"url,omitempty"`
	ThumbnailURL  *string `bun:"-" json:"thumbnail_url,omitempty"`
	NormalizedURL *string `bun:"-" json:"normalized_url,omitempty"`
}
