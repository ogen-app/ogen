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
// Used to order and cap suggestions.
type SuggestionSeverity string

const (
	SeverityHigh   SuggestionSeverity = "high"
	SeverityMedium SuggestionSeverity = "medium"
	SeverityLow    SuggestionSeverity = "low"
)

// Rank orders severities for sorting: high is 0, medium 1, low 2, anything
// else after them.
func (s SuggestionSeverity) Rank() int {
	switch s {
	case SeverityHigh:
		return 0
	case SeverityMedium:
		return 1
	case SeverityLow:
		return 2
	}
	return 3
}

// EvaluationSuggestion is one span-anchored, guidance-only improvement
// note. Span quotes the post text it reacts to, the guard against generic
// advice; Advice is one sentence naming what is wrong and what to do.
type EvaluationSuggestion struct {
	Dimension EvaluationDimensionKey `json:"dimension"`
	Severity  SuggestionSeverity     `json:"severity"`
	Span      string                 `json:"span"`
	// Advice may wrap its one essential phrase in a single **…** pair; no
	// other markup. Rows written before emphasis are plain.
	Advice string `json:"advice"`
}

// UnmarshalJSON reads both the current shape and stored rows written with a
// separate issue and fix, which are joined into Advice.
func (s *EvaluationSuggestion) UnmarshalJSON(b []byte) error {
	type current EvaluationSuggestion
	var v struct {
		current
		Issue string `json:"issue"`
		Fix   string `json:"fix"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = EvaluationSuggestion(v.current)
	if s.Advice == "" {
		s.Advice = joinSentences(v.Issue, v.Fix)
	}
	return nil
}

// EvaluationDimension is the per-dimension result. Score, Verdict and
// Suggestions come from the model (the verdict is written before the score
// per the prompt contract). Weight and Contribution are filled by the
// backend during score composition; the model never sees or sets them.
type EvaluationDimension struct {
	Score int `json:"score"`
	// Verdict may wrap its one essential phrase in a single **…** pair; no
	// other markup. At most 240 characters without the markers. Rows
	// written before emphasis are plain.
	Verdict     string                 `json:"verdict"`
	Suggestions []EvaluationSuggestion `json:"suggestions"`

	// Weight is the fraction (0-1) this dimension contributed to the
	// overall, taken from the Type weight profile. Contribution is the
	// weighted percentage points it added to overall_pct
	// (Weight * Score/10 * 100). Both are backend-computed.
	Weight       float64 `json:"weight"`
	Contribution float64 `json:"contribution"`
}

// UnmarshalJSON reads both the current shape and stored rows written with a
// rationale, which becomes the Verdict.
func (d *EvaluationDimension) UnmarshalJSON(b []byte) error {
	type current EvaluationDimension
	var v struct {
		current
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*d = EvaluationDimension(v.current)
	if d.Verdict == "" {
		d.Verdict = v.Rationale
	}
	return nil
}

func joinSentences(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + " " + b
}

// EvaluationResult is the full four-dimension assessment, stored as JSON
// in post_evaluations.result.
type EvaluationResult struct {
	Correctness EvaluationDimension `json:"correctness"`
	Clarity     EvaluationDimension `json:"clarity"`
	Engagement  EvaluationDimension `json:"engagement"`
	Delivery    EvaluationDimension `json:"delivery"`
}

// Dimensions returns pointers to the four dimensions in rubric order.
func (r *EvaluationResult) Dimensions() []*EvaluationDimension {
	return []*EvaluationDimension{&r.Correctness, &r.Clarity, &r.Engagement, &r.Delivery}
}

// Dimension returns a pointer to the dimension for key, or nil for an
// unknown key.
func (r *EvaluationResult) Dimension(key EvaluationDimensionKey) *EvaluationDimension {
	switch key {
	case DimensionCorrectness:
		return &r.Correctness
	case DimensionClarity:
		return &r.Clarity
	case DimensionEngagement:
		return &r.Engagement
	case DimensionDelivery:
		return &r.Delivery
	}
	return nil
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
	InputHash string `bun:"input_hash,notnull"                           json:"input_hash"`
	// ContentHash and ContextHash split InputHash into the scored post text
	// and everything else the model saw, so a read can say which side went
	// stale and automatic runs can skip context-only changes.
	ContentHash string `bun:"content_hash,notnull" json:"-"`
	ContextHash string `bun:"context_hash,notnull" json:"-"`
	// ModelRuns counts the model runs behind this row; cached reuses do not
	// count. The quality_reviews_per_post entitlement caps it.
	ModelRuns int `bun:"model_runs,notnull" json:"runs_used"`
	// Trigger is what started the run that produced this row.
	Trigger   QualityTrigger `bun:"trigger,notnull"                              json:"trigger"`
	CreatedAt time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt time.Time      `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// QualityTrigger is what started a quality assessment: the author's request,
// or one of the post lifecycle checkpoints that assess automatically.
type QualityTrigger string

const (
	QualityTriggerManual    QualityTrigger = "manual"
	QualityTriggerReady     QualityTrigger = "ready"
	QualityTriggerVersion   QualityTrigger = "version"
	QualityTriggerScheduled QualityTrigger = "scheduled"
	QualityTriggerPublished QualityTrigger = "published"
)

// IsAuto reports whether the trigger is an automatic lifecycle checkpoint.
func (t QualityTrigger) IsAuto() bool {
	return t != "" && t != QualityTriggerManual
}

// QualityStaleReason says which side of an assessment's inputs changed since
// it ran.
type QualityStaleReason string

const (
	QualityStaleContent QualityStaleReason = "content"
	QualityStaleContext QualityStaleReason = "context"
	QualityStaleBoth    QualityStaleReason = "both"
)

// StaleReason compares the stored hashes with the post's current ones and
// returns what changed, or "" when the assessment is current.
func (e *PostEvaluation) StaleReason(contentHash, contextHash string) QualityStaleReason {
	content := e.ContentHash != contentHash
	context := e.ContextHash != contextHash
	switch {
	case content && context:
		return QualityStaleBoth
	case content:
		return QualityStaleContent
	case context:
		return QualityStaleContext
	}
	return ""
}
