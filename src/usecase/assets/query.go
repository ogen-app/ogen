// Package assets reads the asset list's address: the query string the list
// endpoint serves and a saved view of it stores.
package assets

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
)

// Page sizes and text bounds of the asset list.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
	minTextLen      = 2
	maxTextLen      = 200
)

// valueParams are the asset list's value filters, each with its _not twin.
var valueParams = []string{
	"title", models.AssetFacetKind, models.AssetFacetStatus, models.AssetFacetCampaign,
	models.AssetFacetTag, models.AssetFacetIdeas,
}

// Params lists the parameters of the asset list's address, each with whether
// it takes a comma-separated list. Paging and display parameters are not part
// of the address; see VolatileParams.
var Params = func() map[string]bool {
	params := map[string]bool{
		"text": false, "sort": false, "group": false,
		"updated": false, "updated_after": false, "updated_before": false,
		"created": false, "created_after": false, "created_before": false,
	}
	for _, p := range valueParams {
		params[p], params[p+listquery.NotSuffix] = true, true
	}
	return params
}()

// VolatileParams are the parameters a request carries that are not part of
// what a view shows: paging, the facets asked for, the viewer's time zone, the
// campaign page a request comes from (groups belong to the workspace page) and
// checksum, which looks assets up rather than filtering them.
var VolatileParams = []string{"limit", "cursor", "facets", "tz", "ids_only", "scope_campaign", "checksum"}

// ParseQuery reads the asset list parameters at the clock now. It rejects an
// unknown value with a *listquery.Error; parameters it does not know are
// ignored.
func ParseQuery(v url.Values, now time.Time) (models.AssetQuery, error) {
	q := models.AssetQuery{Now: now, Location: time.UTC, Sort: models.AssetSortUpdatedDesc, Limit: DefaultPageSize}
	var err error
	if q.Location, err = listquery.Location(v); err != nil {
		return q, err
	}
	if err := parseFilters(v, &q); err != nil {
		return q, err
	}
	if err := parsePaging(v, &q); err != nil {
		return q, err
	}
	return q, nil
}

func parseFilters(v url.Values, q *models.AssetQuery) (err error) {
	for _, f := range []struct {
		dst     *models.AssetValueFilter
		key     string
		allowed []string
	}{
		{&q.Title, "title", nil},
		{&q.Kind, models.AssetFacetKind, models.AssetKinds},
		{&q.Status, models.AssetFacetStatus, models.AssetListStatuses},
		{&q.Campaign, models.AssetFacetCampaign, nil},
		{&q.Tag, models.AssetFacetTag, nil},
		{&q.Ideas, models.AssetFacetIdeas, models.AssetIdeasValues},
	} {
		if *f.dst, err = listquery.Filter(v, f.key, f.allowed); err != nil {
			return err
		}
	}
	for _, sum := range listquery.Values(v, "checksum") {
		sum = strings.ToLower(sum)
		if len(sum) != 64 || strings.Trim(sum, "0123456789abcdef") != "" {
			return listquery.Invalid("checksum", "%q is not a hex SHA-256", sum)
		}
		q.Checksums = append(q.Checksums, sum)
	}
	if q.UpdatedAfter, q.UpdatedBefore, err = listquery.DateWindow(v, models.AssetFacetUpdated, q.Now, q.Location); err != nil {
		return err
	}
	if q.CreatedAfter, q.CreatedBefore, err = listquery.DateWindow(v, models.AssetFacetCreated, q.Now, q.Location); err != nil {
		return err
	}
	q.Text = listquery.Single(v, "text")
	switch n := utf8.RuneCountInString(q.Text); {
	case n > 0 && n < minTextLen:
		return &listquery.Error{Code: models.AssetCodeQueryTooShort, Param: "text", Msg: fmt.Sprintf("text must be at least %d characters", minTextLen)}
	case n > maxTextLen:
		return &listquery.Error{Code: models.AssetCodeQueryTooLong, Param: "text", Msg: fmt.Sprintf("text must be at most %d characters", maxTextLen)}
	}
	q.ScopeCampaign = listquery.Single(v, "scope_campaign")
	return nil
}

func parsePaging(v url.Values, q *models.AssetQuery) (err error) {
	if q.Sort, err = listquery.OneOf(v, "sort", models.AssetSorts, models.AssetSortUpdatedDesc); err != nil {
		return err
	}
	if q.Sort == models.AssetSortRelevance && q.Text == "" {
		return listquery.Invalid("sort", "relevance needs text")
	}
	if q.Group, err = listquery.OneOf(v, "group", models.AssetGroups, ""); err != nil {
		return err
	}
	if raw := listquery.Single(v, "limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxPageSize {
			return listquery.Invalid("limit", "must be between 1 and %d", MaxPageSize)
		}
		q.Limit = n
	}
	q.Cursor = listquery.Single(v, "cursor")
	q.Facets = listquery.Values(v, "facets")
	for _, f := range q.Facets {
		if !slices.Contains(models.AssetFacets, f) {
			return listquery.Invalid("facets", "unknown field %q (allowed: %s)", f, strings.Join(models.AssetFacets, ", "))
		}
	}
	return nil
}

// IDsOnly reads ids_only: whether the request wants every matching id rather
// than a page.
func IDsOnly(v url.Values) (bool, error) {
	raw := listquery.Single(v, "ids_only")
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, listquery.Invalid("ids_only", "must be true or false")
	}
	return b, nil
}
