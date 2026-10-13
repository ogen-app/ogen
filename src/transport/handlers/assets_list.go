package handlers

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

const (
	defaultAssetPageSize = 50
	maxAssetPageSize     = 200
	minAssetTextLen      = 2
	maxAssetTextLen      = 200
)

// assetListResponse is one page of the asset list. Total counts the assets
// matching every filter and TotalUnfiltered every asset in scope ("24 of 41").
type assetListResponse struct {
	Assets []models.AssetListItem `json:"assets"`
	// NextCursor continues the list in the same sort; null on the last page.
	NextCursor      *string `json:"next_cursor" extensions:"x-nullable"`
	Total           int     `json:"total"`
	TotalUnfiltered int     `json:"total_unfiltered"`
	// Facets holds, per requested field, every value with the number of
	// assets that have it.
	Facets map[string][]models.AssetFacetCount `json:"facets"`
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

// assetQueryError is a rejected list parameter.
type assetQueryError struct {
	code, msg string
}

func (e *assetQueryError) Error() string { return e.msg }

func invalidFilter(format string, args ...any) error {
	return &assetQueryError{models.AssetCodeInvalidFilter, fmt.Sprintf(format, args...)}
}

// queryValues returns a parameter's values: every occurrence split on commas,
// trimmed, without blanks or duplicates.
func queryValues(c *fiber.Ctx, key string) []string {
	var out []string
	for _, raw := range c.Context().QueryArgs().PeekMulti(key) {
		for v := range strings.SplitSeq(string(raw), ",") {
			if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// valueFilter reads key and key_not, rejecting values outside allowed when it
// is non-nil.
func valueFilter(c *fiber.Ctx, key string, allowed []string) (models.AssetValueFilter, error) {
	f := models.AssetValueFilter{Any: queryValues(c, key), Not: queryValues(c, key+"_not")}
	if allowed != nil {
		for _, v := range slices.Concat(f.Any, f.Not) {
			if !slices.Contains(allowed, v) {
				return f, invalidFilter("%s: unknown value %q (allowed: %s)", key, v, strings.Join(allowed, ", "))
			}
		}
	}
	return f, nil
}

func timeParam(c *fiber.Ctx, key string) (*time.Time, error) {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, invalidFilter("%s must be an RFC 3339 timestamp", key)
	}
	return &t, nil
}

// parseAssetQuery reads the list parameters. idsOnly reports ids_only=true.
func parseAssetQuery(c *fiber.Ctx, now time.Time) (q models.AssetQuery, idsOnly bool, err error) {
	q = models.AssetQuery{Now: now, Location: time.UTC, Sort: models.AssetSortUpdatedDesc, Limit: defaultAssetPageSize}
	if err := parseAssetFilters(c, &q); err != nil {
		return q, false, err
	}
	if err := parseAssetPaging(c, &q); err != nil {
		return q, false, err
	}
	if raw := strings.TrimSpace(c.Query("ids_only")); raw != "" {
		if idsOnly, err = strconv.ParseBool(raw); err != nil {
			return q, false, invalidFilter("ids_only must be true or false")
		}
	}
	return q, idsOnly, nil
}

// parseAssetFilters reads the parameters that choose which assets match.
func parseAssetFilters(c *fiber.Ctx, q *models.AssetQuery) (err error) {
	q.Title = models.AssetValueFilter{Any: queryValues(c, "title"), Not: queryValues(c, "title_not")}
	if q.Kind, err = valueFilter(c, "kind", models.AssetKinds); err != nil {
		return err
	}
	if q.Status, err = valueFilter(c, "status", models.AssetListStatuses); err != nil {
		return err
	}
	if q.Campaign, err = valueFilter(c, "campaign", nil); err != nil {
		return err
	}
	if q.Tag, err = valueFilter(c, "tag", nil); err != nil {
		return err
	}
	if q.Ideas, err = valueFilter(c, "ideas", models.AssetIdeasValues); err != nil {
		return err
	}
	for _, sum := range queryValues(c, "checksum") {
		sum = strings.ToLower(sum)
		if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
			return invalidFilter("checksum: %q is not a hex SHA-256", sum)
		}
		q.Checksums = append(q.Checksums, sum)
	}
	if q.UpdatedAfter, err = timeParam(c, "updated_after"); err != nil {
		return err
	}
	if q.UpdatedBefore, err = timeParam(c, "updated_before"); err != nil {
		return err
	}
	q.Text = strings.TrimSpace(c.Query("text"))
	switch n := utf8.RuneCountInString(q.Text); {
	case n > 0 && n < minAssetTextLen:
		return &assetQueryError{models.AssetCodeQueryTooShort, fmt.Sprintf("text must be at least %d characters", minAssetTextLen)}
	case n > maxAssetTextLen:
		return &assetQueryError{models.AssetCodeQueryTooLong, fmt.Sprintf("text must be at most %d characters", maxAssetTextLen)}
	}
	q.ScopeCampaign = strings.TrimSpace(c.Query("scope_campaign"))
	return nil
}

// parseAssetPaging reads the sort, page and facet parameters.
func parseAssetPaging(c *fiber.Ctx, q *models.AssetQuery) error {
	if s := strings.TrimSpace(c.Query("sort")); s != "" {
		if !slices.Contains(models.AssetSorts, s) {
			return invalidFilter("sort: unknown value %q (allowed: %s)", s, strings.Join(models.AssetSorts, ", "))
		}
		q.Sort = s
	}
	if q.Sort == models.AssetSortRelevance && q.Text == "" {
		return invalidFilter("sort=relevance needs text")
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxAssetPageSize {
			return invalidFilter("limit must be between 1 and %d", maxAssetPageSize)
		}
		q.Limit = n
	}
	q.Cursor = strings.TrimSpace(c.Query("cursor"))
	q.Facets = queryValues(c, "facets")
	for _, f := range q.Facets {
		if !slices.Contains(models.AssetFacets, f) {
			return invalidFilter("facets: unknown field %q (allowed: %s)", f, strings.Join(models.AssetFacets, ", "))
		}
	}
	if tz := strings.TrimSpace(c.Query("tz")); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return invalidFilter("tz: unknown time zone %q", tz)
		}
		q.Location = loc
	}
	return nil
}

