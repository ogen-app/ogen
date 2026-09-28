//go:build integration

package integration_test

import (
	"context"
	"errors"
	"os"

	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/repository"
)

var _ = Describe("Post quality assessment flow", Ordered, func() {
	var (
		ctx        context.Context
		db         *bun.DB
		userID     string
		campaignID string
		postRepo   repository.PostRepository
		evalRepo   repository.PostEvaluationRepository
		logRepo    repository.PostLogRepository
		repos      post_quality.PostQualityRepos
		callback   func(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error)

		platformID = "AXqWG7U2qnpt" // seeded LinkedIn platform (Sqid)
	)

	BeforeAll(func() {
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			Skip("ANTHROPIC_API_KEY not set — skipping post quality integration tests")
		}

		ctx = tenantCtx()
		db = mustOpenIntegrationDB()

		tagRepo := repository.NewTagRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		chunksRepo := repository.NewAssetChunksRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)
		postRepo = repository.NewPostRepository(db)
		evalRepo = repository.NewPostEvaluationRepository(db)
		logRepo = repository.NewPostLogRepository(db)

		var err error
		userID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "pq-integration@test.local",
			PasswordHash: "placeholder",
			Name:         "Post Quality Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Post Quality Tester",
			Email:     "pq-integration@test.local",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		campaignID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		Expect(campaignRepo.Create(ctx, &models.Campaign{
			ID:              campaignID,
			Name:            "Integration Test Campaign",
			Description:     "Promote Go as a production language for AI engineering teams.",
			TargetPersona:   "Backend engineers evaluating Go for AI workloads.",
			KeyMessages:     "Static types, single-binary deploys, goroutine concurrency.",
			ToneGuidelines:  "Confident, technical, no marketing fluff.",
			CampaignTypeID:  "Uk",
			Status:          models.StatusDraft,
			Language:        "en",
			TargetPlatforms: models.CampaignPlatforms{{ID: platformID, PostTypes: []string{"text-post", "article"}}},
			CreatedBy:       userID,
		})).To(Succeed())

		repos = post_quality.PostQualityRepos{
			Posts:       postRepo,
			Campaigns:   campaignRepo,
			Assets:      assetRepo,
			Chunks:      chunksRepo,
			Platforms:   platformRepo,
			Evaluations: evalRepo,
			PostLogs:    logRepo,
		}

		g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))
		modelID := os.Getenv("QUALITY_MODEL_ID")
		if modelID == "" {
			modelID = "claude-haiku-4-5-20251001"
		}
		// Weights are required — without a profile ComposeScore returns 0.
		Expect(post_quality.InitPostQuality(g, post_quality.PostQualityFlowConfig{
			ModelID: modelID,
			Weights: post_quality.DefaultWeights(),
		}, repos)).To(Succeed())
		callback = post_quality.NewPostQualityCallback()
	})

	AfterEach(func() {
		// Scope child-table cleanup to this suite's campaign posts. (Each
		// Describe already runs on its own DB file, and both tables cascade
		// on the posts delete below, so this is defensive rather than
		// load-bearing.) Runs before the posts delete so the subquery resolves.
		scoped := "post_id IN (SELECT id FROM posts WHERE campaign_id = ?)"
		_, _ = db.NewDelete().TableExpr("post_evaluations").Where(scoped, campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_logs").Where(scoped, campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)
	})

	AfterAll(func() {
		if db == nil {
			return
		}
		_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns").Where("id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("id = ?", userID).Exec(ctx)
	})

	// seedPost inserts a post with the given content, type, and media, and
	// returns its id.
	seedPost := func(content, postType string, media models.StringSlice) string {
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		Expect(postRepo.Create(ctx, &models.Post{
			ID:               id,
			CampaignID:       campaignID,
			PlatformID:       platformID,
			PlatformPostType: postType,
			Title:            "Why Go for AI Engineering",
			Content:          content,
			MediaURLs:        media,
			Status:           models.PostStatusDraft,
			CTAType:          models.CTATypeNone,
			UsedAssetIDs:     models.StringSlice{},
			CreatedBy:        userID,
		})).To(Succeed())
		return id
	}

	const strongPost = `Most teams reach for Python for AI features. Here's why we didn't.

Go caught three schema mismatches at compile time last sprint — bugs that would have hit production in a dynamically typed stack. Goroutines made streaming tool-calls trivial to fan out. And our deploy is a single 18MB binary, not a 600MB image.

If you're shipping AI features under real latency budgets, Go deserves a second look.

What's stopped your team from trying it? 👇`

	const weakPost = `We are excited to share some thoughts about technology today. Technology is very important and many companies use it. Go is a programming language and Python is also a programming language. There are many benefits to using good tools. Learn more!`

	Describe("happy path", func() {
		It("scores all four dimensions, computes the overall, and persists the result", func() {
			postID := seedPost(strongPost, "text-post", models.StringSlice{})

			resp, err := callback(ctx, postID, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Evaluation).NotTo(BeNil())

			ev := resp.Evaluation
			Expect(ev.OverallPct).To(BeNumerically(">", 0))
			Expect(ev.OverallPct).To(BeNumerically("<=", 100))
			Expect(ev.ModelID).NotTo(BeEmpty())
			Expect(ev.CaptionScoped).To(BeFalse(), "text post has no unseen media")

			// Every dimension scored in range, with rationale and a mandatory
			// weakness, and weights stamped by ComposeScore.
			for _, d := range []models.EvaluationDimension{
				ev.Result.Correctness, ev.Result.Clarity, ev.Result.Engagement, ev.Result.Delivery,
			} {
				Expect(d.Score).To(BeNumerically(">=", 0))
				Expect(d.Score).To(BeNumerically("<=", 10))
				Expect(d.Rationale).NotTo(BeEmpty())
				Expect(d.Weakness).NotTo(BeEmpty(), "a weakness is mandatory on every dimension")
				Expect(d.Weight).To(BeNumerically(">", 0), "ComposeScore must stamp the weight")
			}

			// Contributions sum to the overall (deterministic composition).
			sum := ev.Result.Correctness.Contribution + ev.Result.Clarity.Contribution +
				ev.Result.Engagement.Contribution + ev.Result.Delivery.Contribution
			Expect(sum).To(BeNumerically("~", ev.OverallPct, 0.01))

			// Any suggestion must be span-anchored.
			for _, d := range []models.EvaluationDimension{
				ev.Result.Correctness, ev.Result.Clarity, ev.Result.Engagement, ev.Result.Delivery,
			} {
				for _, s := range d.Suggestions {
					Expect(s.Span).NotTo(BeEmpty(), "every suggestion must quote a span")
				}
			}

			// Persisted and readable back.
			persisted, err := evalRepo.GetByPostID(ctx, postID)
			Expect(err).NotTo(HaveOccurred())
			Expect(persisted).NotTo(BeNil())
			Expect(persisted.OverallPct).To(BeNumerically("~", ev.OverallPct, 0.01))

			// Operation recorded in the Post Log.
			logs, err := logRepo.ListByPostID(ctx, postID, 10)
			Expect(err).NotTo(HaveOccurred())
			var sawAssessed bool
			for _, l := range logs {
				if l.EventType == models.PostLogEventQualityAssessed {
					sawAssessed = true
				}
			}
			Expect(sawAssessed).To(BeTrue(), "a quality_assessed PostLog entry must exist")
		})
	})

	Describe("caption-scoped posts", func() {
		It("flags the score as caption-scoped when the post carries media", func() {
			postID := seedPost(strongPost, "image-post", models.StringSlice{"https://example.com/a.png"})

			resp, err := callback(ctx, postID, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.Evaluation.CaptionScoped).To(BeTrue(), "media present → caption-scoped")
		})
	})

	Describe("re-evaluation", func() {
		It("overwrites the prior result rather than appending a second row", func() {
			postID := seedPost(strongPost, "text-post", models.StringSlice{})

			_, err := callback(ctx, postID, nil)
			Expect(err).NotTo(HaveOccurred())
			_, err = callback(ctx, postID, nil)
			Expect(err).NotTo(HaveOccurred())

			var count int
			Expect(db.NewSelect().Model((*models.PostEvaluation)(nil)).
				Where("post_id = ?", postID).ColumnExpr("count(*)").Scan(ctx, &count)).To(Succeed())
			Expect(count).To(Equal(1), "re-eval overwrites; exactly one row per post")
		})
	})

	Describe("change detection (CON-92)", func() {
		It("serves the stored evaluation when inputs are unchanged, and re-runs after an edit", func() {
			postID := seedPost(strongPost, "text-post", models.StringSlice{})

			// First run: the model executes and the result is persisted.
			first, err := callback(ctx, postID, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(first.Cached).To(BeFalse(), "the first assessment must run the model")
			firstOverall := first.Evaluation.OverallPct

			// Second run with the post unchanged: the rendered prompt + model
			// id hash matches, so the stored result is returned without a
			// model call.
			second, err := callback(ctx, postID, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(second.Cached).To(BeTrue(), "unchanged inputs must be served from cache")
			Expect(second.Evaluation.OverallPct).To(Equal(firstOverall), "cache returns the stored evaluation")

			// Edit the post body. The prompt now differs, so the next assess
			// must invalidate the cache and run the model again.
			_, err = db.NewUpdate().Model((*models.Post)(nil)).
				Set("content = ?", weakPost).
				Where("id = ?", postID).Exec(ctx)
			Expect(err).NotTo(HaveOccurred())

			third, err := callback(ctx, postID, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(third.Cached).To(BeFalse(), "an edit must invalidate the cache and re-run the model")
		})
	})

	Describe("input validation", func() {
		It("rejects an empty-body post with a ValidationError and no model call", func() {
			postID := seedPost("   ", "text-post", models.StringSlice{})

			_, err := callback(ctx, postID, nil)
			Expect(err).To(HaveOccurred())
			var ve *post_quality.ValidationError
			Expect(errors.As(err, &ve)).To(BeTrue(), "empty body must fail input validation")
		})
	})

	Describe("SSE event stream", func() {
		It("emits step events and a final complete event", func() {
			postID := seedPost(strongPost, "text-post", models.StringSlice{})

			var (
				gotStep     bool
				gotComplete bool
				gotError    bool
				order       []post_quality.SSEEventKind
			)
			onEvent := post_quality.OnEventFunc(func(name post_quality.SSEEventKind, _ any) {
				order = append(order, name)
				switch name {
				case post_quality.SSEEventStep:
					gotStep = true
				case post_quality.SSEEventComplete:
					gotComplete = true
				case post_quality.SSEEventError:
					gotError = true
				}
			})

			_, err := callback(ctx, postID, onEvent)
			Expect(err).NotTo(HaveOccurred())
			Expect(gotError).To(BeFalse())
			Expect(gotStep).To(BeTrue(), "step events should fire per flow stage")
			Expect(gotComplete).To(BeTrue())
			Expect(order[len(order)-1]).To(Equal(post_quality.SSEEventComplete), "complete is last")
		})
	})

	// Ranking eval: a cheap model is sufficient if its RANKINGS
	// agree with a stronger model's, even where absolute numbers differ.
	// Run a clearly-strong and a clearly-weak post through each model and
	// assert strong outscores weak. Sonnet is only checked when configured.
	Describe("ranking eval", func() {
		assessBoth := func(modelID string) (strong, weak float64) {
			g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))
			Expect(post_quality.InitPostQuality(g, post_quality.PostQualityFlowConfig{
				ModelID: modelID,
				Weights: post_quality.DefaultWeights(),
			}, repos)).To(Succeed())
			cb := post_quality.NewPostQualityCallback()

			strongID := seedPost(strongPost, "text-post", models.StringSlice{})
			weakID := seedPost(weakPost, "text-post", models.StringSlice{})

			rs, err := cb(ctx, strongID, nil)
			Expect(err).NotTo(HaveOccurred())
			rw, err := cb(ctx, weakID, nil)
			Expect(err).NotTo(HaveOccurred())
			return rs.Evaluation.OverallPct, rw.Evaluation.OverallPct
		}

		It("ranks the strong post above the weak post on Haiku (and Sonnet if set)", func() {
			haiku := os.Getenv("QUALITY_MODEL_ID")
			if haiku == "" {
				haiku = "claude-haiku-4-5-20251001"
			}
			hStrong, hWeak := assessBoth(haiku)
			Expect(hStrong).To(BeNumerically(">", hWeak),
				"Haiku should rank the strong post above the weak post")

			if sonnet := os.Getenv("SONNET_MODEL_ID"); sonnet != "" {
				sStrong, sWeak := assessBoth(sonnet)
				Expect(sStrong).To(BeNumerically(">", sWeak),
					"Sonnet should agree: strong above weak")
			}
		})
	})
})
