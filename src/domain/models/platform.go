package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// ImageConstraints is the structured rule set carried per platform row
// for post-attachment validation (CON-73). It serialises as JSON in a
// jsonb column.
//
// Storing this on the platform row (rather than a Go-side map keyed by
// some platform identifier) keeps the rules portable across
// installations and sets up the spec's §6 "per-workspace override"
// hint cleanly — overrides land as a separate table joined onto
// platforms, no validator change needed.
type ImageConstraints struct {
	MaxFileSizeBytes      int64    `json:"max_file_size_bytes"`
	AllowedFormats        []string `json:"allowed_formats"`
	AnimatedGIFSupported  bool     `json:"animated_gif_supported"`
	MaxAttachmentsPerPost int      `json:"max_attachments_per_post"`
}

func (c ImageConstraints) Value() (driver.Value, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

func (c *ImageConstraints) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*c = ImageConstraints{}
			return nil
		}
		return json.Unmarshal([]byte(v), c)
	case []byte:
		if len(v) == 0 {
			*c = ImageConstraints{}
			return nil
		}
		return json.Unmarshal(v, c)
	case nil:
		*c = ImageConstraints{}
		return nil
	default:
		return fmt.Errorf("ImageConstraints: cannot scan %T", src)
	}
}

// IsZero reports whether c carries no rules — used by callers to skip
// validation cleanly when a platform has not opted in to constraints.
func (c ImageConstraints) IsZero() bool {
	return c.MaxFileSizeBytes == 0 &&
		len(c.AllowedFormats) == 0 &&
		!c.AnimatedGIFSupported &&
		c.MaxAttachmentsPerPost == 0
}

// PDFConstraints is the sibling rule set for PDF post attachments
// (CON-75). A zero value means "this platform does not accept PDFs",
// which the validator surfaces as a soft warning.
type PDFConstraints struct {
	MaxFileSizeBytes      int64    `json:"max_file_size_bytes"`
	AllowedFormats        []string `json:"allowed_formats"`
	MaxPages              int      `json:"max_pages"`
	MaxAttachmentsPerPost int      `json:"max_attachments_per_post"`
}

func (c PDFConstraints) Value() (driver.Value, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

func (c *PDFConstraints) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*c = PDFConstraints{}
			return nil
		}
		return json.Unmarshal([]byte(v), c)
	case []byte:
		if len(v) == 0 {
			*c = PDFConstraints{}
			return nil
		}
		return json.Unmarshal(v, c)
	case nil:
		*c = PDFConstraints{}
		return nil
	default:
		return fmt.Errorf("PDFConstraints: cannot scan %T", src)
	}
}

func (c PDFConstraints) IsZero() bool {
	return c.MaxFileSizeBytes == 0 &&
		len(c.AllowedFormats) == 0 &&
		c.MaxPages == 0 &&
		c.MaxAttachmentsPerPost == 0
}

// VideoConstraints is the sibling rule set for video post attachments
// (CON-148). A zero value means "this platform does not accept video",
// which the validator surfaces as a soft warning — mirroring the PDF
// branch. Duration/resolution/aspect fields are only enforced when
// non-zero, so a platform can opt into just the checks it cares about.
type VideoConstraints struct {
	MaxFileSizeBytes      int64    `json:"max_file_size_bytes"`
	AllowedFormats        []string `json:"allowed_formats"` // e.g. ["mp4","mov","webm"]
	MaxDurationSeconds    int      `json:"max_duration_seconds"`
	MinDurationSeconds    int      `json:"min_duration_seconds"` // Reels/Shorts have floors
	MaxWidth              int      `json:"max_width"`            // 0 = unbounded
	MaxHeight             int      `json:"max_height"`           // 0 = unbounded
	AllowedAspectRatios   []string `json:"allowed_aspect_ratios"`
	MaxAttachmentsPerPost int      `json:"max_attachments_per_post"` // usually 1
	// RequiresVideoTitle blocks publishing a video post whose title is empty
	// (CON-148 §9). YouTube requires a title; most feed/Reel platforms derive
	// one from the caption, so this stays false for them.
	RequiresVideoTitle bool `json:"requires_video_title"`
}

func (c VideoConstraints) Value() (driver.Value, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

func (c *VideoConstraints) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*c = VideoConstraints{}
			return nil
		}
		return json.Unmarshal([]byte(v), c)
	case []byte:
		if len(v) == 0 {
			*c = VideoConstraints{}
			return nil
		}
		return json.Unmarshal(v, c)
	case nil:
		*c = VideoConstraints{}
		return nil
	default:
		return fmt.Errorf("VideoConstraints: cannot scan %T", src)
	}
}

func (c VideoConstraints) IsZero() bool {
	return c.MaxFileSizeBytes == 0 &&
		len(c.AllowedFormats) == 0 &&
		c.MaxDurationSeconds == 0 &&
		c.MinDurationSeconds == 0 &&
		c.MaxWidth == 0 &&
		c.MaxHeight == 0 &&
		len(c.AllowedAspectRatios) == 0 &&
		c.MaxAttachmentsPerPost == 0 &&
		!c.RequiresVideoTitle
}

