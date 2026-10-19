package handlers

import (
	"database/sql"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/assets"
)

// assetListResponse is one page of the asset list. Total counts the assets
// matching every filter and TotalUnfiltered every asset in scope ("24 of 41").
type assetListResponse struct {
	Assets []models.AssetListItem `json:"assets"`
	// NextCursor continues the list in the same sort; null on the last page.
	NextCursor      *string `json:"next_cursor" extensions:"x-nullable"`
	Total           int     `json:"total"`
	TotalUnfiltered int     `json:"total_unfiltered"`
	// IdeasUndecidedTotal sums idea_counts.undecided over every asset that
	// total counts, not just this page.
	IdeasUndecidedTotal int `json:"ideas_undecided_total"`
	// Facets holds, per requested field, every value with the number of
	// assets that have it.
	Facets map[string][]models.AssetFacetCount `json:"facets"`
	// Groups lists, with group, every non-empty group of the whole match with
	// its row count, in display order; empty without group.
	Groups []models.AssetGroupCount `json:"groups"`
	// IDs is present only with ids_only=true, and then it is the whole body
	// (an assetIDsResponse): every matching id in sort order, unpaginated.
	// Swagger allows one schema per status, so the list schema carries it.
	IDs []string `json:"ids,omitempty"`
}

// assetIDsResponse is the body with ids_only=true: every asset id matching
// the filters, unpaginated.
type assetIDsResponse struct {
	IDs []string `json:"ids"`
}

