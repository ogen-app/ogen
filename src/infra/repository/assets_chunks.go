package repository

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// AssetChunksRepository handles persistence of asset text chunks and their
// embedding vectors.
type AssetChunksRepository interface {
	// UpsertChunks atomically replaces all chunks for the given asset.
	UpsertChunks(ctx context.Context, assetID string, chunks []models.AssetChunk) error
	// GetByAssetID returns all chunks for an asset, ordered by chunk_index,
	// without the embedding vector.
	GetByAssetID(ctx context.Context, assetID string) ([]models.AssetChunk, error)
	// PreviewByAssetID returns an asset's chunk count and the content of its
	// first chunk ("" when it has none) in one query.
	PreviewByAssetID(ctx context.Context, assetID string) (count int, first string, err error)
	// ListPageByAssetID returns one page of an asset's chunks ordered by
	// chunk_index, without the embedding vector, plus the asset's total chunk
	// count — the REST chunk view.
	ListPageByAssetID(ctx context.Context, assetID string, offset, limit int) ([]models.AssetChunk, int, error)
	// SearchSimilar returns embedded chunks (without the embedding vector)
	// ordered by cosine similarity to query (closest first), keeping only those scoring >= minScore. When
	// assetIDs is non-empty the search is scoped to those assets; limit <= 0
	// means no row cap. Backed by the pgvector HNSW index (embedding <=> query).
	SearchSimilar(ctx context.Context, query pgvector.HalfVector, assetIDs []string, minScore float64, limit int) ([]models.AssetChunk, error)
	// SearchText answers "which of the tenant's assets mention this text":
	// a full-text match of query (websearch syntax, so "quoted phrases" match as
	// phrases) against chunk content, one row per asset, the assets with the
	// most matching chunks first (ties by best-chunk score), at most limit rows,
	// plus the number of matching assets before the limit. Backed by the GIN
	// index on assets_chunks.content_tsv.
	SearchText(ctx context.Context, query string, limit int) ([]AssetTextMatch, int, error)
	// EmbeddingsByContent returns the stored embedding of each of the asset's
	// chunks whose content is in contents and which model embedded, keyed by
	// content, so a re-embed can skip text that hasn't changed.
	EmbeddingsByContent(ctx context.Context, assetID, model string, contents []string) (map[string]pgvector.HalfVector, error)
	// DeleteByAssetID removes all chunks for an asset.
	DeleteByAssetID(ctx context.Context, assetID string) error
	// GetByIDs returns chunks matching the given primary-key IDs, ordered by
	// asset_id then chunk_index, without the embedding vector.
	GetByIDs(ctx context.Context, ids []string) ([]models.AssetChunk, error)
}

type assetChunksRepository struct {
	db *bun.DB
}

func NewAssetChunksRepository(db *bun.DB) AssetChunksRepository {
	return &assetChunksRepository{db: db}
}

func (r *assetChunksRepository) UpsertChunks(ctx context.Context, assetID string, chunks []models.AssetChunk) error {
	ptrs := make([]*models.AssetChunk, len(chunks))
	for i := range chunks {
		ptrs[i] = &chunks[i]
	}

	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		delRes, err := tx.NewDelete().
			Model((*models.AssetChunk)(nil)).
			Where("asset_id = ?", assetID).
			Exec(ctx)
		if err != nil {
			return fmt.Errorf("delete existing chunks: %w", err)
		}
		delN, _ := delRes.RowsAffected()

		if len(ptrs) == 0 {
			slog.InfoContext(ctx, "upserted chunks, nothing to insert", logging.AttrComponent, "repository.assets_chunks", "asset_id", assetID, "deleted", delN)
			return nil
		}

		insRes, err := tx.NewInsert().Model(&ptrs).Exec(ctx)
		if err != nil {
			return fmt.Errorf("insert chunks: %w", err)
		}
		insN, _ := insRes.RowsAffected()
		slog.InfoContext(ctx, "upserted chunks", logging.AttrComponent, "repository.assets_chunks", "asset_id", assetID, "deleted", delN, "inserted", insN, "wanted", len(ptrs))
		return nil
	})
}

func (r *assetChunksRepository) GetByAssetID(ctx context.Context, assetID string) ([]models.AssetChunk, error) {
	var chunks []models.AssetChunk
	err := r.db.NewSelect().
		Model(&chunks).
		ExcludeColumn("embedding").
		Where("ac.asset_id = ?", assetID).
		OrderExpr("ac.chunk_index ASC").
		Scan(ctx)
	return chunks, err
}

func (r *assetChunksRepository) PreviewByAssetID(ctx context.Context, assetID string) (count int, first string, err error) {
	err = r.db.NewSelect().
		Model((*models.AssetChunk)(nil)).
		ColumnExpr("count(*)").
		ColumnExpr("coalesce((array_agg(ac.content ORDER BY ac.chunk_index))[1], '')").
		Where("ac.asset_id = ?", assetID).
		Scan(ctx, &count, &first)
	return count, first, err
}

