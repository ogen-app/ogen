package models

import (
	"time"
)

// AssetKind is what an asset is to a person browsing the bank, derived from its
// coarse type and, for documents, the MIME type of its file. The server owns
// this classification so the client never maps MIME types.
type AssetKind string

const (
	AssetKindPDF          AssetKind = "pdf"
	AssetKindWeb          AssetKind = "web"
	AssetKindAudio        AssetKind = "audio"
	AssetKindNote         AssetKind = "note"
	AssetKindImage        AssetKind = "image"
	AssetKindDocument     AssetKind = "document"
	AssetKindSpreadsheet  AssetKind = "spreadsheet"
	AssetKindCSV          AssetKind = "csv"
	AssetKindPresentation AssetKind = "presentation"
)

// AssetKinds lists every kind, in the order the kind facet reports them.
var AssetKinds = []string{
	string(AssetKindPDF), string(AssetKindWeb), string(AssetKindAudio), string(AssetKindNote),
	string(AssetKindImage), string(AssetKindDocument), string(AssetKindSpreadsheet),
	string(AssetKindCSV), string(AssetKindPresentation),
}

// AssetListStatuses are the statuses the list filters and counts by. An asset
// still processing counts as pending.
var AssetListStatuses = []string{AssetStatusReady, AssetStatusPartial, AssetStatusPending, AssetStatusFailed}

// AssetFilterNone is the filter value matching assets with no value for a
// field: in no campaign, untagged.
const AssetFilterNone = "none"

// Values of the ideas filter. The first four match an asset with at least one
// analysis idea of that verdict, a later idea whose reminder has passed counting
// as undecided; none matches an analysed asset with no ideas and unread one
// with no finished analysis.
const (
	AssetIdeasUndecided = "undecided"
	AssetIdeasYes       = "yes"
	AssetIdeasLater     = "later"
	AssetIdeasNo        = "no"
	AssetIdeasNone      = "none"
	AssetIdeasUnread    = "unread"
)

// AssetIdeasValues lists every ideas filter value, in facet order.
var AssetIdeasValues = []string{
	AssetIdeasUndecided, AssetIdeasYes, AssetIdeasLater, AssetIdeasNo, AssetIdeasNone, AssetIdeasUnread,
}

// Sort orders of the asset list. The leading "-" means descending.
const (
	AssetSortUpdatedDesc    = "-updated_at"
	AssetSortUpdatedAsc     = "updated_at"
	AssetSortCreatedDesc    = "-created_at"
	AssetSortCreatedAsc     = "created_at"
	AssetSortTitleAsc       = "title"
	AssetSortTitleDesc      = "-title"
	AssetSortIdeasUndecided = "-ideas_undecided"
	// AssetSortRelevance puts the assets with the most matching chunks first;
	// it needs a text query.
	AssetSortRelevance = "relevance"
)

// AssetSorts lists every sort order.
var AssetSorts = []string{
	AssetSortUpdatedDesc, AssetSortUpdatedAsc, AssetSortCreatedDesc, AssetSortCreatedAsc,
	AssetSortTitleAsc, AssetSortTitleDesc, AssetSortIdeasUndecided, AssetSortRelevance,
}

// Fields the asset list can count values of.
const (
	AssetFacetKind     = "kind"
	AssetFacetStatus   = "status"
	AssetFacetCampaign = "campaign"
	AssetFacetTag      = "tag"
	AssetFacetIdeas    = "ideas"
	AssetFacetUpdated  = "updated"
	AssetFacetCreated  = "created"
)

// AssetFacets lists every facet field.
var AssetFacets = []string{
	AssetFacetKind, AssetFacetStatus, AssetFacetCampaign, AssetFacetTag, AssetFacetIdeas, AssetFacetUpdated,
	AssetFacetCreated,
}

// Presets of the updated and created filters and facets: since the start of
// today, of the day six or twenty-nine days ago, and of the month, in the
// viewer's time zone.
const (
	AssetDateToday = "1d"
	AssetDateWeek  = "7d"
	AssetDate30d   = "30d"
	AssetDateMonth = "month"
)

// AssetDatePresets lists the values of the updated and created facets.
var AssetDatePresets = []string{AssetDateToday, AssetDateWeek, AssetDate30d, AssetDateMonth}

