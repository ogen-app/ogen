package repository

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// ideaNone is the value of a field an idea has no value for: no source asset,
// no live campaign, no author.
const ideaNone = models.AssetFilterNone

// ideaSorts maps each sort order to its keys, the idea id last so the order is
// total. An unscored idea sorts as fit 0, below every score.
var ideaSorts = map[string][]assetSortKey{
	models.IdeaSortCreated: {
		{expr: "f.created_at", cast: "timestamptz"}, {expr: "f.id", cast: "text"},
	},
	models.IdeaSortCreatedDesc: {
		{expr: "f.created_at", cast: "timestamptz", desc: true}, {expr: "f.id", cast: "text", desc: true},
	},
	models.IdeaSortFit: {
		{expr: "f.fit_key", cast: "bigint", desc: true},
		{expr: "f.created_at", cast: "timestamptz"}, {expr: "f.id", cast: "text"},
	},
	models.IdeaSortWaiting: {
		{expr: "f.waiting_since", cast: "timestamptz"}, {expr: "f.id", cast: "text"},
	},
}

// ideaRowOrder is the keys a query's rows are ordered by: the group's when it
// groups, then the sort's.
func ideaRowOrder(q models.IdeaQuery) []assetSortKey {
	var keys []assetSortKey
	if q.Group != "" {
		keys = slices.Clone(groupKeys)
	}
	return append(keys, ideaSorts[q.Sort]...)
}

// ideaPreds turns a query's filters into conditions on a facts row f, one per
// field. Every filtered field but text and created is a facts column of the
// field's name.
func ideaPreds(q models.IdeaQuery) []assetPred {
	var preds []assetPred
	for field, f := range map[string]models.AssetValueFilter{
		"pile": q.Pile, models.IdeaFacetOrigin: q.Origin, models.IdeaFacetSourceAsset: q.SourceAsset,
		models.IdeaFacetKind: q.Kind, models.IdeaFacetCampaign: q.Campaign, models.IdeaFacetFit: q.Fit,
		models.IdeaFacetPhase: q.Phase, models.IdeaFacetAuthor: q.Author,
	} {
		if p, ok := inListPred(field, "f."+field, f); ok {
			preds = append(preds, p)
		}
	}
	// Map order is random; a stable order keeps the SQL text, and so its
	// plan cache entry, the same for the same query.
	slices.SortFunc(preds, func(a, b assetPred) int { return cmp.Compare(a.field, b.field) })
	if q.Text != "" {
		like := "%" + escapeLike(q.Text) + "%"
		preds = append(preds, assetPred{"text", `(f.title ILIKE ? ESCAPE '\' OR f.note ILIKE ? ESCAPE '\')`, []any{like, like}})
	}
	if p, ok := windowPred(models.IdeaFacetCreated, "f.created_at", q.CreatedAfter, q.CreatedBefore); ok {
		preds = append(preds, p)
	}
	return preds
}

// inListPred keeps the rows whose col is any of f.Any and none of f.Not;
// false when f does not filter.
func inListPred(field, col string, f models.AssetValueFilter) (assetPred, bool) {
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
	return assetPred{field, strings.Join(sql, " AND "), args}, len(sql) > 0
}

// filterCond renders every predicate but the one on field except as one
// condition, for a FILTER clause.
func filterCond(b *sqlBuf, preds []assetPred, except string) {
	n := 0
	for _, p := range preds {
		if p.field == except {
			continue
		}
		if n > 0 {
			b.add(" AND ")
		}
		b.add(p.sql, p.args...)
		n++
	}
	if n == 0 {
		b.add("TRUE")
	}
}

