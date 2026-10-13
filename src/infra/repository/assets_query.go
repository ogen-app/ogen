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

// assetSortKey is one column of a sort order: the facts expression and the
// cast its cursor value is read back with.
type assetSortKey struct {
	expr, cast string
}

var (
	keyUpdated   = assetSortKey{"f.updated_at", "timestamptz"}
	keyID        = assetSortKey{"f.id", "text"}
	keyTitle     = assetSortKey{"f.title_key", "text"}
	keyUndecided = assetSortKey{"f.i_undecided", "bigint"}
	keyMatches   = assetSortKey{"f.matches", "bigint"}
)

// assetSorts maps each sort order to its keys, all in one direction, the
// asset id last so the order is total.
var assetSorts = map[string]struct {
	keys []assetSortKey
	desc bool
}{
	models.AssetSortUpdatedDesc:    {[]assetSortKey{keyUpdated, keyID}, true},
	models.AssetSortUpdatedAsc:     {[]assetSortKey{keyUpdated, keyID}, false},
	models.AssetSortTitleAsc:       {[]assetSortKey{keyTitle, keyID}, false},
	models.AssetSortTitleDesc:      {[]assetSortKey{keyTitle, keyID}, true},
	models.AssetSortIdeasUndecided: {[]assetSortKey{keyUndecided, keyUpdated, keyID}, true},
	models.AssetSortRelevance:      {[]assetSortKey{keyMatches, keyUpdated, keyID}, true},
}

// assetCursor is the position after the last row of a page.
type assetCursor struct {
	Sort string            `json:"s"`
	Keys []json.RawMessage `json:"k"`
}