// AssetDateSince is the start of a date preset for a viewer whose clock reads
// now in loc. ok is false for an unknown preset.
func AssetDateSince(preset string, now time.Time, loc *time.Location) (since time.Time, ok bool) {
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	switch preset {
	case AssetDateToday:
		return today, true
	case AssetDateWeek:
		return today.AddDate(0, 0, -6), true
	case AssetDate30d:
		return today.AddDate(0, 0, -29), true
	case AssetDateMonth:
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc), true
	}
	return time.Time{}, false
}

// Fields the asset list can group its rows by.
const (
	AssetGroupIdeas    = "ideas"
	AssetGroupCampaign = "campaign"
	AssetGroupTag      = "tag"
	AssetGroupKind     = "kind"
	AssetGroupCreated  = "created"
)

// AssetGroups lists every grouping.
var AssetGroups = []string{AssetGroupIdeas, AssetGroupCampaign, AssetGroupTag, AssetGroupKind, AssetGroupCreated}

// Keys of the ideas grouping, in display order. They partition the assets: an
// asset with an undecided idea, one with ideas but none undecided, and every
// other one, analysed without ideas or not read yet.
const (
	AssetIdeasGroupUndecided = "undecided"
	AssetIdeasGroupAnswered  = "answered"
	AssetIdeasGroupNone      = "none"
)

// AssetIdeasGroupKeys lists the ideas grouping's keys in display order.
var AssetIdeasGroupKeys = []string{AssetIdeasGroupUndecided, AssetIdeasGroupAnswered, AssetIdeasGroupNone}

// AssetCreatedEarlier is the created grouping's band before the 30d preset.
// The other bands are disjoint slices of the presets: 1d is today, 7d the six
// days before it, 30d the 23 days before those.
const AssetCreatedEarlier = "earlier"

// AssetCreatedGroupKeys lists the created grouping's keys in display order.
var AssetCreatedGroupKeys = []string{AssetDateToday, AssetDateWeek, AssetDate30d, AssetCreatedEarlier}

// Machine-readable codes on the 400s of the asset list.
const (
	AssetCodeInvalidFilter = "invalid_filter"
	AssetCodeInvalidCursor = "invalid_cursor"
	AssetCodeQueryTooShort = "query_too_short"
	AssetCodeQueryTooLong  = "query_too_long"
)

// AssetValueFilter is one field's filter: an asset matches when its value is
// any of Any (or Any is empty) and none of Not.
type AssetValueFilter struct {
	Any []string
	Not []string
}

// Active reports whether the filter narrows anything.
func (f AssetValueFilter) Active() bool { return len(f.Any) > 0 || len(f.Not) > 0 }

// AssetQuery is one read of the asset list. Different fields combine with AND.
type AssetQuery struct {
	// Title holds case-insensitive substrings of the title.
	Title    AssetValueFilter
	Kind     AssetValueFilter
	Status   AssetValueFilter
	Campaign AssetValueFilter
	Tag      AssetValueFilter
	Ideas    AssetValueFilter
	// Checksums keeps the assets whose file has one of these SHA-256 sums.
	Checksums []string
	// UpdatedAfter and CreatedAfter are inclusive, UpdatedBefore and
	// CreatedBefore exclusive.
	UpdatedAfter  *time.Time
	UpdatedBefore *time.Time
	CreatedAfter  *time.Time
	CreatedBefore *time.Time
	// Text keeps the assets whose chunks mention it (websearch syntax).
	Text string
	// ScopeCampaign confines the whole read, totals and counts included, to
	// one campaign's assets.
	ScopeCampaign string
	Sort          string
	Limit         int
	// Group orders the rows by one of AssetGroups first, then by Sort within
	// each group; "" does not group.
	Group string
	// Cursor continues a previous page of the same sort and grouping.
	Cursor string
	// Facets names the fields to count values of.
	Facets []string
	// Now is the clock the reminder rule and the date presets read.
	Now time.Time
	// Location is the viewer's time zone for the date presets.
	Location *time.Location
}

// AssetIdeaCounts counts an asset's analysis ideas per verdict. A later idea
// whose reminder has passed counts as undecided, not later.
type AssetIdeaCounts struct {
	Undecided int `json:"undecided"`
	Yes       int `json:"yes"`
	Later     int `json:"later"`
	No        int `json:"no"`
}

