package content_plan

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/firebase/genkit/go/ai"
	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// minAssetSimilarity is the minimum cosine similarity a chunk must score
// against the campaign query to be included in the prompt context.
// CON-101: a conservative starting threshold; revisit against Gemini Embedding
// 2's cosine distribution once there is real corpus data.
const minAssetSimilarity = 0.7

// assetIDsOf returns the distinct IDs of the assets actually retrieved into the
// generation context. These become each generated post's UsedAssetIDs — a
// binding grounded on what the model was given, not its self-reported claims
// (CON-118).
func assetIDsOf(assets []resolvedPiece) []string {
	if len(assets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(assets))
	out := make([]string, 0, len(assets))
	for _, a := range assets {
		if a.ID == "" {
			continue
		}
		if _, ok := seen[a.ID]; ok {
			continue
		}
		seen[a.ID] = struct{}{}
		out = append(out, a.ID)
	}
	return out
}

// idSet builds a lookup set from a slice of asset IDs — the retrieved-context
// grounding set used to validate each post's self-reported assetRefs (CON-118).
func idSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// groundedRefs filters a post's model-reported assetRefs (DraftPost.AssetRefs)
// down to the ids actually retrieved into the generation context, deduped and in
// the model's order. This is each post's UsedAssetIDs binding (CON-118): only
// the assets the model said it drew on for that specific post, and only those we
// can confirm were placed in its prompt — a hallucinated id is dropped, and a
// post that cited nothing records an empty list rather than inheriting the whole
// retrieved set. grounded is the id set from assetIDsOf, precomputed once per run.
func groundedRefs(refs []string, grounded map[string]struct{}) []string {
	out := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, id := range refs {
		if id == "" {
			continue
		}
		if _, ok := grounded[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// assetRefsOf projects the retrieved pieces into the deduped id+title provenance
// surfaced on ContentPlanResponse.UsedAssets (CON-118).
func assetRefsOf(assets []resolvedPiece) []AssetRef {
	if len(assets) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(assets))
	out := make([]AssetRef, 0, len(assets))
	for _, a := range assets {
		if a.ID == "" {
			continue
		}
		if _, ok := seen[a.ID]; ok {
			continue
		}
		seen[a.ID] = struct{}{}
		out = append(out, AssetRef{ID: a.ID, Title: a.Title})
	}
	return out
}

func resolveAssets(ctx context.Context, campaign *models.Campaign, cfg ContentPlanFlowConfig, repos ContentPlanRepos) ([]resolvedPiece, []string, error) {
	if !campaign.UseAssets {
		return nil, nil, nil
	}

	var warnings []string

	// Determine the candidate asset IDs. Assets in `failed` or `partial`
	// status are excluded — per CON-38, only fully embedded assets contribute
	// context.
	candidateIDs, skipWarnings, err := collectReadyCandidateIDs(ctx, campaign, repos)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, skipWarnings...)

	if len(candidateIDs) == 0 {
		warnings = append(warnings, "no ready assets available for this campaign — proceeding without asset context")
		return nil, warnings, nil
	}

	if cfg.MaxContextChars == 0 {
		cfg.MaxContextChars = 10000
	}

	// With an available embedder: rank chunks by semantic similarity and greedily
	// pack. When gemini_api_key is unset (CON-104) the embedder is unavailable —
	// fall through to creation order rather than failing every embed.
	if embedopts.Available(cfg.Embedder) {
		candidateSet := make(map[string]bool, len(candidateIDs))
		for _, id := range candidateIDs {
			candidateSet[id] = true
		}

		ranked, w, err := rankAndPackChunks(ctx, campaign, candidateSet, cfg, repos)
		if err != nil {
			slog.WarnContext(ctx, "chunk ranking failed, falling back to creation order", logging.AttrComponent, "genkit.content_plan", logging.AttrError, err)
			warnings = append(warnings, "chunk ranking unavailable; using creation order for asset selection")
		} else {
			warnings = append(warnings, w...)
			return ranked, warnings, nil
		}
	}

	// Fallback: no embedder — load assets in creation order up to MaxContextAssets.
	if len(candidateIDs) > cfg.MaxContextAssets {
		warnings = append(warnings, fmt.Sprintf("%d assets excluded due to context budget", len(candidateIDs)-cfg.MaxContextAssets))
		candidateIDs = candidateIDs[:cfg.MaxContextAssets]
	}

	result := make([]resolvedPiece, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		a, err := repos.Assets.GetByID(ctx, id)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("asset %q could not be loaded — skipped", id))
			continue
		}
		excerpt, err := buildExcerpt(a.Content)
		if err != nil {
			excerpt = "(content unavailable)"
		}
		result = append(result, resolvedPiece{ID: a.ID, Title: a.Title, Excerpt: excerpt})
	}
	return result, warnings, nil
}

// collectReadyCandidateIDs returns the set of asset IDs eligible for context
// injection, filtering out assets whose status is `failed` or `partial`. If
// the campaign has an explicit AssetIDs list, unknown/unready IDs produce
// warnings. If it does not, all `ready`/`processing`/`pending` assets are
// returned (pending/processing assets still contribute any chunks they
// already have — only definitive failure states are skipped).
func collectReadyCandidateIDs(ctx context.Context, campaign *models.Campaign, repos ContentPlanRepos) ([]string, []string, error) {
	var warnings []string

	isBad := func(status string) bool {
		return status == models.AssetStatusFailed || status == models.AssetStatusPartial
	}

	if len(campaign.AssetIDs) > 0 {
		out := make([]string, 0, len(campaign.AssetIDs))
		for _, id := range campaign.AssetIDs {
			a, err := repos.Assets.GetByID(ctx, id)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("asset %q could not be loaded — skipped", id))
				continue
			}
			if isBad(a.Status) {
				warnings = append(warnings, fmt.Sprintf("asset %q skipped: processing status=%s", id, a.Status))
				continue
			}
			out = append(out, a.ID)
		}
		return out, warnings, nil
	}

	all, err := repos.Assets.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]string, 0, len(all))
	for _, a := range all {
		if isBad(a.Status) {
			continue
		}
		out = append(out, a.ID)
	}
	return out, warnings, nil
}

