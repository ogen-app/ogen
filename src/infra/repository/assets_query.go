package repository

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/textgist"
)

// ErrInvalidAssetCursor is returned for a cursor the asset list did not issue,
// or issued for another sort order.
var ErrInvalidAssetCursor = errors.New("invalid cursor")

// assetKindSQL classifies an asset row a joined with its file af into a
// models.AssetKind.
const assetKindSQL = `CASE
		WHEN a.type IS NULL OR a.type = 'MD' THEN 'note'
		WHEN a.type = 'PDF' THEN 'pdf'
		WHEN a.type = 'URL' THEN 'web'
		WHEN a.type = 'AUDIO' THEN 'audio'
		WHEN a.type = 'IMG' THEN 'image'
		WHEN af.mime_type IN ('text/csv', 'text/tab-separated-values') THEN 'csv'
		WHEN af.mime_type LIKE '%spreadsheet%' OR af.mime_type LIKE 'application/vnd.ms-excel%' THEN 'spreadsheet'
		WHEN af.mime_type LIKE '%presentation%' OR af.mime_type LIKE 'application/vnd.ms-powerpoint%' THEN 'presentation'
		ELSE 'document'
	END`

// ideasBucketSQL is, per ideas filter value, the condition on a facts row f.
var ideasBucketSQL = map[string]string{
	models.AssetIdeasUndecided: "f.i_undecided > 0",
	models.AssetIdeasYes:       "f.i_yes > 0",
	models.AssetIdeasLater:     "f.i_later > 0",
	models.AssetIdeasNo:        "f.i_no > 0",
	models.AssetIdeasNone:      "(f.analysed AND f.i_undecided + f.i_yes + f.i_later + f.i_no = 0)",
	models.AssetIdeasUnread:    "NOT f.analysed",
}

// undecidedIdeaSQL is the condition, on an idea row i, of an idea counted as
// undecided; its argument is the query's clock.
const undecidedIdeaSQL = `(i.verdict IS NULL OR (i.verdict = 'later' AND i.remind_at <= ?))`

// assetSortKey is one column of a row order: the expression, the cast its
// cursor value is read back with and its direction.
type assetSortKey struct {
	expr, cast string
	desc       bool
}

var (
	keyUpdated   = assetSortKey{expr: "f.updated_at", cast: "timestamptz"}
	keyCreated   = assetSortKey{expr: "f.created_at", cast: "timestamptz"}
	keyID        = assetSortKey{expr: "f.id", cast: "text"}
	keyTitle     = assetSortKey{expr: "f.title_key", cast: "text"}
	keyUndecided = assetSortKey{expr: "f.i_undecided", cast: "bigint"}
	keyMatches   = assetSortKey{expr: "f.matches", cast: "bigint"}

	// groupKeys put the groups in display order ahead of the sort: by rank,
	// then name, then key.
	groupKeys = []assetSortKey{
		{expr: "f.g_rank", cast: "bigint"}, {expr: "f.g_name", cast: "text"}, {expr: "f.g_key", cast: "text"},
	}
)

// assetSorts maps each sort order to its keys, all in one direction, the
// asset id last so the order is total.
var assetSorts = map[string]struct {
	keys []assetSortKey
	desc bool
}{
	models.AssetSortUpdatedDesc:    {[]assetSortKey{keyUpdated, keyID}, true},
	models.AssetSortUpdatedAsc:     {[]assetSortKey{keyUpdated, keyID}, false},
	models.AssetSortCreatedDesc:    {[]assetSortKey{keyCreated, keyID}, true},
	models.AssetSortCreatedAsc:     {[]assetSortKey{keyCreated, keyID}, false},
	models.AssetSortTitleAsc:       {[]assetSortKey{keyTitle, keyID}, false},
	models.AssetSortTitleDesc:      {[]assetSortKey{keyTitle, keyID}, true},
	models.AssetSortIdeasUndecided: {[]assetSortKey{keyUndecided, keyUpdated, keyID}, true},
	models.AssetSortRelevance:      {[]assetSortKey{keyMatches, keyUpdated, keyID}, true},
}

// rowOrder is the keys a query's rows are ordered by: the group's when it
// groups, then the sort's.
func rowOrder(q models.AssetQuery) []assetSortKey {
	s := assetSorts[q.Sort]
	var keys []assetSortKey
	if q.Group != "" {
		keys = slices.Clone(groupKeys)
	}
	for _, k := range s.keys {
		k.desc = s.desc
		keys = append(keys, k)
	}
	return keys
}

// assetCursor is the position after the last row of a page.
type assetCursor struct {
	Sort  string            `json:"s"`
	Group string            `json:"g,omitempty"`
	Keys  []json.RawMessage `json:"k"`
}