func (r *assetChunksRepository) ListPageByAssetID(ctx context.Context, assetID string, offset, limit int) ([]models.AssetChunk, int, error) {
	chunks := []models.AssetChunk{}
	total, err := r.db.NewSelect().
		Model(&chunks).
		ExcludeColumn("embedding").
		Where("ac.asset_id = ?", assetID).
		OrderExpr("ac.chunk_index ASC").
		Offset(offset).
		Limit(limit).
		ScanAndCount(ctx)
	return chunks, total, err
}

func (r *assetChunksRepository) SearchSimilar(ctx context.Context, query pgvector.HalfVector, assetIDs []string, minScore float64, limit int) ([]models.AssetChunk, error) {
	var chunks []models.AssetChunk
	// `<=>` is pgvector's cosine-distance operator (0 = identical). Cosine
	// similarity is 1 - distance, so the threshold filter and the
	// closest-first ordering both key off the same expression.
	q := r.db.NewSelect().
		Model(&chunks).
		ExcludeColumn("embedding").
		Where("ac.embedding IS NOT NULL").
		Where("(1 - (ac.embedding <=> ?)) >= ?", query, minScore).
		OrderExpr("ac.embedding <=> ?", query)
	if len(assetIDs) > 0 {
		q = q.Where("ac.asset_id IN (?)", bun.List(assetIDs))
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return chunks, nil
}

// SearchTextEvidenceLimit is how many of SearchText's leading results carry
// evidence (score, snippet, source label, chunk index); the rest carry only the
// asset and its match count. Evidence means ranking every matching chunk of the
// asset and building a headline, which costs far more than finding the assets,
// so a filter can afford the full asset list but only a page of excerpts.
const SearchTextEvidenceLimit = 50

// AssetTextMatch is one asset's full-text hit: how many of its chunks match
// and, for the leading SearchTextEvidenceLimit results, where the best one is.
type AssetTextMatch struct {
	AssetID string `json:"asset_id"`
	// Matches is the number of the asset's chunks that match.
	Matches int `json:"matches"`
	// Score is the best chunk's cover-density rank scaled into (0, 1).
	Score float64 `json:"score,omitzero"`
	// Snippet is an excerpt of the best chunk around the match, with HTML tags
	// in the source dropped. It is HTML-escaped, with each matched word wrapped
	// in <mark>…</mark> (the only markup it contains), and starts or ends with
	// "…" when it was cut from a longer chunk.
	Snippet string `json:"snippet,omitempty"`
	// SourceLabel cites where the best chunk sits in its source ("Slide 4",
	// "1:05–1:40", "Page 14"). Absent when the chunk has no location, as for
	// markdown and scraped-URL chunks.
	SourceLabel *string `json:"source_label,omitempty"`
	// ChunkIndex is the best chunk's position in the asset.
	ChunkIndex *int `json:"chunk_index,omitempty"`
}

// Highlight delimiters ts_headline puts around matched words. Control
// characters, stripped from the content first, so they survive HTML escaping
// and can be swapped for <mark> afterwards without colliding with the text.
const (
	highlightStart = "\x02"
	highlightStop  = "\x03"
)

// searchTextSQL counts matching chunks per asset and keeps the limit assets
// with the most. Only the leading evidence-limit of those get their matching
// chunks ranked to pick a best chunk, whose content is read and headlined:
// ranking reads every hit's tsvector and ts_headline re-parses the whole
// chunk, so on a common word doing either for every matching asset costs
// several times the rest of the query. total is the number of matching assets
// before the limit.
const searchTextSQL = `
WITH q AS (
	SELECT websearch_to_tsquery('english', ?) AS tsq
), hits AS MATERIALIZED (
	SELECT ac.id, ac.asset_id, ac.chunk_index
	FROM assets_chunks ac, q
	WHERE ac.tenant_id = ? AND ac.content_tsv @@ q.tsq
), per_asset AS MATERIALIZED (
	SELECT asset_id, count(*) AS matches FROM hits GROUP BY asset_id
), top AS MATERIALIZED (
	SELECT asset_id, matches FROM per_asset ORDER BY matches DESC, asset_id LIMIT ?
), best AS (
	SELECT DISTINCT ON (hits.asset_id) hits.id, ts_rank_cd(ac.content_tsv, q.tsq, 32) AS score
	FROM (SELECT asset_id FROM top ORDER BY matches DESC, asset_id LIMIT ?) evidenced
	JOIN hits ON hits.asset_id = evidenced.asset_id
	JOIN assets_chunks ac ON ac.id = hits.id
	CROSS JOIN q
	ORDER BY hits.asset_id, score DESC, hits.chunk_index
), chunk AS (
	SELECT best.score, ac.asset_id, ac.chunk_index, ac.source_label, ac.page_start, ac.page_end,
		translate(ac.content, chr(2) || chr(3), '') AS content
	FROM best JOIN assets_chunks ac ON ac.id = best.id
)
SELECT top.asset_id, top.matches, (SELECT count(*) FROM per_asset) AS total,
	chunk.chunk_index, chunk.content, chunk.source_label, chunk.page_start, chunk.page_end, chunk.score,
	ts_headline('english', chunk.content, q.tsq,
		'StartSel=' || chr(2) || ', StopSel=' || chr(3) || ', MaxWords=30, MinWords=12, MaxFragments=1') AS headline
FROM top
LEFT JOIN chunk ON chunk.asset_id = top.asset_id
CROSS JOIN q
ORDER BY top.matches DESC, chunk.score DESC NULLS LAST, top.asset_id`

func (r *assetChunksRepository) SearchText(ctx context.Context, query string, limit int) ([]AssetTextMatch, int, error) {
	// A raw query bypasses the TenantScoped hooks, so scope it here.
	tid, ok := tenantctx.From(ctx)
	if !ok {
		return nil, 0, tenantctx.ErrNoTenant
	}
	var rows []struct {
		AssetID     string   `bun:"asset_id"`
		Matches     int      `bun:"matches"`
		Total       int      `bun:"total"`
		ChunkIndex  *int     `bun:"chunk_index"`
		Content     *string  `bun:"content"`
		SourceLabel *string  `bun:"source_label"`
		PageStart   *int     `bun:"page_start"`
		PageEnd     *int     `bun:"page_end"`
		Score       *float64 `bun:"score"`
		Headline    *string  `bun:"headline"`
	}
	if err := r.db.NewRaw(searchTextSQL, query, tid, limit, SearchTextEvidenceLimit).Scan(ctx, &rows); err != nil {
		return nil, 0, err
	}
	total := 0
	if len(rows) > 0 {
		total = rows[0].Total
	}
	out := make([]AssetTextMatch, len(rows))
	for i, row := range rows {
		out[i] = AssetTextMatch{AssetID: row.AssetID, Matches: row.Matches}
		if row.Score == nil {
			continue // past the evidence limit
		}
		out[i].Score = *row.Score
		out[i].Snippet = snippetHTML(*row.Headline, *row.Content)
		out[i].SourceLabel = chunkCitation(row.SourceLabel, row.PageStart, row.PageEnd)
		out[i].ChunkIndex = row.ChunkIndex
	}
	return out, total, nil
}

// snippetHTML turns a ts_headline excerpt of content into safe HTML: the text
// escaped, the highlight delimiters swapped for <mark>, and "…" on each side
// where the excerpt was cut from the chunk.
//
// The excerpt is compared word by word, not as a substring: ts_headline drops
// HTML tags and edge punctuation, so its text rarely appears verbatim.
func snippetHTML(headline, content string) string {
	plain := strings.NewReplacer(highlightStart, "", highlightStop, "").Replace(headline)
	marked := strings.NewReplacer(highlightStart, "<mark>", highlightStop, "</mark>").Replace(html.EscapeString(headline))
	excerpt, all := words(plain), words(content)
	if len(excerpt) == 0 || len(excerpt) > len(all) {
		return marked
	}
	if !slices.Equal(excerpt, all[:len(excerpt)]) {
		marked = "…" + marked
	}
	if !slices.Equal(excerpt, all[len(all)-len(excerpt):]) {
		marked += "…"
	}
	return marked
}

// words splits s into its runs of letters and digits.
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// chunkCitation is the chunk's stored source label, falling back to its page
// range for PDF chunks, which predate source labels. nil when neither exists.
func chunkCitation(label *string, pageStart, pageEnd *int) *string {
	switch {
	case label != nil && *label != "":
		return label
	case pageStart == nil:
		return nil
	case pageEnd == nil || *pageEnd == *pageStart:
		return new(fmt.Sprintf("Page %d", *pageStart))
	default:
		return new(fmt.Sprintf("Pages %d–%d", *pageStart, *pageEnd))
	}
}

func (r *assetChunksRepository) EmbeddingsByContent(ctx context.Context, assetID, model string, contents []string) (map[string]pgvector.HalfVector, error) {
	if len(contents) == 0 {
		return nil, nil
	}
	var rows []models.AssetChunk
	err := r.db.NewSelect().
		Model(&rows).
		Column("content", "embedding").
		Where("ac.asset_id = ?", assetID).
		Where("ac.model = ?", model).
		Where("ac.embedding IS NOT NULL").
		Where("ac.content IN (?)", bun.List(contents)).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]pgvector.HalfVector, len(rows))
	for i := range rows {
		out[rows[i].Content] = rows[i].Embedding
	}
	return out, nil
}

func (r *assetChunksRepository) DeleteByAssetID(ctx context.Context, assetID string) error {
	_, err := r.db.NewDelete().
		Model((*models.AssetChunk)(nil)).
		Where("asset_id = ?", assetID).
		Exec(ctx)
	return err
}

func (r *assetChunksRepository) GetByIDs(ctx context.Context, ids []string) ([]models.AssetChunk, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var chunks []models.AssetChunk
	err := r.db.NewSelect().
		Model(&chunks).
		ExcludeColumn("embedding").
		Where("ac.id IN (?)", bun.List(ids)).
		OrderExpr("ac.asset_id ASC, ac.chunk_index ASC").
		Scan(ctx)
	return chunks, err
}