// writeIdeaFacts writes the facts CTE: one row per idea in scope with every
// value the filters, facets, groupings and sorts read. Fields an idea has no
// value for read as none (source asset, campaign) or as the empty string
// (kind, phase, author), so a filter excluding a value keeps them.
//
// The campaign is the live one the idea is attached to, as on every idea
// read. The author is the writer of a manual idea while they are a member.
func writeIdeaFacts(b *sqlBuf, q models.IdeaQuery, tenantID string) {
	b.add(`WITH facts AS MATERIALIZED (
	SELECT i.id, i.title, i.note, i.created_at,
		coalesce(i.fit_score, 0)::bigint AS fit_key,
		CASE WHEN `+undecidedIdeaSQL+` THEN ?::text ELSE i.verdict END AS pile,
		CASE WHEN i.origin = 'asset_analysis' THEN ?::text ELSE ?::text END AS origin,
		coalesce(a.id, ?::text) AS source_asset,
		coalesce(a.title, '') AS source_title,
		CASE WHEN a.id IS NULL THEN '' ELSE `+assetKindSQL+` END AS kind,
		coalesce(c.id, ?::text) AS campaign_id,
		coalesce(c.name, '') AS campaign_name,
		CASE WHEN i.fit_score IS NULL THEN ?::text WHEN i.fit_score >= ? THEN ?::text
			WHEN i.fit_score >= ? THEN ?::text ELSE ?::text END AS fit,
		coalesce(i.fit_breakdown -> 'criteria' -> 'phase_fit' ->> 'phase', '') AS phase,
		CASE WHEN i.origin = '' THEN coalesce(i.created_by, '') ELSE '' END AS author,
		CASE WHEN i.origin = '' AND i.created_by IS NOT NULL THEN lower(i.created_by_name) ELSE '' END AS author_name,
		CASE WHEN i.verdict = 'later' AND i.remind_at <= ? THEN i.remind_at ELSE i.created_at END AS waiting_since
	FROM ideas i
	LEFT JOIN campaigns c ON c.id = i.campaign_id AND c.tenant_id = ? AND c.deleted_at IS NULL
	LEFT JOIN assets a ON a.id = i.source_asset_id AND a.tenant_id = ?
	LEFT JOIN asset_files af ON af.asset_id = a.id
	WHERE i.tenant_id = ?`,
		q.Now, models.IdeaPileWaiting,
		models.IdeaOriginKeyAnalysis, models.IdeaOriginKeyManual,
		ideaNone, ideaNone,
		models.IdeaFitUnscored, models.IdeaFitHighMin, models.IdeaFitHigh,
		models.IdeaFitMediumMin, models.IdeaFitMedium, models.IdeaFitLow,
		q.Now, tenantID, tenantID, tenantID)
	if q.ScopeCampaign != "" {
		b.add(` AND c.id = ?`, q.ScopeCampaign)
	}
	b.add(`
)
`)
}

// writeIdeaGrouped writes the grouped CTE when q groups: every facts row with
// the g_rank, g_name and g_key of the one group it is listed under. Named
// groups (a source asset, a campaign, an author) go by name with none last;
// the others keep their vocabulary's order.
func writeIdeaGrouped(b *sqlBuf, q models.IdeaQuery) {
	named := func(key, name string) {
		b.add(`, grouped AS MATERIALIZED (
	SELECT f.*, (`+key+` = ?)::int::bigint AS g_rank, `+name+` AS g_name, `+key+` AS g_key FROM facts f
)
`, ideaNone)
	}
	fixed := func(keys []string, key string, args ...any) {
		b.add(`, grouped AS MATERIALIZED (
	SELECT f.*, array_position(?::text[], g.key)::bigint AS g_rank, ''::text AS g_name, g.key AS g_key
	FROM facts f CROSS JOIN LATERAL (SELECT `+key+` AS key) g
)
`, append([]any{pgdialect.Array(keys)}, args...)...)
	}
	switch q.Group {
	case models.IdeaGroupSource:
		named("f.source_asset", "lower(f.source_title)")
	case models.IdeaGroupCampaign:
		named("f.campaign_id", "lower(f.campaign_name)")
	case models.IdeaGroupAuthor:
		b.add(`, grouped AS MATERIALIZED (
	SELECT f.*, (f.author = '')::int::bigint AS g_rank, f.author_name AS g_name,
		CASE WHEN f.author = '' THEN ?::text ELSE f.author END AS g_key
	FROM facts f
)
`, ideaNone)
	case models.IdeaGroupFit:
		fixed(models.IdeaFitBands, "f.fit")
	case models.IdeaGroupOrigin:
		fixed(models.IdeaOriginKeys, "f.origin")
	case models.IdeaGroupCreated:
		keys := models.AssetCreatedGroupKeys
		var whens []string
		var args []any
		for _, band := range keys[:len(keys)-1] {
			since, _ := models.AssetDateSince(band, q.Now, q.Location)
			whens = append(whens, "WHEN f.created_at >= ? THEN ?::text")
			args = append(args, since, band)
		}
		fixed(keys, "CASE "+strings.Join(whens, " ")+" ELSE ?::text END", append(args, models.AssetCreatedEarlier)...)
	}
}

