package repository_test

import (
	"context"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/ideas"
)

// ideaQueryPerfSeed fills one tenant with 2,400 ideas over 300 assets, 20
// campaigns and 10 authors: the backlog size one list call must stay under
// 150 ms at.
var ideaQueryPerfSeed = []string{
	`INSERT INTO assets (id, tenant_id, title, content, status, type, tag_ids, created_by)
	SELECT 'ipa-' || g, ?0, 'Source ' || md5(g::text), '', 'ready',
		(ARRAY['PDF','URL','MD','DOC','AUDIO'])[1 + g % 5], '[]', 'u'
	FROM generate_series(1, 300) g`,
	`INSERT INTO asset_files (id, tenant_id, asset_id, original_name, mime_type, size_bytes, s3_key)
	SELECT 'ipf-' || g, ?0, 'ipa-' || g, 'f', 'application/pdf', 1, 'k/' || g
	FROM generate_series(1, 300) g WHERE g % 5 = 0`,
	`INSERT INTO campaigns (id, tenant_id, name, description, target_persona, key_messages, tone_guidelines,
		use_assets, asset_ids, target_platforms, campaign_type_id, language, publishing_days, created_by)
	SELECT 'ipc-' || c, ?0, 'Campaign ' || c, '', '', '', '', true, '[]', '[]', '', 'en', '[]', 'u'
	FROM generate_series(1, 20) c`,
	`INSERT INTO asset_analyses (id, tenant_id, asset_id, status, summary, gist)
	SELECT 'ipl-' || g, ?0, 'ipa-' || g, 'ready', '', '' FROM generate_series(1, 300) g`,
	`INSERT INTO ideas (id, tenant_id, title, note, campaign_id, verdict, remind_at, decided_at, origin,
		asset_analysis_id, source_asset_id, fit_score, fit_breakdown, created_by, created_by_name, created_at)
	SELECT 'ipi-' || g, ?0, 'Idea ' || md5(g::text), 'A note about ' || (ARRAY['bees','honey','hive','agents'])[1 + g % 4],
		CASE WHEN g % 3 = 0 THEN 'ipc-' || (1 + g % 20) END,
		(ARRAY[NULL,'yes','later','no'])[1 + g % 4],
		CASE WHEN g % 4 = 2 THEN now() + ((g % 3) - 1) * interval '1 day' END,
		CASE WHEN g % 4 <> 0 THEN now() END,
		CASE WHEN g % 5 = 0 THEN '' ELSE 'asset_analysis' END,
		CASE WHEN g % 5 <> 0 THEN 'ipl-' || (1 + g % 300) END,
		CASE WHEN g % 5 <> 0 THEN 'ipa-' || (1 + g % 300) END,
		CASE WHEN g % 5 <> 0 AND g % 7 <> 0 THEN 10 + g % 91 END,
		CASE WHEN g % 5 <> 0 THEN jsonb_build_object('lens', 'workspace', 'criteria',
			jsonb_build_object('phase_fit', jsonb_build_object('level', 3, 'phase', 'Phase ' || (g % 4)))) END,
		CASE WHEN g % 5 = 0 THEN 'ipu-' || (g % 10) END, 'Member',
		now() - (g % 200) * interval '1 day'
	FROM generate_series(1, 2400) g`,
}

// TestIdeaQueryPerformance is opt-in (IDEA_QUERY_PERF=1): it seeds a large
// backlog and reports each scenario's latency.
func TestIdeaQueryPerformance(t *testing.T) {
	if os.Getenv("IDEA_QUERY_PERF") == "" {
		t.Skip("set IDEA_QUERY_PERF=1 to run")
	}
	const tenant = "tenant-idea-query-perf"
	db := openMigratedDB(t)
	ctx := tenantctx.With(t.Context(), tenant)
	t.Cleanup(func() {
		for _, table := range []string{"ideas", "asset_analyses", "asset_files", "assets", "campaigns"} {
			_, _ = db.NewDelete().TableExpr(table).Where("tenant_id = ?", tenant).Exec(context.Background())
		}
	})
	for _, stmt := range ideaQueryPerfSeed {
		if _, err := db.NewRaw(stmt, tenant).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := db.Exec("ANALYZE ideas, assets, asset_files, campaigns, asset_analyses"); err != nil {
		t.Fatal(err)
	}
	repo := repository.NewIdeaRepository(db)
	all := "&facets=" + url.QueryEscape("origin,source_asset,kind,campaign_id,fit,phase,author,created")
	for _, sc := range []struct{ name, query string }{
		{"waiting, fit, every facet", "pile=waiting&sort=fit&limit=50" + all},
		{"grouped by source", "group=source&sort=fit&limit=50" + all},
		{"text and kind", "text=agents&kind=pdf,web&limit=50" + all},
		{"scoped to a campaign", "scope_campaign=ipc-3&group=fit&limit=50" + all},
		{"unpaged legacy read", ""},
	} {
		v, _ := url.ParseQuery(sc.query)
		q, err := ideas.ParseQuery(v, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		times := timeIdeaQuery(t, ctx, repo, q, 20)
		p50, p95 := times[len(times)/2], times[len(times)*95/100-1]
		t.Logf("%-28s p50 %6s  p95 %6s", sc.name, p50.Round(time.Millisecond), p95.Round(time.Millisecond))
		if sc.query != "" && p50 > 150*time.Millisecond {
			t.Errorf("%s: p50 %s over the 150 ms budget", sc.name, p50)
		}
	}
}

func timeIdeaQuery(t *testing.T, ctx context.Context, repo repository.IdeaRepository, q models.IdeaQuery, runs int) []time.Duration {
	t.Helper()
	times := make([]time.Duration, 0, runs)
	for range runs {
		start := time.Now()
		res, err := repo.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, time.Since(start))
		if res.TotalUnfiltered == 0 {
			t.Fatal("nothing seeded")
		}
	}
	slices.Sort(times)
	return times
}
