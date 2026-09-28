//go:build integration

package integration_test

import (
	"context"
	"os"
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

var _ = Describe("Content plan flow", Ordered, func() {
	var (
		ctx        context.Context
		db         *bun.DB
		userID     string
		campaignID string
		platformID = "AXqWG7U2qnpt" // seeded LinkedIn platform (Sqid)
	)

	BeforeAll(func() {
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			Skip("ANTHROPIC_API_KEY not set — skipping content plan integration tests")
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

		// Seed user.
		var err error
		userID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "cp-integration@test.local",
			PasswordHash: "placeholder",
			Name:         "Content Plan Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		user := &models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Content Plan Tester",
			Email:     "cp-integration@test.local",
		}
		_, err = db.NewInsert().Model(user).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		// Seed a campaign with all required fields.
		start := time.Now().UTC().Truncate(24 * time.Hour)
		end := start.Add(14 * 24 * time.Hour)
		estCount := 6
		campaignID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		campaign := &models.Campaign{
			ID:             campaignID,
			Name:           "Integration Test Campaign",
			Description:    "A campaign to test AI-driven content plan generation.",
			TargetPersona:  "Marketing professionals aged 25–45 interested in SaaS tools.",
			KeyMessages:    "Save time, increase reach, data-driven insights.",
			ToneGuidelines: "Professional yet approachable; use concrete examples.",
			CampaignTypeID: "Uk",
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

		// Initialise the Anthropic-backed Genkit flow.
		g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))

		modelID := os.Getenv("MODEL_ID")
		if modelID == "" {
			modelID = "claude-haiku-4-5-20251001"
		}
		initModelConfig(ctx, modelID, modelID)
		flowCfg := content_plan.ContentPlanFlowConfig{
			MaxContextAssets: 5,
			MaxContextChars:  3000,
			MaxOutputTokens:  8192,
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

	Describe("generateContentPlan flow", func() {
		It("produces draft posts within the campaign date range and persists them", func() {
			resp, err := content_plan.ContentPlanFlow.Run(ctx, content_plan.ContentPlanRequest{CampaignID: campaignID})
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Posts).NotTo(BeEmpty())

			for _, p := range resp.Posts {
				Expect(p.Title).NotTo(BeEmpty())
				Expect(p.Body).NotTo(BeEmpty())
				Expect(p.PlatformID).To(Equal(platformID))
				Expect(p.PublishDate).NotTo(BeEmpty())
			}

			// Verify posts were persisted.
			postRepo := repository.NewPostRepository(db)
			posts, err := postRepo.ListByCampaign(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(posts).To(HaveLen(len(resp.Posts)))
		})

		It("emits SSE step events via NewContentPlanCallback", func() {
			// Clean up posts from previous test so the run is fresh.
			_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)

			var events []content_plan.StepEventPayload
			onEvent := content_plan.OnEventFunc(func(name content_plan.SSEEventKind, data any) {
				if name == content_plan.SSEEventStep {
					if p, ok := data.(content_plan.StepEventPayload); ok {
						events = append(events, p)
					}
				}
			})

			cb := content_plan.NewContentPlanCallback()
			resp, err := cb(ctx, campaignID, onEvent)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			// The flow has 6 named steps; all should be emitted.
			stepNames := make([]string, len(events))
			for i, e := range events {
				stepNames[i] = e.Step
				Expect(e.Status).To(Equal("done"))
			}
			Expect(stepNames).To(ConsistOf(
				"validateInput",
				"resolveAssets",
				"resolvePlatforms",
				"generatePosts",
				"validateOutput",
				"persistDraftPosts",
			))
		})

		It("emits SSE post events incrementally during model streaming", func() {
			// Clean up posts from previous tests so the run is fresh.
			_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)

			var postEvents []content_plan.PostEventPayload
			onEvent := content_plan.OnEventFunc(func(name content_plan.SSEEventKind, data any) {
				if name == content_plan.SSEEventPost {
					if p, ok := data.(content_plan.PostEventPayload); ok {
						postEvents = append(postEvents, p)
					}
				}
			})

			cb := content_plan.NewContentPlanCallback()
			resp, err := cb(ctx, campaignID, onEvent)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			// Every post in the final response must have been streamed as a post event.
			Expect(postEvents).To(HaveLen(len(resp.Posts)))

			// Indices must be sequential starting from 0.
			for i, pe := range postEvents {
				Expect(pe.Index).To(Equal(i))
				Expect(pe.Post.Title).NotTo(BeEmpty())
				Expect(pe.Post.PlatformID).NotTo(BeEmpty())
			}
		})
	})
})