func encodeAssetCursor(q models.AssetQuery, keys []json.RawMessage) string {
	b, _ := json.Marshal(assetCursor{Sort: q.Sort, Group: q.Group, Keys: keys})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeAssetCursor reads a cursor back into one SQL argument per order key.
func decodeAssetCursor(raw string, q models.AssetQuery) ([]any, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidAssetCursor
	}
	var c assetCursor
	if err := json.Unmarshal(b, &c); err != nil || c.Sort != q.Sort || c.Group != q.Group {
		return nil, ErrInvalidAssetCursor
	}
	keys := rowOrder(q)
	if len(c.Keys) != len(keys) {
		return nil, ErrInvalidAssetCursor
	}
	args := make([]any, len(keys))
	for i, k := range keys {
		switch k.cast {
		case "bigint":
			var n int64
			if err := json.Unmarshal(c.Keys[i], &n); err != nil {
				return nil, ErrInvalidAssetCursor
			}
			args[i] = n
		case "timestamptz":
			var t time.Time
			if err := json.Unmarshal(c.Keys[i], &t); err != nil {
				return nil, ErrInvalidAssetCursor
			}
			args[i] = t
		default:
			var s string
			if err := json.Unmarshal(c.Keys[i], &s); err != nil {
				return nil, ErrInvalidAssetCursor
			}
			args[i] = s
		}
	}
	return args, nil
}

// sqlBuf accumulates SQL text with its positional arguments in order.
type sqlBuf struct {
	sb   strings.Builder
	args []any
}

func (b *sqlBuf) add(sql string, args ...any) {
	b.sb.WriteString(sql)
	b.args = append(b.args, args...)
}

// assetPred is one field's filter as a condition on a facts row f.
type assetPred struct {
	field string
	sql   string
	args  []any
}

// assetPreds turns a query's filters into conditions, one per field.
func assetPreds(q models.AssetQuery) []assetPred {
	var preds []assetPred
	if f := q.Title; f.Active() {
		like := func(s string) any { return "%" + escapeLike(s) + "%" }
		preds = append(preds, valuePred("title", f, func(v string) (string, []any) {
			return `f.title ILIKE ? ESCAPE '\'`, []any{like(v)}
		}))
	}
	inList := func(field, col string, f models.AssetValueFilter) {
		if !f.Active() {
			return
		}
		var sql []string
		var args []any
		if len(f.Any) > 0 {
			sql = append(sql, col+" IN (?)")
			args = append(args, bun.List(f.Any))
		}
		if len(f.Not) > 0 {
			sql = append(sql, col+" NOT IN (?)")
			args = append(args, bun.List(f.Not))
		}
		preds = append(preds, assetPred{field, strings.Join(sql, " AND "), args})
	}
	inList(models.AssetFacetKind, "f.kind", q.Kind)
	inList(models.AssetFacetStatus, "f.status", q.Status)
	if f := q.Campaign; f.Active() {
		preds = append(preds, arrayPred(models.AssetFacetCampaign, "f.campaign_ids", f))
	}
	if f := q.Tag; f.Active() {
		preds = append(preds, arrayPred(models.AssetFacetTag, "f.tag_ids", f))
	}
	if f := q.Ideas; f.Active() {
		preds = append(preds, valuePred(models.AssetFacetIdeas, f, func(v string) (string, []any) {
			return ideasBucketSQL[v], nil
		}))
	}
	if p, ok := windowPred(models.AssetFacetUpdated, "f.updated_at", q.UpdatedAfter, q.UpdatedBefore); ok {
		preds = append(preds, p)
	}
	if p, ok := windowPred(models.AssetFacetCreated, "f.created_at", q.CreatedAfter, q.CreatedBefore); ok {
		preds = append(preds, p)
	}
	if len(q.Checksums) > 0 {
		preds = append(preds, assetPred{"checksum", "f.checksum IN (?)", []any{bun.List(q.Checksums)}})
	}
	if q.Text != "" {
		preds = append(preds, assetPred{"text", "f.matches > 0", nil})
	}
	return preds
}

// windowPred keeps the rows whose col is at or after after and before before;
// false when neither bound is set.
func windowPred(field, col string, after, before *time.Time) (assetPred, bool) {
	var sql []string
	var args []any
	if after != nil {
		sql = append(sql, col+" >= ?")
		args = append(args, *after)
	}
	if before != nil {
		sql = append(sql, col+" < ?")
		args = append(args, *before)
	}
	return assetPred{field, strings.Join(sql, " AND "), args}, len(sql) > 0
}

// valuePred matches a row when any of f.Any's conditions holds and none of
// f.Not's does.
func valuePred(field string, f models.AssetValueFilter, cond func(string) (string, []any)) assetPred {
	or := func(values []string) (string, []any) {
		parts := make([]string, len(values))
		var args []any
		for i, v := range values {
			var a []any
			parts[i], a = cond(v)
			args = append(args, a...)
		}
		return "(" + strings.Join(parts, " OR ") + ")", args
	}
	var sql []string
	var args []any
	if len(f.Any) > 0 {
		s, a := or(f.Any)
		sql, args = append(sql, s), append(args, a...)
	}
	if len(f.Not) > 0 {
		s, a := or(f.Not)
		sql, args = append(sql, "NOT "+s), append(args, a...)
	}
	return assetPred{field, strings.Join(sql, " AND "), args}
}

