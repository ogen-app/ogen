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
)

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
	Origin          IdeaOrigin `bun:"origin,notnull,default:''"                    json:"origin"            swaggertype:"string" enums:",asset_analysis"`
	AssetAnalysisID *string    `bun:"asset_analysis_id"                            json:"asset_analysis_id" extensions:"x-nullable"`
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
	// Angle, Rationale and Sources are the parts of an analysis idea that Note
	// also carries as text: nil on manual ideas, and on older analysis ideas
	// whose note did not split cleanly.
	Angle     *string  `bun:"angle"         json:"angle"     extensions:"x-nullable"`
	Rationale *string  `bun:"rationale"     json:"rationale" extensions:"x-nullable"`
	Sources   []string `bun:"sources,array" json:"sources"   extensions:"x-nullable"`
	// SourceAsset is hydrated on reads while the asset exists.
	SourceAsset *IdeaSourceAsset `bun:"-" json:"source_asset,omitempty"`
	// Author is derived on reads: the writer of a manual idea, nil for an
	// analysis idea.
	Author *IdeaAuthor `bun:"-" json:"author" extensions:"x-nullable"`
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
