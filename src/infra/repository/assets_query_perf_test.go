package repository_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// assetQueryPerfSeed fills one tenant with 5,000 assets, 100,000 chunks and
// 20,000 analysis ideas, plus tags and campaigns, the size the list must stay
// fast at.
var assetQueryPerfSeed = []string{
	`INSERT INTO tags (id, tenant_id, name, color, created_by)
	SELECT 'pt-' || g, ?0, 'Tag ' || g, '#000000', 'u' FROM generate_series(1, 30) g`,
	`INSERT INTO assets (id, tenant_id, title, content, status, type, tag_ids, created_by, created_at, updated_at)
	SELECT 'pa-' || g, ?0, 'Asset ' || g || ' ' || md5(g::text), '',
		(ARRAY['ready','ready','ready','partial','pending','processing','failed'])[1 + g % 7],
		(ARRAY['PDF','URL','MD','DOC','AUDIO','IMG'])[1 + g % 6],
		CASE WHEN g % 4 = 0 THEN '[]'::jsonb
			ELSE jsonb_build_array('pt-' || (1 + g % 30), 'pt-' || (1 + (g * 7) % 30)) END,
		'u', now() - (g % 365) * interval '1 day', now() - (g % 365) * interval '1 day' - (g % 1440) * interval '1 minute'
	FROM generate_series(1, 5000) g`,
	`INSERT INTO asset_files (id, tenant_id, asset_id, original_name, mime_type, size_bytes, s3_key)
	SELECT 'pf-' || g, ?0, 'pa-' || g, 'f', (ARRAY['application/pdf','text/csv','application/vnd.openxmlformats-officedocument.spreadsheetml.sheet'])[1 + g % 3], 1, 'k/' || g
	FROM generate_series(1, 5000) g WHERE g % 6 IN (0, 3, 4, 5)`,
	`INSERT INTO campaigns (id, tenant_id, name, description, target_persona, key_messages, tone_guidelines,
		use_assets, asset_ids, target_platforms, campaign_type_id, language, publishing_days, created_by)
	SELECT 'pc-' || c, ?0, 'Campaign ' || c, '', '', '', '', c <> 1,
		CASE WHEN c = 2 THEN '[]'::jsonb
			ELSE (SELECT jsonb_agg('pa-' || g) FROM generate_series(c * 10, 5000, 25) g) END,
		'[]', '', 'en', '[]', 'u'
	FROM generate_series(1, 20) c`,
	`INSERT INTO assets_chunks (id, tenant_id, asset_id, chunk_index, content, token_count, model)
	SELECT 'pk-' || a || '-' || i, ?0, 'pa-' || a, i,
		'Chunk ' || i || ' of asset ' || a || ' covers ' ||
		(ARRAY['bees','honey','hive','pricing','winter','queen','swarm','pollen'])[1 + (a + i) % 8] ||
		CASE WHEN (a * 20 + i) % 50 = 0 THEN ' and overwintering colonies' ELSE '' END ||
		' lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua ut enim ad minim veniam quis nostrud exercitation ullamco laboris',
		40, 'm'
	FROM generate_series(1, 5000) a, generate_series(0, 19) i`,
	`INSERT INTO asset_analyses (id, tenant_id, asset_id, status, summary, gist)
	SELECT 'pl-' || g, ?0, 'pa-' || g, CASE WHEN g % 10 = 0 THEN 'pending' ELSE 'ready' END, 'A summary. More.', ''
	FROM generate_series(1, 5000) g`,
	`INSERT INTO ideas (id, tenant_id, title, note, verdict, remind_at, decided_at, origin, asset_analysis_id, created_by_name)
	SELECT 'pi-' || a || '-' || i, ?0, 'Idea', '',
		(ARRAY[NULL,'yes','later','no'])[1 + (a + i) % 4],
		CASE WHEN (a + i) % 4 = 2 THEN now() + ((a % 3) - 1) * interval '1 day' END,
		CASE WHEN (a + i) % 4 <> 0 THEN now() END,
		'asset_analysis', 'pl-' || a, 'u'
	FROM generate_series(1, 5000) a, generate_series(1, 4) i`,
	`ANALYZE`,
}