// arrayPred filters on an array column of ids where AssetFilterNone stands for
// an empty array.
func arrayPred(field, col string, f models.AssetValueFilter) assetPred {
	return valuePred(field, f, func(v string) (string, []any) {
		if v == models.AssetFilterNone {
			return "cardinality(" + col + ") = 0", nil
		}
		return "? = ANY(" + col + ")", []any{v}
	})
}

// where renders every predicate but the one on field except ("" keeps all).
func where(b *sqlBuf, preds []assetPred, except string) {
	n := 0
	for _, p := range preds {
		if p.field == except {
			continue
		}
		if n == 0 {
			b.add(" WHERE ")
		} else {
			b.add(" AND ")
		}
		b.add(p.sql, p.args...)
		n++
	}
}

// writeAssetFacts writes the CTEs ending in facts: one row per asset in scope
// with every value the filters, facets and sorts read.
//
// The campaigns that count are the active ones, the set the campaign list
// shows: neither deleted nor archived. A campaign scope may still name an
// archived campaign, whose overview stays readable; it counts then.
//
// Campaign membership agrees with how generation reads it: an asset belongs to
// every such campaign listing it, and to every one that draws on the whole
// bank (use_assets with no asset_ids). Ideas count only those of the analysis
// lenses in scope: the workspace lens plus every counted campaign's, or under
// a campaign scope that campaign's alone.
func writeAssetFacts(b *sqlBuf, q models.AssetQuery, tenantID string) {
	b.add(`WITH camps_raw AS (
	SELECT c.id, c.use_assets,
		CASE WHEN jsonb_typeof(c.asset_ids) = 'array' THEN c.asset_ids ELSE '[]'::jsonb END AS asset_ids
	FROM campaigns c WHERE c.tenant_id = ? AND c.deleted_at IS NULL AND (c.archived_at IS NULL OR c.id = ?)
), camps AS MATERIALIZED (
	SELECT c.id, c.asset_ids, (c.use_assets AND c.asset_ids = '[]'::jsonb) AS whole_bank FROM camps_raw c
), members AS MATERIALIZED (
	SELECT m.asset_id, array_agg(m.campaign_id ORDER BY m.campaign_id) AS campaign_ids
	FROM (SELECT jsonb_array_elements_text(c.asset_ids) AS asset_id, c.id AS campaign_id FROM camps c) m
	GROUP BY m.asset_id
), whole AS (
	SELECT coalesce(array_agg(c.id ORDER BY c.id), '{}'::text[]) AS ids FROM camps c WHERE c.whole_bank
), live_tags AS (
	SELECT coalesce(array_agg(t.id), '{}'::text[]) AS ids FROM tags t WHERE t.tenant_id = ?
), lens_ideas AS (
	SELECT i.asset_analysis_id AS lens_id,
		count(*) FILTER (WHERE `+undecidedIdeaSQL+`) AS i_undecided,
		count(*) FILTER (WHERE i.verdict = 'yes') AS i_yes,
		count(*) FILTER (WHERE i.verdict = 'later' AND (i.remind_at IS NULL OR i.remind_at > ?)) AS i_later,
		count(*) FILTER (WHERE i.verdict = 'no') AS i_no
	FROM ideas i
	WHERE i.tenant_id = ? AND i.asset_analysis_id IS NOT NULL AND i.origin = 'asset_analysis'
	GROUP BY i.asset_analysis_id
), per_asset AS MATERIALIZED (
	SELECT aa.asset_id, bool_or(aa.status = 'ready') AS analysed,
		coalesce(sum(li.i_undecided), 0) AS i_undecided, coalesce(sum(li.i_yes), 0) AS i_yes,
		coalesce(sum(li.i_later), 0) AS i_later, coalesce(sum(li.i_no), 0) AS i_no
	FROM asset_analyses aa
	LEFT JOIN lens_ideas li ON li.lens_id = aa.id
	WHERE aa.tenant_id = ? AND `, tenantID, q.ScopeCampaign, tenantID, q.Now, q.Now, tenantID, tenantID)
	lensInScope(b, q)
	b.add(`
	GROUP BY aa.asset_id
)`)
	if q.Text != "" {
		b.add(`, hits AS MATERIALIZED (
	SELECT ac.asset_id, count(*) AS matches FROM assets_chunks ac
	WHERE ac.tenant_id = ? AND `+chunkTSV+` @@ websearch_to_tsquery('english', ?)
	GROUP BY ac.asset_id
)`, tenantID, q.Text)
	}
	b.add(`, facts AS MATERIALIZED (
	SELECT a.id, a.title, lower(a.title) AS title_key, a.updated_at, a.created_at,
		` + assetKindSQL + ` AS kind,
		CASE WHEN a.status = 'processing' THEN 'pending' ELSE a.status END AS status,
		af.checksum_sha256 AS checksum,
		ARRAY(SELECT x FROM jsonb_array_elements_text(
				CASE WHEN jsonb_typeof(a.tag_ids) = 'array' THEN a.tag_ids ELSE '[]'::jsonb END) AS x
			WHERE x = ANY((SELECT t.ids FROM live_tags t)::text[])) AS tag_ids,
		(SELECT w.ids FROM whole w) || coalesce(m.campaign_ids, '{}'::text[]) AS campaign_ids,
		coalesce(pa.i_undecided, 0) AS i_undecided, coalesce(pa.i_yes, 0) AS i_yes,
		coalesce(pa.i_later, 0) AS i_later, coalesce(pa.i_no, 0) AS i_no,
		coalesce(pa.analysed, false) AS analysed,
		`)
	if q.Text != "" {
		b.add(`coalesce(h.matches, 0) AS matches`)
	} else {
		b.add(`0 AS matches`)
	}
	b.add(`
	FROM assets a
	LEFT JOIN asset_files af ON af.asset_id = a.id
	LEFT JOIN members m ON m.asset_id = a.id
	LEFT JOIN per_asset pa ON pa.asset_id = a.id`)
	if q.Text != "" {
		b.add(`
	LEFT JOIN hits h ON h.asset_id = a.id`)
	}
	b.add(`
	WHERE a.tenant_id = ?`, tenantID)
	if q.ScopeCampaign != "" {
		b.add(` AND EXISTS (
		SELECT 1 FROM camps c WHERE c.id = ? AND (c.whole_bank OR c.asset_ids @> jsonb_build_array(a.id)))`, q.ScopeCampaign)
	}
	b.add(`
)
`)
}