// AssetNextIdea is the idea an asset's row names: the oldest one counted in
// its undecided ideas.
type AssetNextIdea struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// AssetTextMatch is where an asset mentions the text of a query.
type AssetTextMatch struct {
	// Matches is the number of the asset's chunks that match.
	Matches int `json:"matches"`
	// Snippet is an excerpt of the best chunk around the match, with HTML tags
	// in the source dropped. It is HTML-escaped, with each matched word wrapped
	// in <mark>…</mark> (the only markup it contains), and starts or ends with
	// "…" when it was cut from a longer chunk.
	Snippet string `json:"snippet"`
	// SourceLabel cites where the best chunk sits in its source ("Slide 4",
	// "1:05–1:40", "Page 14"). Null when the chunk has no location, as for
	// notes and web pages.
	SourceLabel *string `json:"source_label" extensions:"x-nullable"`
	// ChunkIndex is the best chunk's position in the asset.
	ChunkIndex int `json:"chunk_index"`
}

// AssetListFile is the part of an asset's file the list shows.
type AssetListFile struct {
	MimeType  string `json:"mime_type"`
	PageCount *int   `json:"page_count"  extensions:"x-nullable"`
	// DurationMS is a recording's length; 0 when unknown or not a recording.
	DurationMS     int64   `json:"duration_ms"`
	ChecksumSHA256 string  `json:"checksum_sha256,omitempty"`
	ThumbnailURL   *string `json:"thumbnail_url" extensions:"x-nullable"`
	URL            *string `json:"url"           extensions:"x-nullable"`

	S3Key          string  `json:"-"`
	ThumbnailS3Key *string `json:"-"`
}

// AssetListItem is one row of the asset list: the asset without its content.
type AssetListItem struct {
	ID    string    `json:"id"`
	Title string    `json:"title"`
	Type  *string   `json:"type"  extensions:"x-nullable"`
	Kind  AssetKind `json:"kind"  swaggertype:"string" enums:"pdf,web,audio,note,image,document,spreadsheet,csv,presentation"`
	// Status is the stored status; the status filter counts processing as
	// pending.
	Status      string    `json:"status" enums:"pending,processing,ready,partial,failed"`
	FailureCode string    `json:"failure_code"`
	SourceURL   *string   `json:"source_url" extensions:"x-nullable"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	// AnalysisStatus is the workspace analysis lens's status.
	AnalysisStatus *AssetAnalysisStatus `json:"analysis_status" swaggertype:"string" enums:"pending,running,ready,failed,skipped" extensions:"x-nullable"`
	File           *AssetListFile       `json:"file"            extensions:"x-nullable"`
	Tags           []Tag                `json:"tags"`
	// CampaignIDs are the live campaigns the asset serves, including those
	// that draw on the whole bank.
	CampaignIDs []string        `json:"campaign_ids"`
	IdeaCounts  AssetIdeaCounts `json:"idea_counts"`
	// NextIdea is the oldest of the ideas IdeaCounts.Undecided counts; null
	// exactly when that count is 0.
	NextIdea *AssetNextIdea `json:"next_idea" extensions:"x-nullable"`
	// PublishedAt is when the source was published, read from its metadata at
	// ingest; null when unknown.
	PublishedAt *time.Time `json:"published_at" extensions:"x-nullable"`
	// Gist is one sentence of the workspace analysis; null unless that lens is
	// ready and has a gist or a usable summary sentence.
	Gist *string `json:"gist" extensions:"x-nullable"`
	// Match is present only when the query has text.
	Match *AssetTextMatch `json:"match,omitempty"`
	// GroupKey is the group the row is listed under, present only when the
	// query groups: a tag or campaign id, a kind, an ideas or created key, or
	// none.
	GroupKey string `json:"group_key,omitempty"`
}

// AssetFacetCount is how many assets have one value of a field.
type AssetFacetCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// AssetGroupCount is how many rows of the whole match one group lists.
type AssetGroupCount struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// AssetQueryResult is one page of the asset list.
type AssetQueryResult struct {
	Assets []AssetListItem
	// NextCursor continues the list; "" on the last page.
	NextCursor string
	// Total counts the assets matching every filter; TotalUnfiltered every
	// asset in scope.
	Total           int
	TotalUnfiltered int
	// IdeasUndecided sums the undecided ideas of the assets Total counts.
	IdeasUndecided int
	// Facets holds the counts of the requested fields. Each field's counts
	// apply every other field's filter but not its own.
	Facets map[string][]AssetFacetCount
	// Groups lists, when the query groups, every non-empty group of the whole
	// match in display order. A tag grouping lists an asset once per tag, so
	// its counts may add up to more than Total.
	Groups []AssetGroupCount
}
