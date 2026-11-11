package models

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/ideafit"
)

// IdeaVerdict is the triage answer on an idea. It is a stable, closed
// set, mirrored by a DB CHECK. A nil verdict means the idea is waiting in the
// inbox.
type IdeaVerdict string

const (
	IdeaVerdictYes   IdeaVerdict = "yes"
	IdeaVerdictLater IdeaVerdict = "later"
	IdeaVerdictNo    IdeaVerdict = "no"
)

// Valid reports whether v is a known verdict.
func (v IdeaVerdict) Valid() bool {
	switch v {
	case IdeaVerdictYes, IdeaVerdictLater, IdeaVerdictNo:
		return true
	default:
		return false
	}
}

// IdeaOrigin records who produced an idea. It is a closed set mirrored by a DB
// CHECK; the empty value is a person typing it in.
type IdeaOrigin string

const (
	IdeaOriginManual IdeaOrigin = ""
	// IdeaOriginAssetAnalysis marks an idea distilled from an asset by its
	// analysis; AssetAnalysisID points at that analysis while it exists.
	IdeaOriginAssetAnalysis IdeaOrigin = "asset_analysis"
	// IdeaOriginContentPlan marks an idea a content plan proposed, usually
	// with a plan (platform, post type, phase, date).
	IdeaOriginContentPlan IdeaOrigin = "content_plan"
	// IdeaOriginAssistant marks an idea the assistant wrote down for the user.
	IdeaOriginAssistant IdeaOrigin = "assistant"
)

// IdeaPlan is where an idea would land as a post. Every field is a
// suggestion, re-checked when the post is made; all nil means no plan.
type IdeaPlan struct {
	PlatformID *string `bun:"platform_id" json:"platform_id" extensions:"x-nullable"`
	PostType   *string `bun:"post_type"   json:"post_type"   extensions:"x-nullable"`
	// PhaseID is a campaign type phase of the idea's campaign.
	PhaseID *string `bun:"phase_id" json:"phase_id" extensions:"x-nullable"`
	// TargetDate is the day it would publish; a dated idea shows on the
	// calendar as a slot, never as a post.
	TargetDate *CalendarDate `bun:"target_date,type:date" json:"target_date" swaggertype:"string" example:"2026-09-01" extensions:"x-nullable"`
}

// IsZero reports whether the plan sets nothing.
func (p IdeaPlan) IsZero() bool {
	return p.PlatformID == nil && p.PostType == nil && p.PhaseID == nil && p.TargetDate == nil
}

// IdeaSourceAsset is the asset an analysis idea came from, with what the
// idea's source line draws.
type IdeaSourceAsset struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	Kind  AssetKind `json:"kind"  swaggertype:"string" enums:"pdf,web,audio,note,image,document,spreadsheet,csv,presentation"`
	// PageCount is null when the file is not paged.
	PageCount *int `json:"page_count" extensions:"x-nullable"`
	// DurationMS is a recording's length; null when unknown or not a recording.
	DurationMS *int64 `json:"duration_ms" extensions:"x-nullable"`
	// Domain is a web page's host without "www."; null for other kinds.
	Domain *string `json:"domain" extensions:"x-nullable"`
	// Reading is the campaign lens the idea was read in; null for the
	// workspace lens.
	Reading *IdeaReading `json:"reading" extensions:"x-nullable"`
}

// IdeaReading names the campaign whose analysis lens produced an idea.
type IdeaReading struct {
	CampaignID   string `json:"campaign_id"`
	CampaignName string `json:"campaign_name"`
}

// IdeaAuthor is the member who wrote a manual idea. ID is null once the
// member has been removed; Name is the name captured with the idea.
type IdeaAuthor struct {
	ID   *string `json:"id" extensions:"x-nullable"`
	Name string  `json:"name"`
}