// ideaFacetRequested reports whether q asks for field's counts. The campaign
// facet is meaningless inside one campaign's scope, so it is never returned
// there.
func ideaFacetRequested(q models.IdeaQuery, field string) bool {
	if field == models.IdeaFacetCampaign && q.ScopeCampaign != "" {
		return false
	}
	return slices.Contains(q.Facets, field)
}

// ideaFacetSQL writes one field's counts as (field, value, n) rows, a value
// for every one some idea in scope has: those the other filters exclude count
// 0.
func ideaFacetSQL(b *sqlBuf, field string, preds []assetPred, q models.IdeaQuery) {
	if field == models.IdeaFacetCreated {
		rows := make([]string, len(models.AssetDatePresets))
		args := []any{field}
		for i, p := range models.AssetDatePresets {
			since, _ := models.AssetDateSince(p, q.Now, q.Location)
			rows[i] = "(?::text, ?::timestamptz)"
			args = append(args, p, since)
		}
		b.add(`SELECT ?::text AS field, v.value, count(*) FILTER (WHERE f.created_at >= v.since AND `, args[:1]...)
		filterCond(b, preds, field)
		b.add(`) AS n FROM facts f CROSS JOIN (VALUES `+strings.Join(rows, ", ")+`) AS v(value, since) GROUP BY v.value`, args[1:]...)
		return
	}
	b.add(`SELECT ?::text AS field, f.`+field+` AS value, count(*) FILTER (WHERE `, field)
	filterCond(b, preds, field)
	b.add(`) AS n FROM facts f GROUP BY f.` + field)
}

// ideaFacetRow is one (field, value, n) row of the facets and piles columns.
type ideaFacetRow struct {
	Field string `json:"field"`
	Value string `json:"value"`
	N     int    `json:"n"`
}

// ideaPageRow is one row of a page as the query returns it.
type ideaPageRow struct {
	ID       string            `json:"id"`
	GroupKey string            `json:"group_key"`
	Keys     []json.RawMessage `json:"k"`
}

// Query reads one page of the ideas list with its totals, piles, facets and
// groups in one statement.
func (r *ideaRepository) Query(ctx context.Context, q models.IdeaQuery) (*models.IdeaQueryResult, error) {
	// A raw query bypasses the TenantScoped hooks, so every table is scoped
	// here.
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	preds := ideaPreds(q)
	pagePreds := preds
	if q.Cursor != "" {
		vals, err := decodeCursor(q.Cursor, q.Sort, q.Group, ideaRowOrder(q))
		if err != nil {
			return nil, err
		}
		pagePreds = append(slices.Clip(preds), afterCursor(ideaRowOrder(q), vals))
	}

	var b sqlBuf
	writeIdeaFacts(&b, q, tid)
	if q.Group != "" {
		writeIdeaGrouped(&b, q)
	}
	b.add(`SELECT (SELECT count(*) FROM facts f`)
	where(&b, preds, "")
	b.add(`) AS total,
	(SELECT count(*) FROM facts) AS total_unfiltered,
	`)
	writeIdeaPage(&b, q, pagePreds)
	b.add(`,
	`)
	writeIdeaGroups(&b, q, preds)
	b.add(`,
	(SELECT coalesce(json_agg(x), '[]') FROM (`)
	ideaFacetSQL(&b, "pile", preds, q)
	b.add(`) x) AS piles,
	`)
	facets := writeIdeaFacets(&b, q, preds)
	b.add(`,
	`)
	writeIdeaSourceLabels(&b, q)
	b.add(`,
	`)
	writeIdeaScopeFound(&b, q, tid)

	var row struct {
		Total           int             `bun:"total"`
		TotalUnfiltered int             `bun:"total_unfiltered"`
		Page            json.RawMessage `bun:"page"`
		Groups          json.RawMessage `bun:"groups"`
		Piles           json.RawMessage `bun:"piles"`
		Facets          json.RawMessage `bun:"facets"`
		SourceLabels    json.RawMessage `bun:"source_labels"`
		ScopeFound      bool            `bun:"scope_found"`
	}
	if err := r.db.NewRaw(b.sb.String(), b.args...).Scan(ctx, &row); err != nil {
		return nil, err
	}
	if !row.ScopeFound {
		return nil, sql.ErrNoRows
	}
	return r.ideaQueryResult(ctx, q, facets, row.Total, row.TotalUnfiltered,
		row.Page, row.Groups, row.Piles, row.Facets, row.SourceLabels)
}

