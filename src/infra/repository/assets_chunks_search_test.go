package repository_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// seedSearchChunks replaces assetID's chunks under tenant with one chunk per
// content, each tweaked by its index through edit (labels, pages, …).
func seedSearchChunks(t *testing.T, repo repository.AssetChunksRepository, tenant, assetID string, contents []string, edit func(i int, c *models.AssetChunk)) {
	t.Helper()
	chunks := make([]models.AssetChunk, len(contents))
	for i, content := range contents {
		chunks[i] = models.AssetChunk{
			ID: fmt.Sprintf("%s:%d", assetID, i), AssetID: assetID, ChunkIndex: i,
			Content: content, TokenCount: len(strings.Fields(content)), Model: "m",
			Embedding: pgvector.NewHalfVector(make([]float32, 3072)),
		}
		if edit != nil {
			edit(i, &chunks[i])
		}
	}
	if err := repo.UpsertChunks(tenantctx.With(t.Context(), tenant), assetID, chunks); err != nil {
		t.Fatalf("seed %s: %v", assetID, err)
	}
}

func cleanupSearchChunks(t *testing.T, db *bun.DB, tenants ...string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.NewDelete().TableExpr("assets_chunks").Where("tenant_id IN (?)", bun.List(tenants)).Exec(context.Background())
	})
}

func searchByAsset(t *testing.T, repo repository.AssetChunksRepository, tenant, query string, limit int) map[string]repository.AssetTextMatch {
	t.Helper()
	got, total, err := repo.SearchText(tenantctx.With(t.Context(), tenant), query, limit)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	if total < len(got) {
		t.Fatalf("search %q: total %d below the %d results", query, total, len(got))
	}
	out := make(map[string]repository.AssetTextMatch, len(got))
	for _, m := range got {
		out[m.AssetID] = m
	}
	return out
}

func TestAssetChunks_SearchTextAcrossSourceKinds(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAssetChunksRepository(db)
	const tenant = "tenant-search-kinds"
	cleanupSearchChunks(t, db, tenant)

	label := func(s string) func(int, *models.AssetChunk) {
		return func(_ int, c *models.AssetChunk) { c.SourceLabel = &s }
	}
	seedSearchChunks(t, repo, tenant, "pdf", []string{"Colonies below -20 °C need overwintering insulation."}, func(_ int, c *models.AssetChunk) {
		c.PageStart, c.PageEnd = new(14), new(14)
	})
	seedSearchChunks(t, repo, tenant, "pdf-span", []string{"An overwintering plan that runs over a page break."}, func(_ int, c *models.AssetChunk) {
		c.PageStart, c.PageEnd = new(3), new(4)
	})
	seedSearchChunks(t, repo, tenant, "docx", []string{"Overwintering checklist for new keepers."}, label("Guide > Winter"))
	seedSearchChunks(t, repo, tenant, "sheet", []string{"hive 12 | overwintering | ok"}, label("Sheet 'Q3' rows 10–24"))
	seedSearchChunks(t, repo, tenant, "url", []string{"Our blog post on overwintering bees."}, nil)
	seedSearchChunks(t, repo, tenant, "audio", []string{"so overwintering is really about moisture"}, label("1:05–1:40"))
	seedSearchChunks(t, repo, tenant, "unrelated", []string{"Spring swarm control."}, nil)

	got := searchByAsset(t, repo, tenant, "overwintering", 50)
	want := map[string]string{
		"pdf": "Page 14", "pdf-span": "Pages 3–4", "docx": "Guide > Winter",
		"sheet": "Sheet 'Q3' rows 10–24", "url": "", "audio": "1:05–1:40",
	}
	if len(got) != len(want) {
		t.Fatalf("search returned %d assets, want %d: %v", len(got), len(want), got)
	}
	for id, wantLabel := range want {
		m, ok := got[id]
		if !ok {
			t.Errorf("asset %s missing from results", id)
			continue
		}
		gotLabel := ""
		if m.SourceLabel != nil {
			gotLabel = *m.SourceLabel
		}
		if gotLabel != wantLabel {
			t.Errorf("asset %s source_label = %q, want %q", id, gotLabel, wantLabel)
		}
		if !strings.Contains(strings.ToLower(m.Snippet), "<mark>overwintering</mark>") {
			t.Errorf("asset %s snippet %q does not mark the match", id, m.Snippet)
		}
		if m.Matches != 1 || m.Score <= 0 || m.Score >= 1 {
			t.Errorf("asset %s matches/score = %d/%v, want 1 and (0,1)", id, m.Matches, m.Score)
		}
	}

	if stemmed := searchByAsset(t, repo, tenant, "Overwinter", 50); len(stemmed) != len(want) {
		t.Errorf("stemmed search returned %d assets, want %d", len(stemmed), len(want))
	}
}