// List godoc
// @Summary      List assets
// @Description  Filters, counts and pages the workspace's assets, without their content (GET /:id has it).
// @Description
// @Description  Every filter takes a comma-separated list meaning "any of these"; its `_not` twin excludes values. Different fields combine with AND. `campaign` and `tag` take ids plus `none` (in no campaign, untagged). An asset belongs to every live campaign listing it and to every campaign that draws on the whole bank. `ideas` matches an asset with at least one analysis idea of that verdict (`undecided`, `yes`, `later`, `no`; a `later` idea whose reminder has passed counts as `undecided`), `none` an analysed asset without ideas, `unread` one with no finished analysis. `status=pending` includes assets still processing. `updated_after` and `created_after` are inclusive, `updated_before` and `created_before` exclusive; each takes an RFC 3339 instant or a `YYYY-MM-DD` day, which means the start of that day in `tz`.
// @Description
// @Description  `text` searches inside the assets (full-text with English stemming; "quoted phrases", `-word` and `or` work) and adds `match` to each row: the number of matching chunks and the best one's `snippet` (HTML-escaped, matched words in `<mark>`), `source_label` and `chunk_index`.
// @Description
// @Description  `scope_campaign` confines the whole read, totals and counts included, to one campaign's assets; the campaign facet is not returned there. `facets` names the fields to count: each field's counts apply every other filter but not its own, and list every value, with 0 for those nothing matches. The date presets of `updated` and `created`, as filters and facets, are since the start of today (`1d`), of the day 6 or 29 days ago (`7d`, `30d`) and of the month (`month`), in `tz`.
// @Description
// @Description  Ideas and readings come from one analysis lens: the workspace lens in the bank, and under `scope_campaign` that campaign's lens alone. `idea_counts`, `next_idea`, `ideas_undecided_total`, the `ideas` filter, facet, sort and grouping, `gist` and `analysis_status` all read it, so on a campaign page an asset whose campaign lens has not run reads as not analysed (`unread`, no gist, no status) even when its workspace lens is ready.
// @Description
// @Description  Every row carries `next_idea`: the highest `fit_score` of the ideas `idea_counts.undecided` counts (`{id, title}`), oldest first among equal or missing scores, null exactly when that count is 0. `ideas_undecided_total` sums `idea_counts.undecided` over every matching asset, not just the page.
// @Description
// @Description  `group` orders the rows by group first, then by `sort` within each group, and adds `group_key` to every row and `groups` (each non-empty group's key and row count over the whole match, in display order). Paging runs across groups, so a group may span pages. `ideas` groups are `undecided`, `answered` (ideas, none undecided) and `none`; `created` groups are the disjoint bands `1d` (today), `7d` (the 6 days before), `30d` (the 23 days before those) and `earlier`; `kind` groups follow the kind order; `campaign` and `tag` groups go by name with `none` last, and list an asset under each of its campaigns or tags that the field's filter allows, so `groups` counts appearances while `total` counts assets.
// @Description
// @Description  `ids_only=true` returns `{ids}`: every matching id in sort order, unpaginated.
// @Tags         content-bank
// @Produce      json
// @Security     CookieAuth
// @Param        title           query     string  false  "Case-insensitive title substrings"
// @Param        title_not       query     string  false  "Title substrings to exclude"
// @Param        kind            query     string  false  "pdf, web, audio, note, image, document, spreadsheet, csv, presentation"
// @Param        kind_not        query     string  false  "Kinds to exclude"
// @Param        status          query     string  false  "ready, partial, pending, failed"
// @Param        status_not      query     string  false  "Statuses to exclude"
// @Param        campaign        query     string  false  "Campaign ids, or none"
// @Param        campaign_not    query     string  false  "Campaign ids (or none) to exclude"
// @Param        tag             query     string  false  "Tag ids, or none"
// @Param        tag_not         query     string  false  "Tag ids (or none) to exclude"
// @Param        ideas           query     string  false  "undecided, yes, later, no, none, unread"
// @Param        ideas_not       query     string  false  "Ideas values to exclude"
// @Param        updated_after   query     string  false  "RFC 3339, or a YYYY-MM-DD day starting in tz; inclusive"
// @Param        updated_before  query     string  false  "RFC 3339, or a YYYY-MM-DD day starting in tz; exclusive"
// @Param        updated         query     string  false  "Preset start of the updated window: 1d, 7d, 30d, month (not with updated_after)"
// @Param        created_after   query     string  false  "RFC 3339, or a YYYY-MM-DD day starting in tz; inclusive"
// @Param        created_before  query     string  false  "RFC 3339, or a YYYY-MM-DD day starting in tz; exclusive"
// @Param        created         query     string  false  "Preset start of the created window: 1d, 7d, 30d, month (not with created_after)"
// @Param        checksum        query     string  false  "SHA-256 sums of asset files"
// @Param        text            query     string  false  "Text to find inside the assets (2–200 characters)"
// @Param        scope_campaign  query     string  false  "Campaign id to confine the read to"
// @Param        sort            query     string  false  "-updated_at (default), updated_at, -created_at, created_at, title, -title, -ideas_undecided, relevance (needs text)"
// @Param        limit           query     int     false  "Page size (default 50, max 200)"
// @Param        cursor          query     string  false  "next_cursor of the previous page"
// @Param        facets          query     string  false  "kind, status, campaign, tag, ideas, updated, created"
// @Param        group           query     string  false  "ideas, campaign, tag, kind, created"
// @Param        tz              query     string  false  "IANA time zone of the date presets, day bounds and created groups (default UTC)"
// @Param        ids_only        query     bool    false  "Return every matching id instead of a page"
// @Success      200  {object}  assetListResponse  "With ids_only=true only ids is present"
// @Failure      400  {object}  map[string]string  "code: invalid_filter | invalid_cursor | query_too_short | query_too_long"
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string  "scope_campaign names no live campaign"
// @Router       /api/content-bank/assets [get]
func (h *AssetsHandler) List(c *fiber.Ctx) error {
	ctx := reqCtx(c)
	v := queryValuesOf(c)
	q, err := assets.ParseQuery(v, time.Now().UTC())
	if err != nil {
		return listQueryFailure(c, err)
	}
	idsOnly, err := assets.IDsOnly(v)
	if err != nil {
		return listQueryFailure(c, err)
	}
	if idsOnly {
		ids, err := h.repo.QueryIDs(ctx, q)
		if err != nil {
			return assetQueryFailure(c, err)
		}
		return c.JSON(assetIDsResponse{IDs: ids})
	}
	res, err := h.repo.Query(ctx, q)
	if err != nil {
		return assetQueryFailure(c, err)
	}
	for i := range res.Assets {
		if f := res.Assets[i].File; f != nil && h.storage != nil {
			f.URL = h.fileURL(ctx, f.S3Key)
			if f.ThumbnailS3Key != nil {
				f.ThumbnailURL = h.fileURL(ctx, *f.ThumbnailS3Key)
			}
		}
	}
	resp := assetListResponse{
		Assets: res.Assets, Total: res.Total, TotalUnfiltered: res.TotalUnfiltered,
		IdeasUndecidedTotal: res.IdeasUndecided, Facets: res.Facets, Groups: res.Groups,
	}
	if resp.Facets == nil {
		resp.Facets = map[string][]models.AssetFacetCount{}
	}
	if resp.Groups == nil {
		resp.Groups = []models.AssetGroupCount{}
	}
	if res.NextCursor != "" {
		resp.NextCursor = &res.NextCursor
	}
	return c.JSON(resp)
}

// assetQueryFailure maps a rejected cursor to its 400 and a campaign scope
// naming no live campaign to a 404.
func assetQueryFailure(c *fiber.Ctx, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	return listQueryFailure(c, err)
}