// lensInScope is the condition on an analysis row aa of a lens whose ideas
// count: the workspace lens plus every counted campaign's, or under a campaign
// scope that campaign's alone. It reads the camps CTE.
func lensInScope(b *sqlBuf, q models.AssetQuery) {
	if q.ScopeCampaign != "" {
		b.add(`(aa.campaign_id IS NULL OR aa.campaign_id = ?)`, q.ScopeCampaign)
	} else {
		b.add(`(aa.campaign_id IS NULL OR aa.campaign_id IN (SELECT c.id FROM camps c))`)
	}
}

// writeAssetGrouped writes the grouped CTE when q groups: one row per facts
// row and group it is listed under, with the group's g_rank, g_name and g_key.
// A grouping by tag or campaign lists an asset under each of its values that
// the field's filter allows, or under none when it has no value.
func writeAssetGrouped(b *sqlBuf, q models.AssetQuery) {
	switch q.Group {
	case models.AssetGroupTag:
		writeArrayGrouped(b, "f.tag_ids", "tags", q.Tag)
		return
	case models.AssetGroupCampaign:
		writeArrayGrouped(b, "f.campaign_ids", "campaigns", q.Campaign)
		return
	}
	var keys []string
	var key string
	var args []any
	switch q.Group {
	case models.AssetGroupIdeas:
		keys = models.AssetIdeasGroupKeys
		key = `CASE WHEN f.i_undecided > 0 THEN 'undecided'
			WHEN f.i_yes + f.i_later + f.i_no > 0 THEN 'answered' ELSE 'none' END`
	case models.AssetGroupKind:
		keys, key = models.AssetKinds, "f.kind"
	case models.AssetGroupCreated:
		keys = models.AssetCreatedGroupKeys
		var whens []string
		for _, band := range keys[:len(keys)-1] {
			since, _ := models.AssetDateSince(band, q.Now, q.Location)
			whens = append(whens, "WHEN f.created_at >= ? THEN ?::text")
			args = append(args, since, band)
		}
		key = "CASE " + strings.Join(whens, " ") + " ELSE ?::text END"
		args = append(args, models.AssetCreatedEarlier)
	}
	b.add(`, grouped AS MATERIALIZED (
	SELECT f.*, array_position(?::text[], g.key)::bigint AS g_rank, ''::text AS g_name, g.key AS g_key
	FROM facts f CROSS JOIN LATERAL (SELECT `+key+` AS key) g
)
`, append([]any{pgdialect.Array(keys)}, args...)...)
}

// writeArrayGrouped writes the grouped CTE of a grouping by an array column of
// ids, named from table: the named groups by name, then none.
func writeArrayGrouped(b *sqlBuf, col, table string, f models.AssetValueFilter) {
	b.add(`, grouped AS MATERIALIZED (
	SELECT f.*, (v.id IS NULL)::int::bigint AS g_rank,
		coalesce((SELECT lower(n.name) FROM `+table+` n WHERE n.id = v.id), '') AS g_name,
		coalesce(v.id, ?) AS g_key
	FROM facts f
	CROSS JOIN LATERAL unnest(CASE WHEN cardinality(`+col+`) = 0 THEN ARRAY[NULL::text] ELSE `+col+` END) AS v(id)`,
		models.AssetFilterNone)
	var conds []string
	var args []any
	if len(f.Any) > 0 {
		conds = append(conds, "coalesce(v.id, ?) IN (?)")
		args = append(args, models.AssetFilterNone, bun.List(f.Any))
	}
	if len(f.Not) > 0 {
		conds = append(conds, "coalesce(v.id, ?) NOT IN (?)")
		args = append(args, models.AssetFilterNone, bun.List(f.Not))
	}
	if len(conds) > 0 {
		b.add(`
	WHERE `+strings.Join(conds, " AND "), args...)
	}
	b.add(`
)
`)
}

