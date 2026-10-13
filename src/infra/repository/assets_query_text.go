package repository

import (
	"context"
	"fmt"
	"html"
	"slices"
	"strings"
	"unicode"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Highlight delimiters ts_headline puts around matched words. Control
// characters, stripped from the content first, so they survive HTML escaping
// and can be swapped for <mark> afterwards without colliding with the text.
const (
	highlightStart = "\x02"
	highlightStop  = "\x03"
)

// textEvidenceSQL picks, for each asset in a page, its best matching chunk
// and headlines it. Ranking and ts_headline cost far more than finding the
// matches, so they run only for the page's assets, never for every hit.
const textEvidenceSQL = `
WITH q AS (
	SELECT websearch_to_tsquery('english', ?) AS tsq
), best AS (
	SELECT DISTINCT ON (ac.asset_id) ac.id, ts_rank_cd(ac.content_tsv, q.tsq, 32) AS score
	FROM assets_chunks ac CROSS JOIN q
	WHERE ac.tenant_id = ? AND ac.asset_id IN (?) AND ac.content_tsv @@ q.tsq
	ORDER BY ac.asset_id, score DESC, ac.chunk_index
), chunk AS (
	SELECT ac.asset_id, ac.chunk_index, ac.source_label, ac.page_start, ac.page_end,
		translate(ac.content, chr(2) || chr(3), '') AS content
	FROM best JOIN assets_chunks ac ON ac.id = best.id
)
SELECT chunk.asset_id, chunk.chunk_index, chunk.content, chunk.source_label, chunk.page_start, chunk.page_end,
	ts_headline('english', chunk.content, q.tsq,
		'StartSel=' || chr(2) || ', StopSel=' || chr(3) || ', MaxWords=30, MinWords=12, MaxFragments=1') AS headline
FROM chunk CROSS JOIN q`

// textEvidence returns where each of assetIDs best matches query, keyed by
// asset; Matches is left for the caller. Assets without a match are absent.
func (r *assetRepository) textEvidence(ctx context.Context, tenantID, query string, assetIDs []string) (map[string]models.AssetTextMatch, error) {
	var rows []struct {
		AssetID     string  `bun:"asset_id"`
		ChunkIndex  int     `bun:"chunk_index"`
		Content     string  `bun:"content"`
		SourceLabel *string `bun:"source_label"`
		PageStart   *int    `bun:"page_start"`
		PageEnd     *int    `bun:"page_end"`
		Headline    string  `bun:"headline"`
	}
	if err := r.db.NewRaw(textEvidenceSQL, query, tenantID, bun.List(assetIDs)).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]models.AssetTextMatch, len(rows))
	for _, row := range rows {
		out[row.AssetID] = models.AssetTextMatch{
			Snippet:     snippetHTML(row.Headline, row.Content),
			SourceLabel: chunkCitation(row.SourceLabel, row.PageStart, row.PageEnd),
			ChunkIndex:  row.ChunkIndex,
		}
	}
	return out, nil
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
