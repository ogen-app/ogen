package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Audio-segment statuses (CON-282). A segment is a bounded window of the
// normalized derivative, transcribed independently and checkpointed so a retry
// resumes from the first incomplete one.
const (
	AudioSegmentStatusPending = "pending"
	AudioSegmentStatusDone    = "done"
	AudioSegmentStatusFailed  = "failed"
)

// AudioSegment is one bounded transcription window of an extraction (CON-282).
// Boundaries + source offsets are recorded (not inferred); the overlap between
// adjacent segments is de-duplicated at chunk-assembly time. [StartMs, EndMs)
// are on the ORIGINAL asset timeline.
type AudioSegment struct {
	bun.BaseModel `bun:"table:audio_segments,alias:aseg" swaggerignore:"true"`
	TenantScoped

	ID             string `bun:"id,pk"                json:"id"`
	ExtractionID   string `bun:"extraction_id,notnull" json:"extraction_id"`
	AssetID        string `bun:"asset_id,notnull"     json:"asset_id"`
	Index          int    `bun:"index,notnull"        json:"index"`
	StartMs        int64  `bun:"start_ms,notnull"     json:"start_ms"`
	EndMs          int64  `bun:"end_ms,notnull"       json:"end_ms"`
	Status         string `bun:"status,notnull"       json:"status"`
	RetryCount     int    `bun:"retry_count,notnull,default:0" json:"retry_count"`
	FailureReason  string `bun:"failure_reason"      json:"failure_reason,omitempty"`
	UtteranceCount int    `bun:"utterance_count,notnull,default:0" json:"utterance_count"`
	// CostMicros is this segment's transcription cost (CON-282), snapshotted from
	// the versioned gemini price table and persisted in the SAME write that marks
	// the segment done — so cost and completion commit atomically and a resume
	// that skips a done segment never loses its cost. The extraction total sums
	// these.
	CostMicros int64 `bun:"cost_micros,notnull,default:0" json:"cost_micros"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