// ideaQueryResult decodes the columns of Query into its result and loads the
// page's ideas.
func (r *ideaRepository) ideaQueryResult(ctx context.Context, q models.IdeaQuery, facets []string,
	total, totalUnfiltered int, page, groups, piles, facetRows, sourceLabels json.RawMessage,
) (*models.IdeaQueryResult, error) {
	res := &models.IdeaQueryResult{Total: total, TotalUnfiltered: totalUnfiltered, Groups: []models.AssetGroupCount{}}
	var rows []ideaPageRow
	if err := json.Unmarshal(page, &rows); err != nil {
		return nil, fmt.Errorf("decode idea page: %w", err)
	}
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
		res.NextCursor = encodeCursor(q.Sort, q.Group, rows[len(rows)-1].Keys)
	}
	if q.Group != "" {
		if err := json.Unmarshal(groups, &res.Groups); err != nil {
			return nil, fmt.Errorf("decode idea groups: %w", err)
		}
	}
	var pileRows, fieldRows []ideaFacetRow
	if err := json.Unmarshal(piles, &pileRows); err != nil {
		return nil, fmt.Errorf("decode idea piles: %w", err)
	}
	if err := json.Unmarshal(facetRows, &fieldRows); err != nil {
		return nil, fmt.Errorf("decode idea facets: %w", err)
	}
	res.Piles = make(map[string]int, len(models.IdeaPiles))
	for _, p := range models.IdeaPiles {
		res.Piles[p] = 0
	}
	for _, p := range pileRows {
		res.Piles[p.Value] = p.N
	}
	res.Facets = ideaFacetCounts(fieldRows, facets)
	if slices.Contains(facets, models.IdeaFacetSourceAsset) {
		res.SourceAssetLabels = []models.IdeaFacetAsset{}
		if err := json.Unmarshal(sourceLabels, &res.SourceAssetLabels); err != nil {
			return nil, fmt.Errorf("decode idea source labels: %w", err)
		}
	}
	var err error
	if res.Ideas, err = r.pageIdeas(ctx, rows); err != nil {
		return nil, err
	}
	return res, nil
}