// Idea is one entry in a workspace's shared Ideas backlog. It is
// workspace-wide when CampaignID is nil and attached to that campaign otherwise;
// moving between the two keeps the row, its verdict, and its author.
//
// A "woken" idea (verdict later, RemindAt <= now) is still stored as later —
// the UI derives the woken state at read time and no job rewrites it.
type Idea struct {
	bun.BaseModel `bun:"table:ideas,alias:i" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID         string       `bun:"id,pk"                                        json:"id"`
	Title      string       `bun:"title,notnull"                                json:"title"`
	Note       string       `bun:"note,notnull,default:''"                      json:"note"`
	CampaignID *string      `bun:"campaign_id"                                  json:"campaign_id"`
	Verdict    *IdeaVerdict `bun:"verdict"                                      json:"verdict"      swaggertype:"string" enums:"yes,later,no" extensions:"x-nullable"`
	RemindAt   *time.Time   `bun:"remind_at"                                    json:"remind_at"`
	DecidedAt  *time.Time   `bun:"decided_at"                                   json:"decided_at"`
	DecidedBy  *string      `bun:"decided_by"                                   json:"decided_by"`
	// CreatedBy is stamped from the session on insert and never changes. It goes
	// null only when the author's user row is hard-deleted; CreatedByName is the
	// capture-time snapshot that keeps the author after that.
	// Origin and AssetAnalysisID trace an analysis idea back to its source.
	// AssetAnalysisID goes null when that analysis is removed; a triaged idea
	// outlives it.
	Origin          IdeaOrigin `bun:"origin,notnull,default:''"                    json:"origin"            swaggertype:"string" enums:",asset_analysis,content_plan,assistant"`
	AssetAnalysisID *string    `bun:"asset_analysis_id"                            json:"asset_analysis_id" extensions:"x-nullable"`
	// DistillRunID names the distill run an idea came from; nil for the
	// analysis's own batch. Re-running the analysis keeps these ideas.
	DistillRunID *string `bun:"distill_run_id" json:"distill_run_id" extensions:"x-nullable"`
	// FitScore is how well an analysis idea fits the lens it was made in,
	// 10–100, computed from FitBreakdown with the weights of FitRubricVersion.
	// It is set once, at generation: moving or editing the idea keeps it, and
	// FitBreakdown names the lens that scored it. All three are nil for manual
	// ideas and for ideas analysed before scoring existed; FitScore alone is
	// nil when too few criteria were rated.
	FitScore         *int               `bun:"fit_score"                        json:"fit_score"          extensions:"x-nullable"`
	FitBreakdown     *ideafit.Breakdown `bun:"fit_breakdown,type:jsonb"         json:"fit_breakdown"      extensions:"x-nullable"`
	FitRubricVersion *int               `bun:"fit_rubric_version"               json:"fit_rubric_version" extensions:"x-nullable"`
	// SourceAssetID is the asset an analysis idea came from. It outlives the
	// analysis and goes nil only when the asset is deleted.
	SourceAssetID *string `bun:"source_asset_id" json:"-"`
	// MergedAt is when other ideas were last merged into this one. A merged
	// idea is kept by the clean-ups that delete untriaged analysis ideas.
	MergedAt *time.Time `bun:"merged_at" json:"-"`
	// Angle, Rationale and Sources are the parts of an analysis idea that Note
	// also carries as text: nil on manual ideas, and on older analysis ideas
	// whose note did not split cleanly.
	Angle     *string  `bun:"angle"         json:"angle"     extensions:"x-nullable"`
	Rationale *string  `bun:"rationale"     json:"rationale" extensions:"x-nullable"`
	Sources   []string `bun:"sources,array" json:"sources"   extensions:"x-nullable"`
	// Plan is where the idea would land as a post; every field nil for an
	// idea without one. The columns are plan_platform_id, plan_post_type,
	// plan_phase_id and plan_target_date.
	Plan IdeaPlan `bun:"embed:plan_" json:"plan"`
	// GroundingMode says whether a generated idea drew on the campaign's
	// sources; nil for ideas people wrote and for analysis ideas, whose
	// source is SourceAsset.
	GroundingMode *GroundingMode `bun:"grounding_mode" json:"grounding_mode" swaggertype:"string" enums:"grounded,exploratory" extensions:"x-nullable"`
	// AssetIDs are the assets a generated idea drew on, in order; empty
	// for the others.
	AssetIDs []string `bun:"asset_ids,array,notnull,default:'{}'" json:"asset_ids"`
	// SourceAsset is hydrated on reads while the asset exists.
	SourceAsset *IdeaSourceAsset `bun:"-" json:"source_asset,omitempty"`
	// Author is derived on reads: the writer of a manual idea, nil for an
	// analysis idea.
	Author *IdeaAuthor `bun:"-" json:"author" extensions:"x-nullable"`
	// PostCount is read back: how many posts were made from the idea.
	PostCount int `bun:"-" json:"post_count"`
	// GroupKey is the group a grouped list lists the idea under.
	GroupKey      string    `bun:"-"                                            json:"group_key,omitempty"`
	CreatedBy     *string   `bun:"created_by"                                   json:"created_by"`
	CreatedByName string    `bun:"created_by_name,notnull"                      json:"created_by_name"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
}

// DeriveAuthor sets Author from the stored author columns.
func (i *Idea) DeriveAuthor() {
	i.Author = nil
	if i.Origin == IdeaOriginManual {
		i.Author = &IdeaAuthor{ID: i.CreatedBy, Name: i.CreatedByName}
	}
}
