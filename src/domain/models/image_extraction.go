package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Image-extraction lifecycle statuses (CON-281). The extraction row is the
// per-run state machine ogen owns while image-service does stateless compute:
// pending -> normalizing -> classifying -> extracting -> describing ->
// (complete | partial | failed). image-service performs the whole pipeline in a
// single Extract RPC, so ogen mostly transitions pending -> (complete|partial|
// failed); the intermediate statuses exist for observability and for a future
// checkpointed multi-call flow.
const (
	ImageExtractionStatusPending     = "pending"
	ImageExtractionStatusNormalizing = "normalizing"
	ImageExtractionStatusClassifying = "classifying"
	ImageExtractionStatusExtracting  = "extracting"
	ImageExtractionStatusDescribing  = "describing"
	// ImageExtractionStatusPartial: the description landed (asset searchable) but
	// structured extraction failed — blocks are absent, queryable, retriable.
	ImageExtractionStatusPartial  = "partial"
	ImageExtractionStatusComplete = "complete"
	ImageExtractionStatusFailed   = "failed"
)

// Image shapes (CON-281 §8). Exactly one is chosen before extraction; the long
// tail collapses to "creative" (no sixth shape in v1).
const (
	ImageShapeProse        = "prose"
	ImageShapeConversation = "conversation"
	ImageShapeSocialPost   = "social_post"
	ImageShapeTabular      = "tabular"
	ImageShapeCreative     = "creative"
)

// ImageExtraction is one vision-processing run of an IMG asset (CON-281). It
// holds only processing state — the searchable output (description + extracted
// blocks) lands in assets_chunks, and the human-facing blocks in image_blocks.
// A re-extraction mints a fresh row under a new RunKey. Idempotent on
// (asset_id, run_key). cost_micros is snapshotted at write time from the
// versioned gemini price table (CON-86) and never recomputed.
type ImageExtraction struct {
	bun.BaseModel `bun:"table:image_extractions,alias:ie" swaggerignore:"true"`
	TenantScoped  // CON-97: tenant_id column + central scoping hooks

	ID      string `bun:"id,pk"            json:"id"`
	AssetID string `bun:"asset_id,notnull" json:"asset_id"`
	// RunKey makes an extraction idempotent: a duplicate enqueue for the same
	// (asset_id, run_key) hits the unique index and no-ops. A re-extract uses a
	// new RunKey so it is a distinct, additive run.
	RunKey string `bun:"run_key,notnull"  json:"run_key"`
	Status string `bun:"status,notnull"   json:"status"`

	// Shape is the classified image shape (prose/conversation/social_post/
	// tabular/creative); ClassifyConfidence is the model's self-reported
	// confidence in it.
	Shape              string  `bun:"shape"               json:"shape,omitempty"`
	ClassifyConfidence float64 `bun:"classify_confidence" json:"classify_confidence"`

	// Model ids used this run (config, never compiled in). EscalateModel is only
	// meaningful when Escalated is true.
	ClassifyModel string `bun:"classify_model" json:"classify_model,omitempty"`
	ExtractModel  string `bun:"extract_model"  json:"extract_model,omitempty"`
	EscalateModel string `bun:"escalate_model" json:"escalate_model,omitempty"`

	// Escalated marks a one-shot re-run at a stronger model on low confidence;
	// EscalationImproved records whether it actually helped (recorded regardless).
	Escalated          bool `bun:"escalated,notnull,default:false"           json:"escalated"`
	EscalationImproved bool `bun:"escalation_improved,notnull,default:false" json:"escalation_improved"`
	// DescriptionOK/ExtractionOK/Truncated are the service's self-reported quality
	// signals; a description-ok/extraction-failed run settles to `partial`.
	DescriptionOK bool `bun:"description_ok,notnull,default:false" json:"description_ok"`
	ExtractionOK  bool `bun:"extraction_ok,notnull,default:false" json:"extraction_ok"`
	Truncated     bool `bun:"truncated,notnull,default:false"     json:"truncated"`

	// Normalized derivative metadata (the re-encoded normalized.png the service
	// wrote and future re-runs read). NormalizedS3Key is the tenant-relative key;
	// nil until normalization succeeds.
	NormalizedS3Key *string `bun:"normalized_s3_key"                json:"normalized_s3_key,omitempty"`
	NormalizedMime  string  `bun:"normalized_mime,notnull,default:''" json:"normalized_mime,omitempty"`
	Width           int     `bun:"width,notnull,default:0"          json:"width"`
	Height          int     `bun:"height,notnull,default:0"         json:"height"`
	IsAnimated      bool    `bun:"is_animated,notnull,default:false" json:"is_animated"`
	ChecksumSHA256  string  `bun:"checksum_sha256,notnull,default:''" json:"checksum_sha256,omitempty"`

	// Token totals across the run's vision calls (classify + extract + describe +
	// alt + any escalation); CostMicros is the snapshotted price, PriceVersion the
	// table it came from.
	InputTokens  int64  `bun:"input_tokens,notnull,default:0"  json:"input_tokens"`
	OutputTokens int64  `bun:"output_tokens,notnull,default:0" json:"output_tokens"`
	CostMicros   int64  `bun:"cost_micros,notnull,default:0"   json:"cost_micros"`
	PriceVersion string `bun:"price_version,notnull,default:''" json:"price_version,omitempty"`
	// FailureReason is a tenant-visible reason for a terminal failed/partial run
	// (unsupported/vector, over pixel-area/size, over quota, unreadable).
	FailureReason string `bun:"failure_reason,notnull,default:''" json:"failure_reason,omitempty"`
	// FailureCode is the stable, machine-readable companion to FailureReason
	// (CON-281) — one of models.UploadCode* — so the client can distinguish a
	// quota block from a bad image from a transient outage from a searchable-but-
	// partial run without parsing prose. Empty on a clean complete run.
	FailureCode string `bun:"failure_code,notnull,default:''" json:"failure_code,omitempty"`

	// Blocks is hydrated by the read API from image_blocks; not persisted here.
	Blocks []ImageBlock `bun:"-" json:"blocks,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