// orderBy renders an ORDER BY list.
func orderBy(keys []assetSortKey) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k.expr + " ASC"
		if k.desc {
			parts[i] = k.expr + " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

// afterCursor is the keyset condition for the rows after a cursor's values,
// one per key. The keys may mix directions (a grouping is ascending whatever
// the sort), so it is spelt out key by key rather than as a row comparison:
// the first i keys equal and key i past its value, for some i.
func afterCursor(keys []assetSortKey, vals []any) assetPred {
	var alts []string
	var args []any
	for i, k := range keys {
		parts := make([]string, 0, i+1)
		for j := range i {
			parts = append(parts, keys[j].expr+" = ?::"+keys[j].cast)
			args = append(args, vals[j])
		}
		op := " > "
		if k.desc {
			op = " < "
		}
		parts = append(parts, k.expr+op+"?::"+k.cast)
		args = append(args, vals[i])
		alts = append(alts, "("+strings.Join(parts, " AND ")+")")
	}
	return assetPred{"cursor", "(" + strings.Join(alts, " OR ") + ")", args}
}

// assetFacetSQL writes one facet's counts as (field, value, n) rows.
func assetFacetSQL(b *sqlBuf, field string, preds []assetPred, q models.AssetQuery) {
	switch field {
	case models.AssetFacetKind, models.AssetFacetStatus:
		b.add(`SELECT ?::text AS field, f.`+field+` AS value, count(*) AS n FROM facts f`, field)
		where(b, preds, field)
		b.add(` GROUP BY f.` + field)
	case models.AssetFacetTag, models.AssetFacetCampaign:
		col := "f.tag_ids"
		if field == models.AssetFacetCampaign {
			col = "f.campaign_ids"
		}
		b.add(`SELECT ?::text AS field, v.value, count(*) AS n FROM facts f
	CROSS JOIN LATERAL unnest(CASE WHEN cardinality(`+col+`) = 0 THEN ARRAY[?::text] ELSE `+col+` END) AS v(value)`,
			field, models.AssetFilterNone)
		where(b, preds, field)
		b.add(` GROUP BY v.value`)
	case models.AssetFacetIdeas:
		buckets := make([]string, len(models.AssetIdeasValues))
		var args []any
		for i, v := range models.AssetIdeasValues {
			buckets[i] = "CASE WHEN " + ideasBucketSQL[v] + " THEN ?::text END"
			args = append(args, v)
		}
		b.add(`SELECT ?::text AS field, v.value, count(*) AS n FROM facts f
	CROSS JOIN LATERAL unnest(array_remove(ARRAY[`+strings.Join(buckets, ", ")+`], NULL)) AS v(value)`,
			append([]any{field}, args...)...)
		where(b, preds, field)
		b.add(` GROUP BY v.value`)
	case models.AssetFacetUpdated, models.AssetFacetCreated:
		rows := make([]string, len(models.AssetDatePresets))
		var args []any
		for i, p := range models.AssetDatePresets {
			since, _ := models.AssetDateSince(p, q.Now, q.Location)
			rows[i] = "(?::text, ?::timestamptz)"
			args = append(args, p, since)
		}
		b.add(`SELECT ?::text AS field, v.value, count(*) AS n FROM facts f
	JOIN (VALUES `+strings.Join(rows, ", ")+`) AS v(value, since) ON f.`+field+`_at >= v.since`,
			append([]any{field}, args...)...)
		where(b, preds, field)
		b.add(` GROUP BY v.value`)
	}
}

// assetPageRow is one row of a page as the query returns it.
type assetPageRow struct {
	ID          string                `json:"id"`
	Kind        models.AssetKind      `json:"kind"`
	TagIDs      []string              `json:"tag_ids"`
	CampaignIDs []string              `json:"campaign_ids"`
	Undecided   int                   `json:"i_undecided"`
	Yes         int                   `json:"i_yes"`
	Later       int                   `json:"i_later"`
	No          int                   `json:"i_no"`
	Matches     int                   `json:"matches"`
	GroupKey    string                `json:"group_key"`
	NextIdea    *models.AssetNextIdea `json:"next_idea"`
	Keys        []json.RawMessage     `json:"k"`
}

// facetRequested reports whether q asks for field's counts. The campaign facet
// is meaningless inside one campaign's scope, so it is never returned there.
func facetRequested(q models.AssetQuery, field string) bool {
	if field == models.AssetFacetCampaign && q.ScopeCampaign != "" {
		return false
	}
	return slices.Contains(q.Facets, field)
}

func (r *assetRepository) Query(ctx context.Context, q models.AssetQuery) (*models.AssetQueryResult, error) {
	// A raw query bypasses the TenantScoped hooks, so every table is scoped
	// here.
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	preds := assetPreds(q)
	pagePreds := preds
	if q.Cursor != "" {
		vals, err := decodeAssetCursor(q.Cursor, q)
		if err != nil {
			return nil, err
		}
		pagePreds = append(slices.Clip(preds), afterCursor(rowOrder(q), vals))
	}

	var b sqlBuf
	writeAssetFacts(&b, q, tid)
	if q.Group != "" {
		writeAssetGrouped(&b, q)
	}
	b.add(`SELECT (SELECT count(*) FROM facts f`)
	where(&b, preds, "")
	b.add(`) AS total,
	(SELECT coalesce(sum(f.i_undecided), 0) FROM facts f`)
	where(&b, preds, "")
	b.add(`) AS ideas_undecided,
	(SELECT count(*) FROM facts) AS total_unfiltered,
	`)
	writeAssetPage(&b, q, tid, pagePreds)
	b.add(`,
	`)
	writeAssetGroups(&b, q, preds)
	b.add(`,
	`)
	facets := writeAssetFacets(&b, q, preds)
	b.add(`,
	(SELECT to_json(t.ids) FROM live_tags t) AS tag_ids,
	(SELECT coalesce(json_agg(c.id), '[]') FROM camps c) AS campaign_ids,
	`)
	writeScopeFound(&b, q)

	var row struct {
		Total           int             `bun:"total"`
		IdeasUndecided  int             `bun:"ideas_undecided"`
		TotalUnfiltered int             `bun:"total_unfiltered"`
		Page            json.RawMessage `bun:"page"`
		Groups          json.RawMessage `bun:"groups"`
		Facets          json.RawMessage `bun:"facets"`
		TagIDs          json.RawMessage `bun:"tag_ids"`
		CampaignIDs     json.RawMessage `bun:"campaign_ids"`
		ScopeFound      bool            `bun:"scope_found"`
	}
	if err := r.db.NewRaw(b.sb.String(), b.args...).Scan(ctx, &row); err != nil {
		return nil, err
	}
	if !row.ScopeFound {
		return nil, sql.ErrNoRows
	}

	var page []assetPageRow
	if err := json.Unmarshal(row.Page, &page); err != nil {
		return nil, fmt.Errorf("decode asset page: %w", err)
	}
	res := &models.AssetQueryResult{Total: row.Total, TotalUnfiltered: row.TotalUnfiltered, IdeasUndecided: row.IdeasUndecided}
	if len(page) > q.Limit {
		page = page[:q.Limit]
		res.NextCursor = encodeAssetCursor(q, page[len(page)-1].Keys)
	}
	if q.Group != "" {
		if err := json.Unmarshal(row.Groups, &res.Groups); err != nil {
			return nil, fmt.Errorf("decode asset groups: %w", err)
		}
	}
	var err error
	if res.Facets, err = assetFacetCounts(row.Facets, row.TagIDs, row.CampaignIDs, facets); err != nil {
		return nil, err
	}
	if res.Assets, err = r.hydrateListItems(ctx, tid, page, q.Text); err != nil {
		return nil, err
	}
	return res, nil
}

// writeAssetPage writes the page column: the rows after the cursor, one more
// than the limit so the caller knows whether another page follows, each with
// its order keys and the idea its row names.
//
// The next idea is looked up after the limit, so only the page's rows with an
// undecided idea pay for it. It is the oldest of the ideas i_undecided counts:
// the same lenses and the same rule.
func writeAssetPage(b *sqlBuf, q models.AssetQuery, tid string, preds []assetPred) {
	keys := rowOrder(q)
	order := orderBy(keys)
	exprs := make([]string, len(keys))
	for i, k := range keys {
		exprs[i] = k.expr
	}
	from, groupKey := "facts", "''::text"
	if q.Group != "" {
		from, groupKey = "grouped", "f.g_key"
	}
	b.add(`(SELECT coalesce(json_agg(p ORDER BY p.n), '[]') FROM (
		SELECT s.*, ni.next_idea FROM (
			SELECT f.id, f.kind, f.tag_ids, f.campaign_ids, f.i_undecided, f.i_yes, f.i_later, f.i_no, f.matches,
				` + groupKey + ` AS group_key, json_build_array(` + strings.Join(exprs, ", ") + `) AS k,
				row_number() OVER (ORDER BY ` + order + `) AS n
			FROM ` + from + ` f`)
	where(b, preds, "")
	b.add(` ORDER BY `+order+` LIMIT ?
		) s
		LEFT JOIN LATERAL (
			SELECT json_build_object('id', i.id, 'title', i.title) AS next_idea
			FROM ideas i JOIN asset_analyses aa ON aa.id = i.asset_analysis_id
			WHERE s.i_undecided > 0 AND aa.asset_id = s.id AND aa.tenant_id = ? AND i.tenant_id = ?
				AND i.origin = 'asset_analysis' AND `+undecidedIdeaSQL+` AND `, q.Limit+1, tid, tid, q.Now)
	lensInScope(b, q)
	b.add(`
			ORDER BY i.created_at, i.id LIMIT 1
		) ni ON true
	) p) AS page`)
}

// writeAssetGroups writes the groups column: each non-empty group of the whole
// match with its row count, in display order.
func writeAssetGroups(b *sqlBuf, q models.AssetQuery, preds []assetPred) {
	if q.Group == "" {
		b.add(`'[]'::json AS groups`)
		return
	}
	b.add(`(SELECT coalesce(json_agg(json_build_object('key', g.g_key, 'count', g.n)
		ORDER BY g.g_rank, g.g_name, g.g_key), '[]') FROM (
		SELECT f.g_rank, f.g_name, f.g_key, count(*) AS n FROM grouped f`)
	where(b, preds, "")
	b.add(` GROUP BY f.g_rank, f.g_name, f.g_key) g) AS groups`)
}

// writeAssetFacets writes the facets column with the counts of every facet q
// asks for, and returns those fields.
func writeAssetFacets(b *sqlBuf, q models.AssetQuery, preds []assetPred) []string {
	var facets []string
	for _, field := range models.AssetFacets {
		if facetRequested(q, field) {
			facets = append(facets, field)
		}
	}
	if len(facets) == 0 {
		b.add(`'[]'::json AS facets`)
		return nil
	}
	b.add(`(SELECT coalesce(json_agg(x), '[]') FROM (`)
	for i, field := range facets {
		if i > 0 {
			b.add(` UNION ALL `)
		}
		assetFacetSQL(b, field, preds, q)
	}
	b.add(`) x) AS facets`)
	return facets
}

// writeScopeFound writes the scope_found column: whether a campaign scope names
// a campaign that counts.
func writeScopeFound(b *sqlBuf, q models.AssetQuery) {
	if q.ScopeCampaign != "" {
		b.add(`EXISTS (SELECT 1 FROM camps c WHERE c.id = ?) AS scope_found`, q.ScopeCampaign)
	} else {
		b.add(`TRUE AS scope_found`)
	}
}

// QueryIDs lists every matching id in sort order; it never groups.
func (r *assetRepository) QueryIDs(ctx context.Context, q models.AssetQuery) ([]string, error) {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	q.Group = ""
	var b sqlBuf
	writeAssetFacts(&b, q, tid)
	b.add(`SELECT (SELECT coalesce(json_agg(f.id ORDER BY ` + orderBy(rowOrder(q)) + `), '[]') FROM facts f`)
	where(&b, assetPreds(q), "")
	b.add(`) AS ids, `)
	writeScopeFound(&b, q)
	var row struct {
		IDs        json.RawMessage `bun:"ids"`
		ScopeFound bool            `bun:"scope_found"`
	}
	if err := r.db.NewRaw(b.sb.String(), b.args...).Scan(ctx, &row); err != nil {
		return nil, err
	}
	if !row.ScopeFound {
		return nil, sql.ErrNoRows
	}
	ids := []string{}
	if err := json.Unmarshal(row.IDs, &ids); err != nil {
		return nil, fmt.Errorf("decode asset ids: %w", err)
	}
	return ids, nil
}

// assetFacetCounts turns the facet rows into each requested field's counts,
// with every value of the field present: a value nothing matches gets 0.
// Fixed vocabularies keep their order; tags and campaigns go by count, then
// id, with none last.
func assetFacetCounts(raw, tagIDs, campaignIDs json.RawMessage, facets []string) (map[string][]models.AssetFacetCount, error) {
	if len(facets) == 0 {
		return nil, nil
	}
	var rows []struct {
		Field string `json:"field"`
		Value string `json:"value"`
		N     int    `json:"n"`
	}
	var tags, campaigns []string
	for _, d := range []struct {
		raw json.RawMessage
		v   any
	}{{raw, &rows}, {tagIDs, &tags}, {campaignIDs, &campaigns}} {
		if err := json.NewDecoder(bytes.NewReader(d.raw)).Decode(d.v); err != nil {
			return nil, fmt.Errorf("decode asset facets: %w", err)
		}
	}
	counts := make(map[string]map[string]int, len(facets))
	for _, r := range rows {
		if counts[r.Field] == nil {
			counts[r.Field] = map[string]int{}
		}
		counts[r.Field][r.Value] = r.N
	}
	out := make(map[string][]models.AssetFacetCount, len(facets))
	for _, field := range facets {
		var values []string
		ranked := false
		switch field {
		case models.AssetFacetKind:
			values = models.AssetKinds
		case models.AssetFacetStatus:
			values = models.AssetListStatuses
		case models.AssetFacetIdeas:
			values = models.AssetIdeasValues
		case models.AssetFacetUpdated, models.AssetFacetCreated:
			values = models.AssetDatePresets
		case models.AssetFacetTag:
			values, ranked = tags, true
		case models.AssetFacetCampaign:
			values, ranked = campaigns, true
		}
		list := make([]models.AssetFacetCount, 0, len(values)+1)
		for _, v := range values {
			list = append(list, models.AssetFacetCount{Value: v, Count: counts[field][v]})
		}
		if ranked {
			slices.SortFunc(list, func(a, b models.AssetFacetCount) int {
				return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Value, b.Value))
			})
			list = append(list, models.AssetFacetCount{Value: models.AssetFilterNone, Count: counts[field][models.AssetFilterNone]})
		}
		out[field] = list
	}
	return out, nil
}

