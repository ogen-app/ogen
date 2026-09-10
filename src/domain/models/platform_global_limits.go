package models

import (
	"time"

	"github.com/uptrace/bun"
)

// PlatformGlobalLimitsID is the fixed primary key of the single
// platform_global_limits row. The table carries a CHECK (id = 'global'), so
// there is exactly one row ever.
const PlatformGlobalLimitsID = "global"

// PlatformGlobalLimits holds the cross-platform safety ceilings that used to be
// Go constants (maxImageUploadBytes / maxPDFUploadBytes / maxVideoUploadBytes /
// maxAltTextLen / MaxThreadSegments). CON-292 moves them into operator config: a
// single-row table edited via PlatformAdminService. Unlike the per-platform
// constraint jsonb, these are the hard upload ceilings enforced in the
// attachment handlers and the thread-segment validator.
type PlatformGlobalLimits struct {
	bun.BaseModel `bun:"table:platform_global_limits,alias:pgl" swaggerignore:"true"`

	ID                  string    `bun:"id,pk"                                        json:"id"`
	MaxImageUploadBytes int64     `bun:"max_image_upload_bytes,notnull"               json:"max_image_upload_bytes"`
	MaxPDFUploadBytes   int64     `bun:"max_pdf_upload_bytes,notnull"                 json:"max_pdf_upload_bytes"`
	MaxVideoUploadBytes int64     `bun:"max_video_upload_bytes,notnull"               json:"max_video_upload_bytes"`
	MaxAltTextChars     int       `bun:"max_alt_text_chars,notnull"                   json:"max_alt_text_chars"`
	MaxThreadSegments   int       `bun:"max_thread_segments,notnull"                  json:"max_thread_segments"`
	UpdatedAt           time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// DefaultPlatformGlobalLimits returns the built-in ceilings — identical to the
// seeded row and the former Go constants. It is the safe fallback used when the
// DB read fails at boot so the app still starts (CON-292 §10.2), and the
// reference the migration seed must match byte-for-byte.
func DefaultPlatformGlobalLimits() PlatformGlobalLimits {
	return PlatformGlobalLimits{
		ID:                  PlatformGlobalLimitsID,
		MaxImageUploadBytes: 50 << 20,  // 50 MB
		MaxPDFUploadBytes:   100 << 20, // 100 MB
		MaxVideoUploadBytes: 5 << 30,   // 5 GB
		MaxAltTextChars:     2000,
		MaxThreadSegments:   25,
	}
}
