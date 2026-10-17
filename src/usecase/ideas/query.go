package ideas

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
)

// Page sizes of the ideas list. A request without limit gets every match up to
// UnpagedLimit, which is what clients that predate paging rely on.
const (
	MaxPageSize  = 200
	UnpagedLimit = 1000
	maxTextLen   = 200
)

// Params lists the parameters of the ideas list's address, each with whether
// it takes a comma-separated list. Paging and display parameters are not part
// of the address; see VolatileParams.
var Params = func() map[string]bool {
	params := map[string]bool{
		"text": false, "scope_campaign": false, "sort": false, "group": false,
		"created": false, "created_after": false, "created_before": false, "asset_id": false,
	}
	for _, p := range []string{
		models.IdeaFacetOrigin, models.IdeaFacetSourceAsset, models.IdeaFacetKind, models.IdeaFacetCampaign,
		models.IdeaFacetFit, models.IdeaFacetPhase, models.IdeaFacetAuthor,
	} {
		params[p], params[p+listquery.NotSuffix] = true, true
	}
	return params
}()

// VolatileParams are the parameters a request carries that are not part of
// what a list shows: paging, the facets asked for, the viewer's time zone, and
// the pile tab.
var VolatileParams = []string{"limit", "cursor", "facets", "tz", "ids_only", "pile", "pile" + listquery.NotSuffix}

// ParseQuery reads the ideas list parameters at the clock now. It rejects an
// unknown value with a *listquery.Error; parameters it does not know are
// ignored.
func ParseQuery(v url.Values, now time.Time) (models.IdeaQuery, error) {
	q := models.IdeaQuery{Now: now, Location: time.UTC, Limit: UnpagedLimit}
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

func parseFilters(v url.Values, q *models.IdeaQuery) (err error) {
	for _, f := range []struct {
		dst     *models.AssetValueFilter
		key     string
		allowed []string
	}{
		{&q.Pile, "pile", models.IdeaPiles},
		{&q.Origin, models.IdeaFacetOrigin, models.IdeaOriginKeys},
		{&q.SourceAsset, models.IdeaFacetSourceAsset, nil},
		{&q.Kind, models.IdeaFacetKind, models.AssetKinds},
		{&q.Campaign, models.IdeaFacetCampaign, nil},
		{&q.Fit, models.IdeaFacetFit, models.IdeaFitBands},
		{&q.Phase, models.IdeaFacetPhase, nil},
		{&q.Author, models.IdeaFacetAuthor, nil},
	} {
		if *f.dst, err = listquery.Filter(v, f.key, f.allowed); err != nil {
			return err
		}
	}
	if id := listquery.Single(v, "asset_id"); id != "" && !slices.Contains(q.SourceAsset.Any, id) {
		q.SourceAsset.Any = append(q.SourceAsset.Any, id)
	}
	q.Text = listquery.Single(v, "text")
	if utf8.RuneCountInString(q.Text) > maxTextLen {
		return listquery.Invalid("text", "must be at most %d characters", maxTextLen)
	}
	if q.CreatedAfter, q.CreatedBefore, err = listquery.DateWindow(v, "created", q.Now, q.Location); err != nil {
		return err
	}
	q.ScopeCampaign = listquery.Single(v, "scope_campaign")
	return nil
}

func parsePaging(v url.Values, q *models.IdeaQuery) (err error) {
	if q.Sort, err = listquery.OneOf(v, "sort", models.IdeaSorts, models.IdeaSortCreated); err != nil {
		return err
	}
	if q.Group, err = listquery.OneOf(v, "group", models.IdeaGroups, ""); err != nil {
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
		if !slices.Contains(models.IdeaFacets, f) {
			return listquery.Invalid("facets", "unknown field %q (allowed: %s)", f, strings.Join(models.IdeaFacets, ", "))
		}
	}
	return nil
}