// listItemSources holds what a page's list rows are built from, each keyed by
// asset id.
type listItemSources struct {
	assets  map[string]*models.Asset
	files   map[string]*models.AssetFile
	tags    map[string]models.Tag
	digests map[string]WorkspaceDigest
	// evidence is nil without a text query.
	evidence map[string]models.AssetTextMatch
}

// hydrateListItems builds the list rows of a page, in page order: the asset
// columns but content, its file, tags, workspace analysis status and gist,
// audio duration and, for a text query, where it matches.
func (r *assetRepository) hydrateListItems(ctx context.Context, tid string, page []assetPageRow, text string) ([]models.AssetListItem, error) {
	items := make([]models.AssetListItem, 0, len(page))
	if len(page) == 0 {
		return items, nil
	}
	src, err := r.loadListItemSources(ctx, tid, page, text)
	if err != nil {
		return nil, err
	}
	for _, p := range page {
		if item, ok := src.item(p); ok {
			items = append(items, item)
		}
	}
	return items, nil
}

func (r *assetRepository) loadListItemSources(ctx context.Context, tid string, page []assetPageRow, text string) (*listItemSources, error) {
	ids := make([]string, len(page))
	var tagIDs []string
	for i, p := range page {
		ids[i] = p.ID
		tagIDs = append(tagIDs, p.TagIDs...)
	}
	slices.Sort(tagIDs)

	src := &listItemSources{}
	var err error
	if src.assets, err = r.listColumns(ctx, ids); err != nil {
		return nil, err
	}
	if r.fileRepo != nil {
		if src.files, err = r.fileRepo.ListByAssetIDs(ctx, ids); err != nil {
			return nil, err
		}
	}
	if src.tags, err = r.tagRepo.GetByIDs(ctx, slices.Compact(tagIDs)); err != nil {
		return nil, err
	}
	if src.digests, err = (&assetAnalysisRepository{db: r.db}).WorkspaceDigests(ctx, ids); err != nil {
		return nil, err
	}
	if text != "" {
		if src.evidence, err = r.textEvidence(ctx, tid, text, ids); err != nil {
			return nil, err
		}
	}
	return src, nil
}

