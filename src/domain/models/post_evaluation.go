package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// EvaluationDimensionKey identifies one of the four fixed quality
// dimensions a Post is scored on. The set is closed: the rubric,
// the weight profiles, and the structured output all key off these.
type EvaluationDimensionKey string

const (
	DimensionCorrectness EvaluationDimensionKey = "correctness"
	DimensionClarity     EvaluationDimensionKey = "clarity"
	DimensionEngagement  EvaluationDimensionKey = "engagement"
	DimensionDelivery    EvaluationDimensionKey = "delivery"
)

// SuggestionSeverity ranks how badly a suggestion's issue hurts the post.
// Used to order and cap suggestions (top-3 per dimension by severity).
type SuggestionSeverity string

const (
	SeverityHigh   SuggestionSeverity = "high"
	SeverityMedium SuggestionSeverity = "medium"
	SeverityLow    SuggestionSeverity = "low"
)

// EvaluationSuggestion is one span-anchored, guidance-only improvement
// note. Span is required and must quote/point to actual post
// text — it is the primary guard against generic advice ("make it more
// engaging"). v1 suggestions are guidance, not rewrites.
type EvaluationSuggestion struct {
	Dimension EvaluationDimensionKey `json:"dimension"`
	Severity  SuggestionSeverity     `json:"severity"`
	Issue     string                 `json:"issue"`
	Fix       string                 `json:"fix"`
	Span      string                 `json:"span"`
}

// EvaluationDimension is the per-dimension result. Score, Rationale,
// Weakness, and Suggestions come from the model (the rationale is written
// before the score per the prompt contract). Weight and Contribution are
// filled by the backend during score composition — the model never sees
// or sets them.
type EvaluationDimension struct {
	Score       int                    `json:"score"`
	Rationale   string                 `json:"rationale"`
	Weakness    string                 `json:"weakness"`
	Suggestions []EvaluationSuggestion `json:"suggestions"`

	// Weight is the fraction (0-1) this dimension contributed to the
	// overall, taken from the Type weight profile. Contribution is the
	// weighted percentage points it added to overall_pct
	// (Weight * Score/10 * 100). Both are backend-computed.
	Weight       float64 `json:"weight"`
	Contribution float64 `json:"contribution"`
}

// EvaluationResult is the full four-dimension assessment, stored as JSON
// in post_evaluations.result.
type EvaluationResult struct {
	Correctness EvaluationDimension `json:"correctness"`
	Clarity     EvaluationDimension `json:"clarity"`
	Engagement  EvaluationDimension `json:"engagement"`
	Delivery    EvaluationDimension `json:"delivery"`
}

func (r EvaluationResult) Value() (driver.Value, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

func (r *EvaluationResult) Scan(src any) error {
	switch v := src.(type) {
	case string:
		if v == "" {
			*r = EvaluationResult{}
			return nil
		}
		return json.Unmarshal([]byte(v), r)
	case []byte:
		if len(v) == 0 {
			*r = EvaluationResult{}
			return nil
		}
		return json.Unmarshal(v, r)
	case nil:
		*r = EvaluationResult{}
		return nil
	default:
		return fmt.Errorf("EvaluationResult: cannot scan %T", src)
	}
}

// PostEvaluation is the latest quality assessment for a Post.
// Exactly one row per Post: re-evaluation overwrites via upsert on
// post_id, so this always reflects the most recent run.
//
// PlatformID and PlatformPostType are echoed from the Post at evaluation
// time. CaptionScoped is true when the Post carried media the model could
// not see (the score is caption/text-scoped). OverallPct and the
// per-dimension Weight/Contribution inside Result are backend-computed,
// never returned by the model.
type PostEvaluation struct {
	bun.BaseModel `bun:"table:post_evaluations,alias:pe" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID               string           `bun:"id,pk"                                        json:"id"`
	PostID           string           `bun:"post_id,notnull"                              json:"post_id"`
	PlatformID       string           `bun:"platform_id,notnull"                          json:"platform_id"`
	PlatformPostType string           `bun:"platform_post_type,notnull"                   json:"platform_post_type"`
	CaptionScoped    bool             `bun:"caption_scoped,notnull"                       json:"caption_scoped"`
	OverallPct       float64          `bun:"overall_pct,notnull"                          json:"overall_pct"`
	Result           EvaluationResult `bun:"result,notnull,type:jsonb"                    json:"result"`
	ModelID          string           `bun:"model_id,notnull"                             json:"model_id"`
	// InputHash fingerprints the assessment inputs (rendered prompt + model)
	// so the assess flow skips re-running the model when nothing the model
	// sees has changed.
	InputHash string    `bun:"input_hash,notnull"                           json:"input_hash"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}
