package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Audio-extraction lifecycle statuses (CON-282). The extraction row is the
// per-run state machine ogen owns while audio-service does stateless compute:
// pending -> normalizing -> transcribing -> (complete | partial | failed).
const (
	AudioExtractionStatusPending      = "pending"
	AudioExtractionStatusNormalizing  = "normalizing"
	AudioExtractionStatusTranscribing = "transcribing"
	// AudioExtractionStatusPartial: some segments transcribed, some failed
	// terminally — queryable via the status API but NOT searchable (chunks are
	// withheld until every segment is done).
	AudioExtractionStatusPartial  = "partial"
	AudioExtractionStatusComplete = "complete"
	AudioExtractionStatusFailed   = "failed"
)

// AudioExtraction is one transcription run of an AUDIO asset (CON-282). It holds
// only processing state — the searchable output lands in assets_chunks. A crash
// mid-run is resumed from the first incomplete segment; re-extraction mints a
// fresh row under a new RunKey. Idempotent on (asset_id, run_key).
type AudioExtraction struct {
	bun.BaseModel `bun:"table:audio_extractions,alias:ae" swaggerignore:"true"`
	TenantScoped  // CON-97: tenant_id column + central scoping hooks

	ID      string `bun:"id,pk"                json:"id"`
	AssetID string `bun:"asset_id,notnull"     json:"asset_id"`
	// RunKey makes an extraction idempotent: a duplicate enqueue for the same
	// (asset_id, run_key) hits the unique index and no-ops. A re-extract uses a
	// new RunKey so it is a distinct run.
	RunKey string `bun:"run_key,notnull"      json:"run_key"`
	Status string `bun:"status,notnull"       json:"status"`

	DetectedLanguage string  `bun:"detected_language"    json:"detected_language,omitempty"`
	SourceDurationMs int64   `bun:"source_duration_ms"   json:"source_duration_ms"`
	NormalizedS3Key  *string `bun:"normalized_s3_key"    json:"normalized_s3_key,omitempty"`
	SegmentCount     int     `bun:"segment_count"        json:"segment_count"`
	TranscribeModel  string  `bun:"transcribe_model"     json:"transcribe_model,omitempty"`
	EmbedModel       string  `bun:"embed_model"          json:"embed_model,omitempty"`
	// AudioSeconds is the processed source duration (a usage/quota dimension).
	AudioSeconds int64 `bun:"audio_seconds"        json:"audio_seconds"`
	// CostMicros is the run's transcription cost, snapshotted at write time from
	// the versioned gemini price table (CON-86); PriceVersion records which table.
	CostMicros   int64  `bun:"cost_micros"          json:"cost_micros"`
	PriceVersion string `bun:"price_version"        json:"price_version,omitempty"`
	// FailureReason is a tenant-visible reason for a terminal failed/partial run
	// (over-duration, over-cap, unusable audio).
	FailureReason string `bun:"failure_reason"      json:"failure_reason,omitempty"`
	// FailureCode is the stable, machine-readable companion to FailureReason
	// (models.UploadCode*), so the client can word it without parsing prose
	// (CON-312).
	FailureCode string `bun:"failure_code,notnull,default:''" json:"failure_code,omitempty"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