// listColumns reads the assets' list columns, everything but content.
func (r *assetRepository) listColumns(ctx context.Context, ids []string) (map[string]*models.Asset, error) {
	var assets []models.Asset
	if err := r.db.NewSelect().Model(&assets).
		Column("a.id", "a.title", "a.type", "a.status", "a.failure_code", "a.source_url", "a.published_at", "a.created_at", "a.updated_at").
		Where("a.id IN (?)", bun.List(ids)).
		Scan(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]*models.Asset, len(assets))
	for i := range assets {
		out[assets[i].ID] = &assets[i]
	}
	return out, nil
}

// item builds p's list row; false when the asset was deleted between the
// reads.
func (s *listItemSources) item(p assetPageRow) (models.AssetListItem, bool) {
	a, ok := s.assets[p.ID]
	if !ok {
		return models.AssetListItem{}, false
	}
	item := models.AssetListItem{
		ID: a.ID, Title: a.Title, Type: a.Type, Kind: p.Kind, Status: a.Status,
		FailureCode: a.FailureCode, SourceURL: a.SourceURL, PublishedAt: a.PublishedAt,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		Tags:        make([]models.Tag, 0, len(p.TagIDs)),
		CampaignIDs: append([]string{}, p.CampaignIDs...),
		IdeaCounts:  models.AssetIdeaCounts{Undecided: p.Undecided, Yes: p.Yes, Later: p.Later, No: p.No},
		NextIdea:    p.NextIdea,
		GroupKey:    p.GroupKey,
	}
	for _, id := range p.TagIDs {
		if t, ok := s.tags[id]; ok {
			item.Tags = append(item.Tags, t)
		}
	}
	if f := s.files[p.ID]; f != nil {
		item.File = &models.AssetListFile{
			MimeType: f.MimeType, PageCount: f.PageCount, DurationMS: f.DurationMs,
			ChecksumSHA256: f.ChecksumSHA256, S3Key: f.S3Key, ThumbnailS3Key: f.ThumbnailS3Key,
		}
	}
	if d, ok := s.digests[p.ID]; ok {
		item.AnalysisStatus = new(d.Status)
		// The same rule as the single-asset read: a gist only on a ready lens,
		// derived from the summary for lenses analysed before gists existed.
		if d.Status == models.AssetAnalysisStatusReady {
			if gist := cmp.Or(d.Gist, textgist.FromSummary(d.SummaryHead)); gist != "" {
				item.Gist = &gist
			}
		}
	}
	if s.evidence != nil {
		m := s.evidence[p.ID]
		m.Matches = p.Matches
		item.Match = &m
	}
	return item, true
}