func encodeAssetCursor(sort string, keys []json.RawMessage) string {
	b, _ := json.Marshal(assetCursor{Sort: sort, Keys: keys})
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeAssetCursor reads a cursor back into one SQL argument per sort key.
func decodeAssetCursor(raw, sort string) ([]any, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, ErrInvalidAssetCursor
	}
	var c assetCursor
	if err := json.Unmarshal(b, &c); err != nil || c.Sort != sort {
		return nil, ErrInvalidAssetCursor
	}
	keys := assetSorts[sort].keys
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
	var updated []string
	var updatedArgs []any
	if q.UpdatedAfter != nil {
		updated = append(updated, "f.updated_at >= ?")
		updatedArgs = append(updatedArgs, *q.UpdatedAfter)
	}
	if q.UpdatedBefore != nil {
		updated = append(updated, "f.updated_at < ?")
		updatedArgs = append(updatedArgs, *q.UpdatedBefore)
	}
	if len(updated) > 0 {
		preds = append(preds, assetPred{models.AssetFacetUpdated, strings.Join(updated, " AND "), updatedArgs})
	}
	if len(q.Checksums) > 0 {
		preds = append(preds, assetPred{"checksum", "f.checksum IN (?)", []any{bun.List(q.Checksums)}})
	}
	if q.Text != "" {
		preds = append(preds, assetPred{"text", "f.matches > 0", nil})
	}
	return preds
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
// Campaign membership agrees with how generation reads it: an asset belongs to
// every live campaign listing it, and to every campaign that draws on the
// whole bank (use_assets with no asset_ids). Ideas count only those of the
// analysis lenses in scope: the workspace lens plus every live campaign's,
// or under a campaign scope that campaign's alone.
func writeAssetFacts(b *sqlBuf, q models.AssetQuery, tenantID string) {
	b.add(`WITH camps_raw AS (
	SELECT c.id, c.use_assets,
		CASE WHEN jsonb_typeof(c.asset_ids) = 'array' THEN c.asset_ids ELSE '[]'::jsonb END AS asset_ids
	FROM campaigns c WHERE c.tenant_id = ? AND c.deleted_at IS NULL
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
		count(*) FILTER (WHERE i.verdict IS NULL OR (i.verdict = 'later' AND i.remind_at <= ?)) AS i_undecided,
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
	WHERE aa.tenant_id = ? AND `, tenantID, tenantID, q.Now, q.Now, tenantID, tenantID)
	if q.ScopeCampaign != "" {
		b.add(`(aa.campaign_id IS NULL OR aa.campaign_id = ?)`, q.ScopeCampaign)
	} else {
		b.add(`(aa.campaign_id IS NULL OR aa.campaign_id IN (SELECT c.id FROM camps c))`)
	}
	b.add(`
	GROUP BY aa.asset_id
)`)
	if q.Text != "" {
		b.add(`, hits AS MATERIALIZED (
	SELECT ac.asset_id, count(*) AS matches FROM assets_chunks ac
	WHERE ac.tenant_id = ? AND ac.content_tsv @@ websearch_to_tsquery('english', ?)
	GROUP BY ac.asset_id
)`, tenantID, q.Text)
	}
	b.add(`, facts AS MATERIALIZED (
	SELECT a.id, a.title, lower(a.title) AS title_key, a.updated_at,
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

// orderBy renders the sort's ORDER BY list.
func orderBy(sort string) string {
	s := assetSorts[sort]
	dir := " ASC"
	if s.desc {
		dir = " DESC"
	}
	parts := make([]string, len(s.keys))
	for i, k := range s.keys {
		parts[i] = k.expr + dir
	}
	return strings.Join(parts, ", ")
}

// afterCursor is the keyset condition for the rows after a cursor's keys.
func afterCursor(sort string, args []any) assetPred {
	s := assetSorts[sort]
	cols := make([]string, len(s.keys))
	vals := make([]string, len(s.keys))
	for i, k := range s.keys {
		cols[i] = k.expr
		vals[i] = "?::" + k.cast
	}
	op := " > "
	if s.desc {
		op = " < "
	}
	return assetPred{"cursor", "(" + strings.Join(cols, ", ") + ")" + op + "(" + strings.Join(vals, ", ") + ")", args}
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
	case models.AssetFacetUpdated:
		rows := make([]string, len(models.AssetUpdatedPresets))
		var args []any
		for i, p := range models.AssetUpdatedPresets {
			since, _ := models.AssetUpdatedSince(p, q.Now, q.Location)
			rows[i] = "(?::text, ?::timestamptz)"
			args = append(args, p, since)
		}
		b.add(`SELECT ?::text AS field, v.value, count(*) AS n FROM facts f
	JOIN (VALUES `+strings.Join(rows, ", ")+`) AS v(value, since) ON f.updated_at >= v.since`,
			append([]any{field}, args...)...)
		where(b, preds, field)
		b.add(` GROUP BY v.value`)
	}
}

// assetPageRow is one row of a page as the query returns it.
type assetPageRow struct {
	ID          string            `json:"id"`
	Kind        models.AssetKind  `json:"kind"`
	TagIDs      []string          `json:"tag_ids"`
	CampaignIDs []string          `json:"campaign_ids"`
	Undecided   int               `json:"i_undecided"`
	Yes         int               `json:"i_yes"`
	Later       int               `json:"i_later"`
	No          int               `json:"i_no"`
	Matches     int               `json:"matches"`
	Keys        []json.RawMessage `json:"k"`
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
		keys, err := decodeAssetCursor(q.Cursor, q.Sort)
		if err != nil {
			return nil, err
		}
		pagePreds = append(slices.Clip(preds), afterCursor(q.Sort, keys))
	}
	order := orderBy(q.Sort)

	var b sqlBuf
	writeAssetFacts(&b, q, tid)
	b.add(`SELECT (SELECT count(*) FROM facts f`)
	where(&b, preds, "")
	b.add(`) AS total,
	(SELECT count(*) FROM facts) AS total_unfiltered,
	(SELECT coalesce(json_agg(p ORDER BY p.n), '[]') FROM (
		SELECT f.id, f.kind, f.tag_ids, f.campaign_ids, f.i_undecided, f.i_yes, f.i_later, f.i_no, f.matches,
			json_build_array(`)
	keys := assetSorts[q.Sort].keys
	for i, k := range keys {
		if i > 0 {
			b.add(", ")
		}
		b.add(k.expr)
	}
	b.add(`) AS k, row_number() OVER (ORDER BY ` + order + `) AS n
		FROM facts f`)
	where(&b, pagePreds, "")
	b.add(` ORDER BY `+order+` LIMIT ?
	) p) AS page,
	`, q.Limit+1)

	var facets []string
	for _, field := range models.AssetFacets {
		if facetRequested(q, field) {
			facets = append(facets, field)
		}
	}
	if len(facets) == 0 {
		b.add(`'[]'::json AS facets,`)
	} else {
		b.add(`(SELECT coalesce(json_agg(x), '[]') FROM (`)
		for i, field := range facets {
			if i > 0 {
				b.add(` UNION ALL `)
			}
			assetFacetSQL(&b, field, preds, q)
		}
		b.add(`) x) AS facets,`)
	}
	b.add(`
	(SELECT to_json(t.ids) FROM live_tags t) AS tag_ids,
	(SELECT coalesce(json_agg(c.id), '[]') FROM camps c) AS campaign_ids,
	`)
	if q.ScopeCampaign != "" {
		b.add(`EXISTS (SELECT 1 FROM camps c WHERE c.id = ?) AS scope_found`, q.ScopeCampaign)
	} else {
		b.add(`TRUE AS scope_found`)
	}

	var row struct {
		Total           int             `bun:"total"`
		TotalUnfiltered int             `bun:"total_unfiltered"`
		Page            json.RawMessage `bun:"page"`
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
	res := &models.AssetQueryResult{Total: row.Total, TotalUnfiltered: row.TotalUnfiltered}
	if len(page) > q.Limit {
		page = page[:q.Limit]
		res.NextCursor = encodeAssetCursor(q.Sort, page[len(page)-1].Keys)
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

func (r *assetRepository) QueryIDs(ctx context.Context, q models.AssetQuery) ([]string, error) {
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	var b sqlBuf
	writeAssetFacts(&b, q, tid)
	b.add(`SELECT (SELECT coalesce(json_agg(f.id ORDER BY ` + orderBy(q.Sort) + `), '[]') FROM facts f`)
	where(&b, assetPreds(q), "")
	b.add(`) AS ids, `)
	if q.ScopeCampaign != "" {
		b.add(`EXISTS (SELECT 1 FROM camps c WHERE c.id = ?) AS scope_found`, q.ScopeCampaign)
	} else {
		b.add(`TRUE AS scope_found`)
	}
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
		case models.AssetFacetUpdated:
			values = models.AssetUpdatedPresets
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
		Column("a.id", "a.title", "a.type", "a.status", "a.failure_code", "a.source_url", "a.created_at", "a.updated_at").
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
		FailureCode: a.FailureCode, SourceURL: a.SourceURL, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
		Tags:        make([]models.Tag, 0, len(p.TagIDs)),
		CampaignIDs: append([]string{}, p.CampaignIDs...),
		IdeaCounts:  models.AssetIdeaCounts{Undecided: p.Undecided, Yes: p.Yes, Later: p.Later, No: p.No},
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
