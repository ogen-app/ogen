package models

import "time"

// Piles of the ideas list. They partition every idea: waiting holds the
// undecided ones and the later ones whose reminder has passed, later only
// those still snoozed.
const (
	IdeaPileWaiting = "waiting"
	IdeaPileYes     = "yes"
	IdeaPileLater   = "later"
	IdeaPileNo      = "no"
)

// IdeaPiles lists the piles in tab order.
var IdeaPiles = []string{IdeaPileWaiting, IdeaPileYes, IdeaPileLater, IdeaPileNo}

// Values of the origin filter, facet and grouping.
const (
	IdeaOriginKeyAnalysis = "asset_analysis"
	IdeaOriginKeyManual   = "manual"
)

// IdeaOriginKeys lists the origin values in facet order.
var IdeaOriginKeys = []string{IdeaOriginKeyAnalysis, IdeaOriginKeyManual}

// Fit bands of the fit filter, facet and grouping, over the 10–100 fit score.
const (
	IdeaFitHigh     = "high"
	IdeaFitMedium   = "medium"
	IdeaFitLow      = "low"
	IdeaFitUnscored = "unscored"
)

// IdeaFitBands lists the fit bands, best first.
var IdeaFitBands = []string{IdeaFitHigh, IdeaFitMedium, IdeaFitLow, IdeaFitUnscored}

// Lower bounds of the high and medium fit bands; low is the rest.
const (
	IdeaFitHighMin   = 70
	IdeaFitMediumMin = 40
)

// Sort orders of the ideas list.
const (
	// IdeaSortCreated lists the oldest first.
	IdeaSortCreated = "created"
	// IdeaSortCreatedDesc lists the newest first.
	IdeaSortCreatedDesc = "created_desc"
	// IdeaSortFit lists the highest fit score first, unscored ideas last,
	// oldest first among equals.
	IdeaSortFit = "fit"
	// IdeaSortWaiting lists by when an idea started waiting, oldest first: its
	// reminder for a later idea whose reminder has passed, else its creation.
	IdeaSortWaiting = "waiting"
)

// IdeaSorts lists every sort order.
var IdeaSorts = []string{IdeaSortCreated, IdeaSortCreatedDesc, IdeaSortFit, IdeaSortWaiting}

// Fields the ideas list can count values of; each is also its filter's
// parameter name.
const (
	IdeaFacetOrigin      = "origin"
	IdeaFacetSourceAsset = "source_asset"
	IdeaFacetKind        = "kind"
	IdeaFacetCampaign    = "campaign_id"
	IdeaFacetFit         = "fit"
	IdeaFacetPhase       = "phase"
	IdeaFacetAuthor      = "author"
	IdeaFacetCreated     = "created"
)

// IdeaFacets lists every facet field.
var IdeaFacets = []string{
	IdeaFacetOrigin, IdeaFacetSourceAsset, IdeaFacetKind, IdeaFacetCampaign,
	IdeaFacetFit, IdeaFacetPhase, IdeaFacetAuthor, IdeaFacetCreated,
}

// Fields the ideas list can group its rows by.
const (
	IdeaGroupSource   = "source"
	IdeaGroupCampaign = "campaign"
	IdeaGroupFit      = "fit"
	IdeaGroupOrigin   = "origin"
	IdeaGroupAuthor   = "author"
	IdeaGroupCreated  = "created"
)

// IdeaGroups lists every grouping.
var IdeaGroups = []string{
	IdeaGroupSource, IdeaGroupCampaign, IdeaGroupFit, IdeaGroupOrigin, IdeaGroupAuthor, IdeaGroupCreated,
}

// IdeaQuery is one read of the ideas list. Different fields combine with AND.
type IdeaQuery struct {
	Pile        AssetValueFilter
	Origin      AssetValueFilter
	SourceAsset AssetValueFilter
	Kind        AssetValueFilter
	Campaign    AssetValueFilter
	Fit         AssetValueFilter
	Phase       AssetValueFilter
	Author      AssetValueFilter
	// Text keeps the ideas whose title or note contains it, ignoring case.
	Text string
	// CreatedAfter is inclusive, CreatedBefore exclusive.
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	// ScopeCampaign confines the whole read, totals and counts included, to
	// one campaign's ideas.
	ScopeCampaign string
	Sort          string
	// Group orders the rows by one of IdeaGroups first, then by Sort within
	// each group; "" does not group.
	Group string
	Limit int
	// Cursor continues a previous page of the same sort and grouping.
	Cursor string
	// Facets names the fields to count values of.
	Facets []string
	// Now is the clock the pile rule and the date presets read.
	Now time.Time
	// Location is the viewer's time zone for the date presets.
	Location *time.Location
}

// IdeaFacetAsset labels one value of the source_asset facet.
type IdeaFacetAsset struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	Kind  AssetKind `json:"kind" swaggertype:"string" enums:"pdf,web,audio,note,image,document,spreadsheet,csv,presentation"`
}

// IdeaQueryResult is one page of the ideas list.
type IdeaQueryResult struct {
	Ideas []Idea
	// NextCursor continues the list; "" on the last page.
	NextCursor string
	// Total counts the ideas matching every filter; TotalUnfiltered every
	// idea in scope.
	Total           int
	TotalUnfiltered int
	// Piles counts the match per pile, ignoring the pile filter.
	Piles map[string]int
	// Facets holds the counts of the requested fields. Each field's counts
	// apply every other field's filter but not its own.
	Facets map[string][]AssetFacetCount
	// SourceAssetLabels labels every asset of the source_asset facet; set only
	// when that facet is requested.
	SourceAssetLabels []IdeaFacetAsset
	// Groups lists, when the query groups, every non-empty group of the whole
	// match in display order.
	Groups []AssetGroupCount
}

// IdeaViewCounts is what a saved view of the ideas list counts: its match,
// and the waiting ideas in it.
type IdeaViewCounts struct {
	Total   int `json:"total"`
	Waiting int `json:"waiting"`
}