// TextConstraints is the sibling rule set for post text length (CON-91).
// The opaque `constraints` prose column still feeds the model prompt; this
// carries the machine-readable limits the composer's Validations panel (and
// the publish gate) check against.
//
// Char limits vary by platform AND by post type — LinkedIn feed posts cap at
// 3000 while native articles allow ~100k — so the default lives here with
// PerPostType overriding it per slug. Counts are Unicode code points
// (runes), not bytes. A zero value means "no text limit on this platform".
type TextConstraints struct {
	// MaxContentChars is the default body-text ceiling applied to every post
	// type that PerPostType doesn't override. 0 = unbounded.
	MaxContentChars int `json:"max_content_chars"`
	// MaxTitleChars caps the title on platforms with a distinct title field
	// (YouTube, LinkedIn article). 0 = no separate title limit.
	MaxTitleChars int `json:"max_title_chars"`
	// PerPostType overrides MaxContentChars for specific post-type slugs —
	// e.g. LinkedIn {"article": 100000} while its feed posts stay at 3000.
	PerPostType map[string]int `json:"per_post_type,omitempty"`
}

func (c TextConstraints) Value() (driver.Value, error) {
	b, err := json.Marshal(c)
	return string(b), err
}

func (c *TextConstraints) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*c = TextConstraints{}
			return nil
		}
		// Reset first: json.Unmarshal merges into an existing PerPostType map
		// (and leaves omitted scalars untouched), so a reused receiver would
		// otherwise carry state from a prior scan.
		*c = TextConstraints{}
		return json.Unmarshal([]byte(v), c)
	case []byte:
		if len(v) == 0 {
			*c = TextConstraints{}
			return nil
		}
		*c = TextConstraints{}
		return json.Unmarshal(v, c)
	case nil:
		*c = TextConstraints{}
		return nil
	default:
		return fmt.Errorf("TextConstraints: cannot scan %T", src)
	}
}

func (c TextConstraints) IsZero() bool {
	return c.MaxContentChars == 0 &&
		c.MaxTitleChars == 0 &&
		len(c.PerPostType) == 0
}

// ContentLimitFor returns the resolved body-text ceiling for a post-type
// slug: the PerPostType override when present, otherwise MaxContentChars.
// A zero return means "unbounded" — callers skip the check.
func (c TextConstraints) ContentLimitFor(slug string) int {
	if v, ok := c.PerPostType[slug]; ok {
		return v
	}
	return c.MaxContentChars
}

type Platform struct {
	bun.BaseModel `bun:"table:platforms,alias:pl" swaggerignore:"true"`

	ID   string `bun:"id,pk"        json:"id"`
	Name string `bun:"name,notnull" json:"name"`
	// ZernioID is the Zernio wire slug ("twitter", "linkedin", …). It replaces
	// the retired Go registry's sqidToZernioID map (CON-292): the publish path
	// and connect flow resolve this off the row. "" means the operator has not
	// yet assigned a slug (the row is not publishable until they do).
	ZernioID string `bun:"zernio_id,notnull,default:''" json:"zernio_id"`
	// Enabled is the operator soft on/off switch (CON-292). A disabled platform
	// drops from GET /api/platforms and blocks new connects, but already-scheduled
	// posts still publish (the publish path resolves zernio_id regardless).
	Enabled bool `bun:"enabled,notnull,default:false" json:"enabled"`
	// ConnectSupported records whether Ogen can OAuth-redirect connect this
	// platform. false documents the Bluesky-style app-password exclusion as data
	// rather than code.
	ConnectSupported bool        `bun:"connect_supported,notnull,default:true" json:"connect_supported"`
	PostTypes        PostTypeMap `bun:"post_types,notnull,type:jsonb"          json:"post_types"`
	// SupportedPostTypes is the Zernio-publishable subset of PostTypes' slugs
	// (CON-292) — replaces SupportedPlatform.SupportedPostTypes. PostTypes carries
	// every slug for display; this array marks which ones actually publish.
	SupportedPostTypes StringSlice      `bun:"supported_post_types,notnull,type:jsonb"      json:"supported_post_types"`
	Cadence            string           `bun:"cadence,notnull"                              json:"cadence"`
	Constraints        string           `bun:"constraints,notnull"                          json:"constraints"`
	ImageConstraints   ImageConstraints `bun:"image_constraints,notnull,type:jsonb"         json:"image_constraints"`
	PDFConstraints     PDFConstraints   `bun:"pdf_constraints,notnull,type:jsonb"           json:"pdf_constraints"`
	VideoConstraints   VideoConstraints `bun:"video_constraints,notnull,type:jsonb"         json:"video_constraints"`
	TextConstraints    TextConstraints  `bun:"text_constraints,notnull,type:jsonb"          json:"text_constraints"`
	// SortOrder drives composer/picker ordering (CON-292).
	SortOrder int       `bun:"sort_order,notnull,default:0"                 json:"sort_order"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
