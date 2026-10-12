package models

import (
	"slices"
	"time"

	"github.com/uptrace/bun"
)

// AssetAnalysisStatus is where one analysis lens is in its lifecycle. It is a
// closed set mirrored by a DB CHECK.
type AssetAnalysisStatus string

const (
	AssetAnalysisStatusPending AssetAnalysisStatus = "pending"
	AssetAnalysisStatusRunning AssetAnalysisStatus = "running"
	AssetAnalysisStatusReady   AssetAnalysisStatus = "ready"
	AssetAnalysisStatusFailed  AssetAnalysisStatus = "failed"
	// AssetAnalysisStatusSkipped means the run was gated (entitlement off or
	// monthly cap reached); FailureCode says which.
	AssetAnalysisStatusSkipped AssetAnalysisStatus = "skipped"
)

// Retryable reports whether the retry endpoint may re-enqueue this status.
func (s AssetAnalysisStatus) Retryable() bool {
	return s == AssetAnalysisStatusFailed || s == AssetAnalysisStatusSkipped
}

// AssetAnalysisStrategy is how the source was fed to the model.
type AssetAnalysisStrategy string

const (
	// AssetAnalysisStrategyFull is one call over the whole source.
	AssetAnalysisStrategyFull AssetAnalysisStrategy = "full"
	// AssetAnalysisStrategyMapReduce summarises source groups first, then
	// analyses the group notes.
	AssetAnalysisStrategyMapReduce AssetAnalysisStrategy = "map_reduce"
)

// Machine-readable reasons an analysis is failed or skipped.
const (
	AnalysisCodeEntitlementDisabled = "entitlement_disabled"
	AnalysisCodeLimitReached        = "limit_reached"
	AnalysisCodeEmptySource         = "empty_source"
	AnalysisCodeInvalidOutput       = "invalid_output"
	AnalysisCodeSourceUnavailable   = "source_unavailable"
	AnalysisCodeModelUnavailable    = "model_unavailable"
	AnalysisCodeModelRefused        = "model_refused"
)

// AnalyzableAssetTypes are the asset types that carry text worth analysing.
// Images are excluded: their content is a vision description, not source
// material. Untyped (legacy markdown) assets are analysed too.
var AnalyzableAssetTypes = []string{AssetTypeMarkdown, AssetTypePDF, AssetTypeURL, AssetTypeDocument, AssetTypeAudio}

// AnalyzableAssetStatuses are the asset statuses whose extracted text is
// settled enough to analyse.
var AnalyzableAssetStatuses = []string{AssetStatusReady, AssetStatusPartial}

// IsAnalyzableAssetType reports whether an asset type carries text worth
// analysing.
func IsAnalyzableAssetType(t *string) bool {
	if t == nil {
		// Untyped assets are legacy markdown created before the type column.
		return true
	}
	return slices.Contains(AnalyzableAssetTypes, *t)
}

// IsAnalyzableAssetStatus reports whether an asset's extracted text is
// settled enough to analyse.
func IsAnalyzableAssetStatus(status string) bool {
	return slices.Contains(AnalyzableAssetStatuses, status)
}

// AnalysisEntity is a named thing the source mentions.
type AnalysisEntity struct {
	Name string `json:"name"`
	// Kind is person, org, product, place or other.
	Kind string `json:"kind"`
}

// AnalysisParticipant is a person the model inferred took part in a recording.
type AnalysisParticipant struct {
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
	// Confidence is "stated" when the person introduced themselves or was named
	// directly, "inferred" when the model deduced it.
	Confidence string `json:"confidence"`
}

// AnalysisCommitment is a promise made in a recording.
type AnalysisCommitment struct {
	Owner      string `json:"owner"`
	Commitment string `json:"commitment"`
	Due        string `json:"due,omitempty"`
	// SourceLabel cites where it was said, e.g. a "12:03–12:47" time range.
	SourceLabel string `json:"source_label,omitempty"`
	Confidence  string `json:"confidence"`
}

