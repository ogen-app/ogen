//go:build integration

package integration_test

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

var _ = Describe("Post assistant flow", Ordered, func() {
	var (
		ctx         context.Context
		db          *bun.DB
		userID      string
		campaignID  string
		postRepo    repository.PostRepository
		versionRepo repository.PostVersionRepository
		messageRepo repository.PostAssistantMessageRepository
		callback    func(ctx context.Context, req post_assistant.PostAssistantRequest, onEvent post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error)

		platformID = "AXqWG7U2qnpt" // seeded LinkedIn platform (Sqid)
	)

	BeforeAll(func() {
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			Skip("ANTHROPIC_API_KEY not set — skipping post assistant integration tests")
		}

		ctx = tenantCtx()
		db = mustOpenIntegrationDB()

		// Wire up every repository the flow consumes.
		tagRepo := repository.NewTagRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		chunksRepo := repository.NewAssetChunksRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)
		postRepo = repository.NewPostRepository(db)
		versionRepo = repository.NewPostVersionRepository(db)
		messageRepo = repository.NewPostAssistantMessageRepository(db)

		// Seed user.
		var err error
		userID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "pa-integration@test.local",
			PasswordHash: "placeholder",
			Name:         "Post Assistant Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Post Assistant Tester",
			Email:     "pa-integration@test.local",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		// Seed a campaign with everything the assistant prompt expects.
		start := time.Now().UTC().Truncate(24 * time.Hour)
		end := start.Add(14 * 24 * time.Hour)
		estCount := 6
		campaignID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		Expect(campaignRepo.Create(ctx, &models.Campaign{
			ID:                 campaignID,
			Name:               "Integration Test Campaign",
			Description:        "Campaign for testing the post assistant flow.",
			TargetPersona:      "Backend engineers evaluating Go for AI workloads.",
			KeyMessages:        "Static types, single-binary deploys, goroutine concurrency.",
			ToneGuidelines:     "Confident, technical, no marketing fluff.",
			CampaignTypeID:     "Uk",
			Status:             models.StatusDraft,
			Language:           "en",
			TargetPlatforms:    models.CampaignPlatforms{{ID: platformID, PostTypes: []string{"text-post", "article"}}},
			EstimatedPostCount: &estCount,
			StartDate:          &start,
			EndDate:            &end,
			CreatedBy:          userID,
		})).To(Succeed())

		// Initialise Genkit with the native Anthropic plugin and register
		// the post assistant flow. Returns the same callback the HTTP
		// handler would use in production.
		g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))
		modelID := os.Getenv("MODEL_ID")
		if modelID == "" {
			modelID = "claude-haiku-4-5-20251001"
		}
		planningModelID := os.Getenv("PLANNING_MODEL_ID")
		if planningModelID == "" {
			planningModelID = "claude-haiku-4-5-20251001"
		}
		// CON-128: the flow resolves its loop model (and the editPost writer
		// model) through the Provider — without one, cfg.Provider.Ref would
		// panic. Exercise the hybrid planner path by default (the shipped
		// default); set POST_ASSISTANT_PLANNER=false to run the legacy
		// single-Sonnet path instead, so the same suite covers both.
		provider := llm.NewProvider(modelID, modelID, planningModelID)
		// CON-308: the flow resolves its loop + writer models through the
		// modelconfig resolver; seed it so Ref returns real model ids.
		initModelConfig(ctx, modelID, planningModelID)
		flowCfg := post_assistant.PostAssistantFlowConfig{
			Provider:       provider,
			ModelID:        modelID,
			PlannerEnabled: os.Getenv("POST_ASSISTANT_PLANNER") != "false",
		}
		repos := post_assistant.PostAssistantRepos{
			Posts:     postRepo,
			Assets:    assetRepo,
			Chunks:    chunksRepo,
			Campaigns: campaignRepo,
			Versions:  versionRepo,
			Messages:  messageRepo,
		}
		Expect(post_assistant.InitPostAssistant(g, flowCfg, repos)).To(Succeed())
		callback = post_assistant.NewPostAssistantCallback()
	})

	AfterEach(func() {
		// Cleared between tests so each spec starts with a fresh post.
		_, _ = db.NewDelete().TableExpr("post_assistant_messages").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_versions").Where("1 = 1").Exec(ctx)
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

	// seedPost inserts a fresh post with the given Markdown content and
	// returns its ID.
	seedPost := func(content string) string {
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		Expect(postRepo.Create(ctx, &models.Post{
			ID:               id,
			CampaignID:       campaignID,
			PlatformID:       platformID,
			PlatformPostType: "text-post",
			Title:            "Why Go for AI Engineering",
			Content:          content,
			MediaURLs:        models.StringSlice{},
			Status:           models.PostStatusDraft,
			CTAType:          models.CTATypeNone,
			UsedAssetIDs:     models.StringSlice{},
			CreatedBy:        userID,
		})).To(Succeed())
		return id
	}

	Describe("edit mode", func() {
		It("shortens the post and persists the change with a version snapshot", func() {
			postID := seedPost(`# Why Go for AI Engineering

Go is a great choice for AI engineering for many reasons. The language has strong static typing which catches schema mismatches before they reach production. Goroutines provide a natural concurrency model that suits the parallel tool calls and streaming that AI features demand. Single-binary deploys keep operational overhead low. And the dependency graph stays small, which reduces the supply-chain attack surface.

This post explores each of these benefits in detail and explains why teams shipping production AI features should consider Go alongside Python.`)

			resp, err := callback(ctx, post_assistant.PostAssistantRequest{
				PostID:      postID,
				Instruction: "Cut this post in half. Keep the same meaning but make it much shorter and tighter.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("edited"))
			Expect(resp.UpdatedContent).NotTo(BeEmpty(), "updatedContent should carry the rewritten Markdown")
			Expect(resp.Explanation).NotTo(BeEmpty(), "explanation should describe the change")

			// Persisted post should now have the assistant's content.
			persisted, err := postRepo.GetByID(ctx, postID)
			Expect(err).NotTo(HaveOccurred())
			Expect(persisted.Content).To(Equal(resp.UpdatedContent))

			// Initial-version snapshot is created on the first assistant call;
			// the assistant's own edit may add a second one if it set
			// saveVersion=true (a structural rewrite typically does).
			versions, err := versionRepo.ListByPostID(ctx, postID)
			Expect(err).NotTo(HaveOccurred())
			Expect(versions).NotTo(BeEmpty(), "at least the initial-version snapshot must exist")
			Expect(versions[0].Content).NotTo(BeEmpty(), "initial version should hold the pre-edit content")
		})
	})

	Describe("answer mode", func() {
		It("declines edits but answers an informational question via the explanation field", func() {
			postID := seedPost(`# Why Go for AI Engineering

Go's static typing catches schema mismatches at compile time. Goroutines give a natural concurrency model. Single-binary deploys minimise ops overhead.`)

			resp, err := callback(ctx, post_assistant.PostAssistantRequest{
				PostID:      postID,
				Instruction: "Don't change the post. Just tell me roughly how many words this post has.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("declined"), "informational request should not be treated as an edit")
			Expect(resp.UpdatedContent).To(BeEmpty(), "answer mode never writes to updatedContent")
			Expect(resp.Explanation).NotTo(BeEmpty(), "the answer to the user's question lives in explanation")

			// Post should be unchanged.
			persisted, err := postRepo.GetByID(ctx, postID)
			Expect(err).NotTo(HaveOccurred())
			Expect(persisted.Content).To(ContainSubstring("Go's static typing"))
		})
	})

	Describe("conversation history", func() {
		It("persists user and model turns so subsequent calls see prior context", func() {
			postID := seedPost(`# Quick Note

A short note about Go.`)

			_, err := callback(ctx, post_assistant.PostAssistantRequest{
				PostID:      postID,
				Instruction: "Expand this into a single paragraph mentioning static typing.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())

			msgs, err := messageRepo.ListRecentByPostID(ctx, postID, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(msgs)).To(BeNumerically(">=", 2), "user instruction + model response should be persisted")

			// Find roles — order isn't guaranteed by the repo signature.
			var sawUser, sawModel bool
			for _, m := range msgs {
				switch m.Role {
				case "user":
					sawUser = true
					Expect(m.Content).To(ContainSubstring("static typing"))
				case "model":
					sawModel = true
					// Model turn is persisted as JSON of action/explanation/saveVersion/versionNote.
					Expect(strings.HasPrefix(strings.TrimSpace(m.Content), "{")).To(BeTrue(),
						"model turn should be stored as JSON for badge reconstruction on reload")
				}
			}
			Expect(sawUser).To(BeTrue())
			Expect(sawModel).To(BeTrue())
		})
	})

	Describe("SSE event stream", func() {
		It("emits explanation_delta and complete events for an edit", func() {
			postID := seedPost(`# Concise

A concise post about Go.`)

			var (
				gotExplanationDelta bool
				gotContentDelta     bool
				gotComplete         bool
				gotError            bool
				eventOrder          []post_assistant.SSEEventKind
			)
			onEvent := post_assistant.OnEventFunc(func(name post_assistant.SSEEventKind, _ any) {
				eventOrder = append(eventOrder, name)
				switch name {
				case post_assistant.SSEEventExplanationDelta:
					gotExplanationDelta = true
				case post_assistant.SSEEventContentDelta:
					gotContentDelta = true
				case post_assistant.SSEEventComplete:
					gotComplete = true
				case post_assistant.SSEEventError:
					gotError = true
				}
			})

			_, err := callback(ctx, post_assistant.PostAssistantRequest{
				PostID:      postID,
				Instruction: "Make it about three sentences longer with concrete examples.",
			}, onEvent)
			Expect(err).NotTo(HaveOccurred())

			Expect(gotError).To(BeFalse(), "error event should not fire on the happy path")
			Expect(gotExplanationDelta).To(BeTrue(), "explanation_delta should fire as the model writes the explanation")
			// content_delta must stream the edited copy: in the hybrid path this
			// proves the editPost writer sub-call streams through to the client;
			// in the legacy path it is the inline updatedContent stream (CON-128).
			Expect(gotContentDelta).To(BeTrue(), "content_delta should stream the rewritten post content on an edit")
			Expect(gotComplete).To(BeTrue(), "complete event signals the canonical final response")

			// complete should be the last event emitted.
			Expect(eventOrder[len(eventOrder)-1]).To(Equal(post_assistant.SSEEventComplete))
		})
	})
})