// List godoc
// @Summary      List assets
// @Description  Filters, counts and pages the workspace's assets, without their content (GET /:id has it).
// @Description
// @Description  Every filter takes a comma-separated list meaning "any of these"; its `_not` twin excludes values. Different fields combine with AND. `campaign` and `tag` take ids plus `none` (in no campaign, untagged). An asset belongs to every live campaign listing it and to every campaign that draws on the whole bank. `ideas` matches an asset with at least one analysis idea of that verdict (`undecided`, `yes`, `later`, `no`; a `later` idea whose reminder has passed counts as `undecided`), `none` an analysed asset without ideas, `unread` one with no finished analysis. `status=pending` includes assets still processing. `updated_after` is inclusive, `updated_before` exclusive.
// @Description
// @Description  `text` searches inside the assets (full-text with English stemming; "quoted phrases", `-word` and `or` work) and adds `match` to each row: the number of matching chunks and the best one's `snippet` (HTML-escaped, matched words in `<mark>`), `source_label` and `chunk_index`.
// @Description
// @Description  `scope_campaign` confines the whole read, totals and counts included, to one campaign's assets; the campaign facet is not returned there. `facets` names the fields to count: each field's counts apply every other filter but not its own, and list every value, with 0 for those nothing matches. The `updated` presets are since the start of today (`1d`), of the day 6 or 29 days ago (`7d`, `30d`) and of the month (`month`), in `tz`.
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
// @Param        updated_after   query     string  false  "RFC 3339, inclusive"
// @Param        updated_before  query     string  false  "RFC 3339, exclusive"
// @Param        checksum        query     string  false  "SHA-256 sums of asset files"
// @Param        text            query     string  false  "Text to find inside the assets (2–200 characters)"
// @Param        scope_campaign  query     string  false  "Campaign id to confine the read to"
// @Param        sort            query     string  false  "-updated_at (default), updated_at, title, -title, -ideas_undecided, relevance (needs text)"
// @Param        limit           query     int     false  "Page size (default 50, max 200)"
// @Param        cursor          query     string  false  "next_cursor of the previous page"
// @Param        facets          query     string  false  "kind, status, campaign, tag, ideas, updated"
// @Param        tz              query     string  false  "IANA time zone of the updated presets (default UTC)"
// @Param        ids_only        query     bool    false  "Return every matching id instead of a page"
// @Success      200  {object}  assetListResponse  "With ids_only=true only ids is present"
// @Failure      400  {object}  map[string]string  "code: invalid_filter | invalid_cursor | query_too_short | query_too_long"
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string  "scope_campaign names no live campaign"
// @Router       /api/content-bank/assets [get]
func (h *AssetsHandler) List(c *fiber.Ctx) error {
	ctx := reqCtx(c)
	q, idsOnly, err := parseAssetQuery(c, time.Now().UTC())
	if err != nil {
		if qe, ok := errors.AsType[*assetQueryError](err); ok {
			return rejectCode(c, fiber.StatusBadRequest, qe.code, qe.msg)
		}
		return err
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
		Facets: res.Facets,
	}
	if resp.Facets == nil {
		resp.Facets = map[string][]models.AssetFacetCount{}
	}
	if res.NextCursor != "" {
		resp.NextCursor = &res.NextCursor
	}
	return c.JSON(resp)
}

func assetQueryFailure(c *fiber.Ctx, err error) error {
	switch {
	case errors.Is(err, repository.ErrInvalidAssetCursor):
		return rejectCode(c, fiber.StatusBadRequest, models.AssetCodeInvalidCursor, "cursor is not from this list or this sort")
	case errors.Is(err, sql.ErrNoRows):
		return fiber.NewError(fiber.StatusNotFound, "campaign not found")
	}
	return err
}
