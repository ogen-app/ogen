package models

import (
	"time"

	"github.com/uptrace/bun"
)

// Utterance is one transcribed span returned by audio-service (CON-282) and
// persisted as the raw transcript. It is the source for both chunk assembly
// (into assets_chunks) and the transcript API. [StartMs, EndMs) are on the
// ORIGINAL asset timeline (model-reported, approximate under Gemini). IsSpeech
// is false for a marked no-speech region (not empty text).
type Utterance struct {
	bun.BaseModel `bun:"table:utterances,alias:utt" swaggerignore:"true"`
	TenantScoped

	ID         string  `bun:"id,pk"                json:"id"`
	SegmentID  string  `bun:"segment_id,notnull"   json:"segment_id"`
	AssetID    string  `bun:"asset_id,notnull"     json:"asset_id"`
	Index      int     `bun:"index,notnull"        json:"index"`
	StartMs    int64   `bun:"start_ms,notnull"     json:"start_ms"`
	EndMs      int64   `bun:"end_ms,notnull"       json:"end_ms"`
	Text       string  `bun:"text,notnull"         json:"text"`
	Confidence float64 `bun:"confidence,notnull,default:-1" json:"confidence"`
	Language   string  `bun:"language"             json:"language,omitempty"`
	IsSpeech   bool    `bun:"is_speech,notnull,default:true" json:"is_speech"`

	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}