func TestAssetChunks_SearchTextRanksCountsAndLimits(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAssetChunksRepository(db)
	const tenant = "tenant-search-rank"
	cleanupSearchChunks(t, db, tenant)

	seedSearchChunks(t, repo, tenant, "many", []string{
		"Varroa notes.",
		"Varroa counts after the varroa treatment, varroa everywhere.",
		"A short varroa aside.",
		"Nothing here.",
	}, nil)
	seedSearchChunks(t, repo, tenant, "once", []string{"One varroa mention buried in a much longer chunk about queen rearing, drones and honey flow timing."}, nil)
	seedSearchChunks(t, repo, tenant, "dense", []string{"Varroa, varroa, varroa."}, nil)

	got, total, err := repo.SearchText(tenantctx.With(t.Context(), tenant), "varroa", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	// Most matching chunks first; between equal counts, the denser best chunk.
	if total != 3 || len(got) != 3 || got[0].AssetID != "many" || got[1].AssetID != "dense" || got[2].AssetID != "once" {
		t.Fatalf("results = %+v (total %d), want many, dense, once", got, total)
	}
	if got[1].Score <= got[2].Score {
		t.Errorf("dense score %v not above once score %v", got[1].Score, got[2].Score)
	}
	if got[0].Matches != 3 || got[0].ChunkIndex == nil || *got[0].ChunkIndex != 1 {
		t.Errorf("many = %+v, want 3 matches with chunk 1 the best", got[0])
	}

	limited, total, err := repo.SearchText(tenantctx.With(t.Context(), tenant), "varroa", 1)
	if err != nil || len(limited) != 1 || limited[0].AssetID != "many" || total != 3 {
		t.Fatalf("limit 1 = (%+v, total %d, %v), want only the asset with most matches and total 3", limited, total, err)
	}

	stop, total, err := repo.SearchText(tenantctx.With(t.Context(), tenant), "the and of", 50)
	if err != nil || len(stop) != 0 || total != 0 {
		t.Fatalf("stopword-only search = (%+v, total %d, %v), want no results and no error", stop, total, err)
	}
}

func TestAssetChunks_SearchTextEvidenceOnlyOnLeadingResults(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAssetChunksRepository(db)
	const tenant = "tenant-search-evidence"
	cleanupSearchChunks(t, db, tenant)

	// One asset with two matching chunks leads; the rest match once each.
	seedSearchChunks(t, repo, tenant, "lead", []string{"Nectar flow.", "Nectar again."}, nil)
	n := repository.SearchTextEvidenceLimit + 5
	for i := range n - 1 {
		seedSearchChunks(t, repo, tenant, fmt.Sprintf("asset-%03d", i), []string{"Nectar sources."}, nil)
	}

	got, total, err := repo.SearchText(tenantctx.With(t.Context(), tenant), "nectar", 1000)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if total != n || len(got) != n || got[0].AssetID != "lead" || got[0].Matches != 2 {
		t.Fatalf("got %d results (total %d), first %+v; want all %d with lead first", len(got), total, got[0], n)
	}
	for i, m := range got {
		withEvidence := m.Snippet != "" && m.Score > 0 && m.ChunkIndex != nil
		if want := i < repository.SearchTextEvidenceLimit; withEvidence != want {
			t.Errorf("result %d (%s): evidence = %v, want %v", i, m.AssetID, withEvidence, want)
		}
	}
}

func TestAssetChunks_SearchTextPhrases(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAssetChunksRepository(db)
	const tenant = "tenant-search-phrase"
	cleanupSearchChunks(t, db, tenant)

	seedSearchChunks(t, repo, tenant, "phrase", []string{"Where AI sits on the Hype Cycle this year."}, nil)
	seedSearchChunks(t, repo, tenant, "apart", []string{"The sales cycle ignored the hype entirely."}, nil)

	if got := searchByAsset(t, repo, tenant, `"hype cycle"`, 50); len(got) != 1 || got["phrase"].AssetID == "" {
		t.Errorf(`"hype cycle" matched %v, want only the phrase asset`, got)
	}
	if got := searchByAsset(t, repo, tenant, "hype cycle", 50); len(got) != 2 {
		t.Errorf("unquoted hype cycle matched %d assets, want both", len(got))
	}
}

func TestAssetChunks_SearchTextSnippetIsSafeHTML(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAssetChunksRepository(db)
	const tenant = "tenant-search-snippet"
	cleanupSearchChunks(t, db, tenant)

	filler := strings.Repeat("lorem ipsum dolor sit amet ", 20)
	seedSearchChunks(t, repo, tenant, "html", []string{
		filler + `<script>alert(1)</script> 1 < 2 & "propolis" ` + "\x02stray\x03 " + filler,
	}, nil)

	got := searchByAsset(t, repo, tenant, "propolis", 50)["html"]
	if got.Snippet == "" {
		t.Fatal("no result for propolis")
	}
	// ts_headline drops tags itself; what text is left must come back escaped,
	// and stray delimiter characters in the content must not turn into marks.
	for _, bad := range []string{"<script", "</script", "\x02", "\x03", `"`} {
		if strings.Contains(got.Snippet, bad) {
			t.Errorf("snippet %q contains %q", got.Snippet, bad)
		}
	}
	for _, escaped := range []string{"&lt;", "&amp;", "&#34;"} {
		if !strings.Contains(got.Snippet, escaped) {
			t.Errorf("snippet %q lacks %q: not HTML-escaped", got.Snippet, escaped)
		}
	}
	if strings.Count(got.Snippet, "<mark>") != 1 {
		t.Errorf("snippet %q should mark exactly the one match", got.Snippet)
	}
	if !strings.Contains(got.Snippet, "<mark>propolis</mark>") {
		t.Errorf("snippet %q does not mark the match", got.Snippet)
	}
	if !strings.HasPrefix(got.Snippet, "…") || !strings.HasSuffix(got.Snippet, "…") {
		t.Errorf("snippet %q cut from the middle of a chunk should carry ellipses", got.Snippet)
	}

	seedSearchChunks(t, repo, tenant, "short", []string{"Royal jelly basics."}, nil)
	short := searchByAsset(t, repo, tenant, "jelly", 50)["short"]
	if short.Snippet != "Royal <mark>jelly</mark> basics" { // ts_headline trims edge punctuation
		t.Errorf("whole-chunk snippet = %q, want no ellipses", short.Snippet)
	}
}

func TestAssetChunks_SearchTextIsTenantScoped(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewAssetChunksRepository(db)
	const tenantA, tenantB = "tenant-search-a", "tenant-search-b"
	cleanupSearchChunks(t, db, tenantA, tenantB)

	seedSearchChunks(t, repo, tenantA, "a-asset", []string{"Beeswax wraps for tenant A."}, nil)
	seedSearchChunks(t, repo, tenantB, "b-asset", []string{"Beeswax candles for tenant B."}, nil)

	if got := searchByAsset(t, repo, tenantA, "beeswax", 50); len(got) != 1 || got["a-asset"].AssetID == "" {
		t.Errorf("tenant A saw %v, want only its own asset", got)
	}
	if got := searchByAsset(t, repo, tenantB, "beeswax", 50); len(got) != 1 || got["b-asset"].AssetID == "" {
		t.Errorf("tenant B saw %v, want only its own asset", got)
	}
	if _, _, err := repo.SearchText(context.Background(), "beeswax", 50); !errors.Is(err, tenantctx.ErrNoTenant) {
		t.Errorf("search without a tenant = %v, want ErrNoTenant", err)
	}
}