// rankAndPackChunks fetches all embedded chunks, scores them against the
// campaign query, and greedily packs top-scoring chunks into the context budget.
// Multiple chunks from the same asset are merged in chunk_index order.
func rankAndPackChunks(
	ctx context.Context,
	campaign *models.Campaign,
	candidateSet map[string]bool,
	cfg ContentPlanFlowConfig,
	repos ContentPlanRepos,
) ([]resolvedPiece, []string, error) {
	var warnings []string

	// Embed the campaign query.
	query := campaign.Name + "\n" + campaign.KeyMessages + "\n" + campaign.Description
	qResp, err := cfg.Embedder.Embed(ctx, &ai.EmbedRequest{
		Input:   []*ai.Document{ai.DocumentFromText(query, nil)},
		Options: embedopts.Query(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("embed query: %w", err)
	}
	if len(qResp.Embeddings) == 0 {
		return nil, nil, fmt.Errorf("no embeddings returned for query")
	}
	queryVec := qResp.Embeddings[0].Embedding

	// pgvector ANN search: chunks scoped to the candidate assets, scored
	// against the campaign query, filtered at minAssetSimilarity and ordered
	// closest-first — all in-database via the HNSW index (replaces the
	// fetch-all-and-cosine-in-Go scan).
	candidateIDs := make([]string, 0, len(candidateSet))
	for id := range candidateSet {
		candidateIDs = append(candidateIDs, id)
	}
	ranked, err := repos.Chunks.SearchSimilar(ctx, pgvector.NewHalfVector(queryVec), candidateIDs, minAssetSimilarity, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("search similar chunks: %w", err)
	}
	slog.InfoContext(ctx, "pgvector search returned chunks", logging.AttrComponent, "genkit.content_plan", "chunks", len(ranked), "min_similarity", minAssetSimilarity)

	// Greedy packing: accumulate chunks (closest-first) until the context
	// budget is exhausted.
	type assetEntry struct {
		title  string
		chunks []models.AssetChunk
	}
	selected := make(map[string]*assetEntry)
	order := make([]string, 0)
	totalChars := 0

	for _, c := range ranked {
		if totalChars >= cfg.MaxContextChars {
			break
		}
		if _, exists := selected[c.AssetID]; !exists {
			selected[c.AssetID] = &assetEntry{}
			order = append(order, c.AssetID)
		}
		selected[c.AssetID].chunks = append(selected[c.AssetID].chunks, c)
		totalChars += len(c.Content)
	}

	if len(selected) == 0 {
		warnings = append(warnings, "no chunks met the similarity threshold; no asset context will be provided")
		return nil, warnings, nil
	}

	// Populate asset titles.
	for assetID, entry := range selected {
		a, err := repos.Assets.GetByID(ctx, assetID)
		if err != nil {
			entry.title = assetID // fallback to ID if asset can't be loaded
			continue
		}
		entry.title = a.Title
	}

	// Build resolved pieces: one entry per asset, chunks concatenated in order.
	result := make([]resolvedPiece, 0, len(order))
	for _, assetID := range order {
		entry := selected[assetID]

		slices.SortFunc(entry.chunks, func(a, b models.AssetChunk) int {
			return cmp.Compare(a.ChunkIndex, b.ChunkIndex)
		})

		parts := make([]chunkPart, 0, len(entry.chunks))
		for _, c := range entry.chunks {
			if c.Content == "" {
				continue
			}
			parts = append(parts, chunkPart{
				Content:   c.Content,
				PageStart: c.PageStart,
				PageEnd:   c.PageEnd,
			})
		}
		result = append(result, resolvedPiece{
			ID:      assetID,
			Title:   entry.title,
			Excerpt: joinPagedChunks(parts),
		})
	}

	return result, warnings, nil
}

// buildExcerpt truncates the Markdown content to ~800 characters for the
// no-embedder fallback path. Content is stored as Markdown, which is close
// enough to plain text for the model to consume directly.
func buildExcerpt(content string) (string, error) {
	const maxChars = 800
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content, nil
	}
	return string(runes[:maxChars]) + "…", nil
}

// chunkPart carries a chunk's text plus its optional source page range,
// used to produce page-attributed excerpts in the prompt.
type chunkPart struct {
	Content   string
	PageStart *int
	PageEnd   *int
}

// joinPagedChunks concatenates chunk texts with a visual separator, prefixing
// each chunk with its page citation when page info is available (e.g.
// "[p. 4]" for a single page, "[pp. 3–5]" for a range). Chunks without
// page info are rendered without a prefix.
func joinPagedChunks(parts []chunkPart) string {
	if len(parts) == 0 {
		return ""
	}
	render := func(p chunkPart) string {
		if p.PageStart == nil || *p.PageStart <= 0 {
			return p.Content
		}
		if p.PageEnd == nil || *p.PageEnd == *p.PageStart {
			return fmt.Sprintf("[p. %d] %s", *p.PageStart, p.Content)
		}
		return fmt.Sprintf("[pp. %d–%d] %s", *p.PageStart, *p.PageEnd, p.Content)
	}
	result := render(parts[0])
	for _, p := range parts[1:] {
		result += "\n\n[...]\n\n" + render(p)
	}
	return result
}