// AnalysisDecision is a decision taken in a recording.
type AnalysisDecision struct {
	Text        string `json:"text"`
	SourceLabel string `json:"source_label,omitempty"`
}

// AnalysisRelevance scores how well an asset serves a campaign. Only campaign
// lenses carry it.
type AnalysisRelevance struct {
	Score int    `json:"score"`
	Why   string `json:"why"`
}

// AssetAnalysisResult is the structured part of an analysis, stored as jsonb.
// The meeting fields are filled only for audio.
type AssetAnalysisResult struct {
	KeyPoints     []string              `json:"key_points"`
	Topics        []string              `json:"topics"`
	Entities      []AnalysisEntity      `json:"entities"`
	Participants  []AnalysisParticipant `json:"participants,omitempty"`
	Commitments   []AnalysisCommitment  `json:"commitments,omitempty"`
	Decisions     []AnalysisDecision    `json:"decisions,omitempty"`
	OpenQuestions []string              `json:"open_questions,omitempty"`
	Relevance     *AnalysisRelevance    `json:"relevance,omitempty"`
	// MapModelID is the model that ran the map step of a map-reduce run.
	MapModelID string `json:"map_model_id,omitempty"`
}

// AssetAnalysis is one lens of an asset's analysis: the workspace lens when
// CampaignID is nil, otherwise the lens of that campaign.
type AssetAnalysis struct {
	bun.BaseModel `bun:"table:asset_analyses,alias:aa" swaggerignore:"true"`
	TenantScoped

	ID         string  `bun:"id,pk"                                        json:"id"`
	AssetID    string  `bun:"asset_id,notnull"                             json:"asset_id"`
	CampaignID *string `bun:"campaign_id"                                  json:"campaign_id"`
	// CampaignName is read alongside the lens, never written.
	CampaignName       *string                `bun:"campaign_name,scanonly" json:"campaign_name"`
	Status             AssetAnalysisStatus    `bun:"status,notnull"                               json:"status"         swaggertype:"string" enums:"pending,running,ready,failed,skipped"`
	Strategy           *AssetAnalysisStrategy `bun:"strategy"                                     json:"strategy"       swaggertype:"string" enums:"full,map_reduce" extensions:"x-nullable"`
	Summary            string                 `bun:"summary,notnull,default:''"                   json:"summary"`
	Result             AssetAnalysisResult    `bun:"result,notnull,type:jsonb"                    json:"-"`
	Language           string                 `bun:"language,notnull,default:''"                  json:"language"`
	PartialSource      bool                   `bun:"partial_source,notnull,default:false"         json:"partial_source"`
	Truncated          bool                   `bun:"truncated,notnull,default:false"              json:"truncated"`
	SourceTokens       int                    `bun:"source_tokens,notnull,default:0"              json:"-"`
	ContextFingerprint string                 `bun:"context_fingerprint,notnull,default:''"       json:"-"`
	// ModelID is the model that produced this analysis. Spend lives in the
	// analytics usage events, not here.
	ModelID       string     `bun:"model_id,notnull,default:''"                  json:"-"`
	FailureCode   string     `bun:"failure_code,notnull,default:''"              json:"failure_code"`
	FailureReason string     `bun:"failure_reason,notnull,default:''"            json:"failure_reason"`
	AnalyzedAt    *time.Time `bun:"analyzed_at"                                  json:"analyzed_at"`
	CreatedAt     time.Time  `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt     time.Time  `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// AssetAnalysisRun records one analysis run that reached the model, for the
// monthly run cap.
type AssetAnalysisRun struct {
	bun.BaseModel `bun:"table:asset_analysis_runs,alias:aar" swaggerignore:"true"`
	TenantScoped

	ID         string    `bun:"id,pk"`
	AnalysisID string    `bun:"analysis_id,notnull"`
	CreatedAt  time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

// IsWorkspaceLens reports whether a is the asset's workspace-wide lens.
func (a *AssetAnalysis) IsWorkspaceLens() bool { return a.CampaignID == nil }