// TestAssetQuery_Performance measures the list at the PRD's target size. It
// seeds about a minute's worth of data, so it runs only with ASSET_QUERY_PERF
// set. The budget is checked against the main statement's server-side time;
// wall-clock p50/p95 (which add the hydration round trips and whatever else
// the machine is doing) are logged. ASSET_QUERY_EXPLAIN also logs the plan.
func TestAssetQuery_Performance(t *testing.T) {
	if os.Getenv("ASSET_QUERY_PERF") == "" {
		t.Skip("set ASSET_QUERY_PERF=1 to measure the asset list at 5k assets / 100k chunks / 20k ideas")
	}
	db := openMigratedDB(t)
	const tenant = "tenant-asset-perf"
	for _, stmt := range assetQueryPerfSeed {
		if _, err := db.ExecContext(t.Context(), stmt, tenant); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
	repo := repository.NewAssetRepository(db, repository.NewTagRepository(db), repository.NewAssetFileRepository(db))
	ctx := tenantctx.With(t.Context(), tenant)

	scenarios := []struct {
		name   string
		q      models.AssetQuery
		budget time.Duration // 0 only reports
	}{
		{"default page, all facets", models.AssetQuery{Facets: models.AssetFacets}, 250 * time.Millisecond},
		{"filtered page, all facets", models.AssetQuery{
			Facets: models.AssetFacets, Tag: anyOf("pt-3", "pt-7"), Kind: noneOf("image"),
			Ideas: anyOf("undecided"), UpdatedAfter: new(time.Now().AddDate(0, 0, -90)),
		}, 250 * time.Millisecond},
		{"campaign scope, all facets", models.AssetQuery{Facets: models.AssetFacets, ScopeCampaign: "pc-5"}, 250 * time.Millisecond},
		{"undecided sort, page 1", models.AssetQuery{Sort: models.AssetSortIdeasUndecided}, 250 * time.Millisecond},
		{"rare text, all facets", models.AssetQuery{Facets: models.AssetFacets, Text: "overwintering", Sort: models.AssetSortRelevance}, 0},
		{"common text, all facets", models.AssetQuery{Facets: models.AssetFacets, Text: "lorem"}, 0},
	}
	hook := &lastQueryHook{}
	db.AddQueryHook(hook)
	// serverTime is the best of a few server-side executions of the captured
	// main statement: wall-clock latency on a busy machine mostly measures the
	// machine, this mostly measures the query.
	serverTime := func(sql string) time.Duration {
		best := time.Duration(0)
		for range 5 {
			var plan []string
			if err := db.NewRaw("EXPLAIN (ANALYZE, TIMING OFF) "+sql).Scan(ctx, &plan); err != nil {
				t.Fatal(err)
			}
			for _, line := range plan {
				if ms, ok := strings.CutPrefix(line, "Execution Time: "); ok {
					d, _ := time.ParseDuration(strings.ReplaceAll(strings.TrimSuffix(ms, " ms"), " ", "") + "ms")
					if best == 0 || d < best {
						best = d
					}
				}
			}
		}
		return best
	}
	const runs = 20
	for i, sc := range scenarios {
		q := sc.q
		q.Now, q.Location, q.Limit = time.Now(), time.UTC, 50
		if q.Sort == "" {
			q.Sort = models.AssetSortUpdatedDesc
		}
		hook.main = ""
		if _, err := repo.Query(ctx, q); err != nil {
			t.Fatal(err)
		}
		server := serverTime(hook.main)
		if i == 0 && os.Getenv("ASSET_QUERY_EXPLAIN") != "" {
			var plan []string
			if err := db.NewRaw("EXPLAIN (ANALYZE, BUFFERS) "+hook.main).Scan(ctx, &plan); err != nil {
				t.Fatal(err)
			}
			t.Log("\n" + strings.Join(plan, "\n"))
		}
		times := make([]time.Duration, 0, runs)
		for range runs {
			start := time.Now()
			res, err := repo.Query(ctx, q)
			if err != nil {
				t.Fatalf("%s: %v", sc.name, err)
			}
			times = append(times, time.Since(start))
			if res.TotalUnfiltered == 0 {
				t.Fatalf("%s: nothing seeded", sc.name)
			}
		}
		slices.Sort(times)
		p50, p95 := times[runs/2], times[runs*95/100-1]
		t.Logf("%-28s p50 %6s  p95 %6s  server best %6s", sc.name,
			p50.Round(time.Millisecond), p95.Round(time.Millisecond), server.Round(time.Millisecond))
		if sc.budget > 0 && server > sc.budget {
			t.Errorf("%s: server time %s over the %s budget", sc.name, server, sc.budget)
		}
	}
}

// lastQueryHook remembers the first query that starts with WITH: the list's
// main statement.
type lastQueryHook struct{ main string }

func (h *lastQueryHook) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	if h.main == "" && strings.HasPrefix(e.Query, "WITH") {
		h.main = e.Query
	}
	return ctx
}

func (h *lastQueryHook) AfterQuery(context.Context, *bun.QueryEvent) {}
