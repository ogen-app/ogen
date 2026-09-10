//go:build integration

package integration_test

import (
	"context"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// content_plan_batching_test exercises the parallel-batched generation path
// (CON-67). The existing content_plan_test runs with the default K (30), so
// any campaign small enough to test cheaply ends up in a single batch and
// never hits the batched code path. Here we deliberately set
// MaxPostsPerBatch=5 with EstimatedPostCount=10 to force two batches against
// the real Anthropic API.
//
// Cost note: 2 batches × ~5 posts × ~500 tokens ≈ 5K output tokens per run on
// haiku — roughly $0.005 per execution. Cheap, but skipped without a key.
var _ = Describe("Content plan flow — parallel batched generation", Ordered, func() {
	var (
		ctx        context.Context
		db         *bun.DB
		userID     string
		campaignID string
		platformID = "AXqWG7U2qnpt" // seeded LinkedIn platform (Sqid)
	)

	const (
		// EstimatedPostCount=10 with K=5 forces two batches; the awareness
		// campaign type (Uk) has 2 phases, so each batch's slot mix is
		// non-trivial (5 slots split across 2 phases).
		estimatedCount   = 10
		maxPostsPerBatch = 5
		// Two batches is enough to exercise the fan-out path; capping
		// parallel at 2 keeps Anthropic ITPM bursts modest.
		maxParallelBatch = 2
	)

	BeforeAll(func() {
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			Skip("ANTHROPIC_API_KEY not set — skipping batching integration tests")
		}

		ctx = tenantCtx()
		db = mustOpenIntegrationDB()

		tagRepo := repository.NewTagRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		chunksRepo := repository.NewAssetChunksRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)
		postRepo := repository.NewPostRepository(db)

		// Seed user (separate from the non-batched suite to avoid email
		// collisions when both suites run in the same process).
		var err error
		userID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "cp-batching@test.local",
			PasswordHash: "placeholder",
			Name:         "Batching Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		user := &models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Batching Tester",
			Email:     "cp-batching@test.local",
		}
		_, err = db.NewInsert().Model(user).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		// 21-day window so each phase has ~10 days even after the slot
		// allocator splits — keeps publishDate distribution comfortable
		// against a 2-phase awareness campaign type.
		start := time.Now().UTC().Truncate(24 * time.Hour)
		end := start.Add(21 * 24 * time.Hour)
		estCount := estimatedCount
		campaignID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		campaign := &models.Campaign{
			ID:             campaignID,
			Name:           "Batching Integration Test Campaign",
			Description:    "A campaign forcing the parallel-batched generation path.",
			TargetPersona:  "Engineering managers evaluating new dev-tooling SaaS.",
			KeyMessages:    "Faster onboarding, fewer footguns, transparent pricing.",
			ToneGuidelines: "Direct, technical, no marketing hype.",
			CampaignTypeID: "Uk", // awareness — 2 phases ('98' and 'xh')
			Status:         models.StatusDraft,
			Language:       "en",
			TargetPlatforms: models.CampaignPlatforms{
				{ID: platformID, PostTypes: []string{"text-post", "article"}},
			},
			EstimatedPostCount: &estCount,
			StartDate:          &start,
			EndDate:            &end,
			CreatedBy:          userID,
		}
		Expect(campaignRepo.Create(ctx, campaign)).To(Succeed())

		// Fresh genkit instance for this suite so the package-global
		// runner inside content_plan/flow.go points at the K=5 config
		// regardless of which Describe ran first.
		g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))

		modelID := os.Getenv("MODEL_ID")
		if modelID == "" {
			modelID = "claude-haiku-4-5-20251001"
		}
		flowCfg := content_plan.ContentPlanFlowConfig{
			ModelID:            modelID,
			MaxContextAssets:   5,
			MaxContextChars:    3000,
			MaxOutputTokens:    8192,
			MaxPostsPerBatch:   maxPostsPerBatch,
			MaxParallelBatches: maxParallelBatch,
		}
		repos := content_plan.ContentPlanRepos{
			Campaigns: campaignRepo,
			Assets:    assetRepo,
			Chunks:    chunksRepo,
			Platforms: platformRepo,
			Posts:     postRepo,
		}
		Expect(content_plan.InitContentPlan(g, flowCfg, repos)).To(Succeed())
	})

	AfterAll(func() {
		_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns").Where("id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("id = ?", userID).Exec(ctx)
		_, err := db.NewDelete().TableExpr("accounts").Where("id = ?", userID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("parallel batches against the real model", func() {
		It("generates posts across multiple batches and validates them", func() {
			cb := content_plan.NewContentPlanCallback()
			resp, err := cb(ctx, campaignID, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			// Real models occasionally produce slightly more or fewer posts
			// than the per-batch ask — validate against a tolerance band
			// rather than exact equality. Two batches of ~5 each gives a
			// reasonable window of [6..14].
			Expect(len(resp.Posts)).To(BeNumerically(">=", 6),
				"expected at least 6 posts across two batches, got %d", len(resp.Posts))
			Expect(len(resp.Posts)).To(BeNumerically("<=", 14),
				"expected at most 14 posts across two batches, got %d", len(resp.Posts))

			// Awareness campaign type's phase IDs (see migration
			// 20260414000001_create_campaign_types.up.sql).
			validPhaseIDs := map[string]bool{"98": true, "xh": true}

			startStr := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02")
			endStr := time.Now().UTC().Truncate(24 * time.Hour).Add(21 * 24 * time.Hour).Format("2006-01-02")

			seenPhases := map[string]bool{}
			for _, p := range resp.Posts {
				Expect(p.Title).NotTo(BeEmpty())
				Expect(p.Body).NotTo(BeEmpty())
				Expect(p.PlatformID).To(Equal(platformID))
				Expect(p.PublishDate).NotTo(BeEmpty())
				Expect(validPhaseIDs[p.PhaseID]).To(BeTrue(),
					"post %q has unknown phaseId %q", p.Title, p.PhaseID)
				// Date-range string comparison matches validateOutput's
				// own check — keeps the assertion identical to what the
				// flow already enforces.
				Expect(p.PublishDate >= startStr).To(BeTrue(),
					"post publishDate %q before campaign start %q", p.PublishDate, startStr)
				Expect(p.PublishDate <= endStr).To(BeTrue(),
					"post publishDate %q after campaign end %q", p.PublishDate, endStr)
				seenPhases[p.PhaseID] = true
			}

			// The slot allocator distributes posts across all phases of the
			// campaign type — so on a 10-post run with 2 phases, both phases
			// must appear at least once. (If only one shows up, either the
			// allocator regressed or every post in the under-represented
			// phase was dropped by validateOutput.)
			Expect(seenPhases).To(HaveLen(2),
				"both awareness phases ('98', 'xh') must be represented across batches; saw %v", seenPhases)
		})

		// Index uniqueness is the load-bearing invariant for the React UI's
		// slot-based placement: every post event must carry a stable global
		// slot index, and no two events may share an index. Arrival order is
		// explicitly NOT asserted — under parallel batching, posts arrive
		// interleaved by completion time.
		It("emits post events with unique global indices across batches", func() {
			// Clean up so the run starts fresh.
			_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)

			var (
				mu     sync.Mutex
				events []content_plan.PostEventPayload
			)
			onEvent := content_plan.OnEventFunc(func(name content_plan.SSEEventKind, data any) {
				if name != content_plan.SSEEventPost {
					return
				}
				if p, ok := data.(content_plan.PostEventPayload); ok {
					// onEvent fires from per-batch goroutines under
					// runBatchesParallel; the production wrapper
					// serialises it, but we add a defensive lock here
					// in case a future refactor relaxes that.
					mu.Lock()
					events = append(events, p)
					mu.Unlock()
				}
			})

			cb := content_plan.NewContentPlanCallback()
			resp, err := cb(ctx, campaignID, onEvent)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			// Every post in the final response must have arrived as a
			// post event — each batch streams or falls back to blocking,
			// either path emits.
			Expect(events).To(HaveLen(len(resp.Posts)),
				"streamed event count must match resp.Posts len; got %d events for %d posts", len(events), len(resp.Posts))

			// Indices must be unique. Sort and walk to detect dupes.
			idxs := make([]int, len(events))
			for i, e := range events {
				idxs[i] = e.Index
				Expect(e.Index).To(BeNumerically(">=", 0))
			}
			slices.Sort(idxs)
			for i := 1; i < len(idxs); i++ {
				Expect(idxs[i]).NotTo(Equal(idxs[i-1]),
					"duplicate post index %d found in stream — slot allocator should produce unique global indices", idxs[i])
			}

			// Indices are global slot IDs (CON-67), so the lowest seen
			// index must be 0 — batch 0's GlobalStartIndex.
			Expect(idxs[0]).To(Equal(0),
				"lowest streamed index must be 0; got %d", idxs[0])
		})

		// Per CON-66 every post is persisted before its post-event fires.
		// PostEventPayload.ID carries the row id; the event is the proof
		// that the row exists. Verify that:
		//   1. Every emitted event has a non-empty ID.
		//   2. The DB has exactly one row per emitted ID.
		//   3. The persisted row count matches resp.Posts (the run's
		//      authoritative result is also the persisted set — there's
		//      no separate aggregate persist step any more).
		It("persists each post before its event fires (per-CON-66)", func() {
			_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)

			var (
				mu     sync.Mutex
				events []content_plan.PostEventPayload
			)
			onEvent := content_plan.OnEventFunc(func(name content_plan.SSEEventKind, data any) {
				if name != content_plan.SSEEventPost {
					return
				}
				if p, ok := data.(content_plan.PostEventPayload); ok {
					mu.Lock()
					events = append(events, p)
					mu.Unlock()
				}
			})

			cb := content_plan.NewContentPlanCallback()
			resp, err := cb(ctx, campaignID, onEvent)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			// Every event must carry a persisted row ID.
			for _, e := range events {
				Expect(e.ID).NotTo(BeEmpty(),
					"PostEventPayload.ID must be set under CON-66; missing for index %d (%q)", e.Index, e.Post.Title)
			}

			// DB row count = streamed event count = resp.Posts length.
			postRepo := repository.NewPostRepository(db)
			persisted, err := postRepo.ListByCampaign(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(persisted)).To(Equal(len(events)),
				"DB row count (%d) must match emitted post-event count (%d) — CON-66 promise is per-event persistence",
				len(persisted), len(events))
			Expect(len(persisted)).To(Equal(len(resp.Posts)),
				"DB row count (%d) must match resp.Posts len (%d) — flow now returns the persisted set",
				len(persisted), len(resp.Posts))

			// Every emitted ID must reference a real row in the DB.
			gotIDs := map[string]bool{}
			for _, p := range persisted {
				gotIDs[p.ID] = true
			}
			for _, e := range events {
				Expect(gotIDs[e.ID]).To(BeTrue(),
					"emitted ID %q (slot %d, %q) is not present in the posts table", e.ID, e.Index, e.Post.Title)
			}
		})
	})
})