// writeIdeaPage writes the page column: the ids of the rows after the cursor,
// one more than the limit so the caller knows whether another page follows,
// each with its group and order keys.
func writeIdeaPage(b *sqlBuf, q models.IdeaQuery, preds []assetPred) {
	keys := ideaRowOrder(q)
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
		SELECT f.id, ` + groupKey + ` AS group_key, json_build_array(` + strings.Join(exprs, ", ") + `) AS k,
			row_number() OVER (ORDER BY ` + order + `) AS n
		FROM ` + from + ` f`)
	where(b, preds, "")
	b.add(` ORDER BY `+order+` LIMIT ?
	) p) AS page`, q.Limit+1)
}

// writeIdeaGroups writes the groups column: each non-empty group of the whole
// match with its row count, in display order.
func writeIdeaGroups(b *sqlBuf, q models.IdeaQuery, preds []assetPred) {
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

// writeIdeaFacets writes the facets column with the counts of every facet q
// asks for, and returns those fields.
func writeIdeaFacets(b *sqlBuf, q models.IdeaQuery, preds []assetPred) []string {
	var facets []string
	for _, field := range models.IdeaFacets {
		if ideaFacetRequested(q, field) {
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
		ideaFacetSQL(b, field, preds, q)
	}
	b.add(`) x) AS facets`)
	return facets
}

// writeIdeaSourceLabels writes the source_labels column: every source asset of
// the ideas in scope, with what labels it, when q asks for the source_asset
// facet.
func writeIdeaSourceLabels(b *sqlBuf, q models.IdeaQuery) {
	if !ideaFacetRequested(q, models.IdeaFacetSourceAsset) {
		b.add(`'[]'::json AS source_labels`)
		return
	}
	b.add(`(SELECT coalesce(json_agg(json_build_object('id', s.source_asset, 'title', s.source_title, 'kind', s.kind)
		ORDER BY lower(s.source_title), s.source_asset), '[]') FROM (
		SELECT DISTINCT f.source_asset, f.source_title, f.kind FROM facts f WHERE f.source_asset <> ?
	) s) AS source_labels`, ideaNone)
}

// writeIdeaScopeFound writes the scope_found column: whether a campaign scope
// names a live campaign.
func writeIdeaScopeFound(b *sqlBuf, q models.IdeaQuery, tid string) {
	if q.ScopeCampaign == "" {
		b.add(`TRUE AS scope_found`)
		return
	}
	b.add(`EXISTS (SELECT 1 FROM campaigns c WHERE c.id = ? AND c.tenant_id = ? AND c.deleted_at IS NULL) AS scope_found`,
		q.ScopeCampaign, tid)
}

// ideaFacetCounts turns the facet rows into each requested field's counts.
// Fixed vocabularies list every value in their order; the other fields list
// the values ideas in scope have, by count and then value, with none last
// where an idea can have no value.
func ideaFacetCounts(rows []ideaFacetRow, facets []string) map[string][]models.AssetFacetCount {
	out := make(map[string][]models.AssetFacetCount, len(facets))
	for _, field := range facets {
		counts := map[string]int{}
		var seen []string
		for _, r := range rows {
			if r.Field == field && r.Value != "" {
				counts[r.Value] = r.N
				seen = append(seen, r.Value)
			}
		}
		var values []string
		withNone := false
		switch field {
		case models.IdeaFacetOrigin:
			values = models.IdeaOriginKeys
		case models.IdeaFacetFit:
			values = models.IdeaFitBands
		case models.IdeaFacetKind:
			values = models.AssetKinds
		case models.IdeaFacetCreated:
			values = models.AssetDatePresets
		default:
			withNone = field == models.IdeaFacetSourceAsset || field == models.IdeaFacetCampaign
			values = slices.DeleteFunc(seen, func(v string) bool { return v == ideaNone })
			slices.SortFunc(values, func(a, b string) int {
				return cmp.Or(cmp.Compare(counts[b], counts[a]), cmp.Compare(a, b))
			})
		}
		list := make([]models.AssetFacetCount, 0, len(values)+1)
		for _, v := range values {
			list = append(list, models.AssetFacetCount{Value: v, Count: counts[v]})
		}
		if withNone {
			list = append(list, models.AssetFacetCount{Value: ideaNone, Count: counts[ideaNone]})
		}
		out[field] = list
	}
	return out
}

// pageIdeas loads a page's ideas in page order, each with its group key.
func (r *ideaRepository) pageIdeas(ctx context.Context, rows []ideaPageRow) ([]models.Idea, error) {
	out := make([]models.Idea, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	var loaded []models.Idea
	if err := r.selectIdeas(&loaded).Where("i.id IN (?)", bun.List(ids)).Scan(ctx); err != nil {
		return nil, err
	}
	byID := make(map[string]models.Idea, len(loaded))
	for _, idea := range loaded {
		byID[idea.ID] = idea
	}
	for _, row := range rows {
		// An idea deleted between the two reads drops out of the page.
		if idea, ok := byID[row.ID]; ok {
			idea.GroupKey = row.GroupKey
			out = append(out, idea)
		}
	}
	if err := r.hydrate(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// CountViews counts, for each query, its match and the waiting ideas in it, in
// one statement. A query whose campaign scope names no live campaign counts 0.
func (r *ideaRepository) CountViews(ctx context.Context, qs []models.IdeaQuery) ([]models.IdeaViewCounts, error) {
	if len(qs) == 0 {
		return nil, nil
	}
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, tenantctx.ErrNoTenant
	}
	var b sqlBuf
	b.add(`SELECT json_build_array(`)
	for i, q := range qs {
		if i > 0 {
			b.add(`, `)
		}
		preds := ideaPreds(q)
		b.add(`(`)
		writeIdeaFacts(&b, q, tid)
		b.add(`SELECT json_build_object('total', count(*) FILTER (WHERE `)
		filterCond(&b, preds, "")
		b.add(`), 'waiting', count(*) FILTER (WHERE f.pile = ? AND `, models.IdeaPileWaiting)
		filterCond(&b, preds, "")
		b.add(`)) FROM facts f)`)
	}
	b.add(`) AS counts`)
	var row struct {
		Counts json.RawMessage `bun:"counts"`
	}
	if err := r.db.NewRaw(b.sb.String(), b.args...).Scan(ctx, &row); err != nil {
		return nil, err
	}
	out := make([]models.IdeaViewCounts, 0, len(qs))
	if err := json.Unmarshal(row.Counts, &out); err != nil {
		return nil, fmt.Errorf("decode view counts: %w", err)
	}
	return out, nil
}
