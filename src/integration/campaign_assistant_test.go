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
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
)

// This suite exercises the real Campaign Assistant flow end to end against a
// live Anthropic model, wiring the content_plan and enrich_brief flows as tools
// exactly as the server does. It is skipped unless ANTHROPIC_API_KEY is set.
var _ = Describe("Campaign assistant flow", Ordered, func() {
	var (
		ctx          context.Context
		db           *bun.DB
		userID       string
		campaignID   string
		campaignRepo repository.CampaignRepository
		postRepo     repository.PostRepository
		ideaRepo     repository.IdeaRepository
		postNoteRepo repository.PostNoteRepository
		messageRepo  repository.AssistantMessageRepository
		assetRepo    repository.AssetRepository
		callback     func(ctx context.Context, req campaignTurn, onEvent campaign_assistant.OnEventFunc) (*campaignTurnResult, error)

		platformID = "AXqWG7U2qnpt" // seeded LinkedIn platform (Sqid)
	)

	BeforeAll(func() {
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			Skip("ANTHROPIC_API_KEY not set — skipping campaign assistant integration tests")
		}

		ctx = tenantCtx()
		db = mustOpenIntegrationDB()

		tagRepo := repository.NewTagRepository(db)
		assetRepo = repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		chunksRepo := repository.NewAssetChunksRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo = repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)
		postRepo = repository.NewPostRepository(db)
		ideaRepo = repository.NewIdeaRepository(db)
		postNoteRepo = repository.NewPostNoteRepository(db)
		messageRepo = repository.NewAssistantMessageRepository(db)

		// Seed user.
		var err error
		userID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "ca-integration@test.local",
			PasswordHash: "placeholder",
			Name:         "Campaign Assistant Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Campaign Assistant Tester",
			Email:     "ca-integration@test.local",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		// Seed a campaign with everything the sub-flows expect (type for
		// enrich_brief; target platform for content_plan).
		start := time.Now().UTC().Truncate(24 * time.Hour)
		end := start.Add(14 * 24 * time.Hour)
		estCount := 4
		campaignID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		Expect(campaignRepo.Create(ctx, &models.Campaign{
			ID:                 campaignID,
			Name:               "Go for AI Engineering",
			Description:        "Campaign about using Go for production AI features.",
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

		// Wire Genkit with the native Anthropic plugin and register all three
		// flows on the shared instance, injecting content_plan + enrich_brief
		// as the assistant's tools — the same wiring the server does.
		g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))
		modelID := os.Getenv("MODEL_ID")
		if modelID == "" {
			modelID = "claude-haiku-4-5-20251001"
		}
		provider := llm.NewProvider()
		// Seed the modelconfig resolver so every flow slot (orchestrator
		// + the content_plan/enrich_brief/draft_post sub-flows) resolves a model.
		initModelConfig(ctx, modelID, modelID)

		Expect(content_plan.InitContentPlan(g, content_plan.ContentPlanFlowConfig{Provider: provider, MaxContextAssets: 5}, content_plan.ContentPlanRepos{
			Campaigns: campaignRepo,
			Assets:    assetRepo,
			Chunks:    chunksRepo,
			Platforms: platformRepo,
			Ideas:     ideaRepo,
		})).To(Succeed())
		contentPlanCb := content_plan.NewContentPlanCallback()

		Expect(enrich_brief.InitEnrichBrief(g, enrich_brief.EnrichBriefFlowConfig{Provider: provider}, enrich_brief.EnrichBriefRepos{
			Campaigns:     campaignRepo,
			CampaignTypes: campaignTypeRepo,
		})).To(Succeed())
		enrichBriefCb := enrich_brief.NewEnrichBriefCallback()
		generatePostsCb := content_plan.NewGeneratePostsCallback()

		// Register the draftPost generation flow as an assistant tool.
		Expect(draft_post.InitDraftPost(g, draft_post.DraftPostFlowConfig{Provider: provider}, draft_post.DraftPostRepos{
			Campaigns: campaignRepo,
			Platforms: platformRepo,
			Posts:     postRepo,
			Notes:     postNoteRepo,
		})).To(Succeed())
		draftPostCb := draft_post.NewDraftPostCallback()

		overviewSvc := overview.New(campaignRepo, postRepo, platformRepo)

		proposalRepo := repository.NewAssistantProposalRepository(db)
		pack, err := campaign_assistant.NewPack(campaign_assistant.CampaignAssistantFlowConfig{
			ContentPlan:      contentPlanCb,
			EnrichBrief:      enrichBriefCb,
			Overview:         overviewSvc,
			GeneratePosts:    generatePostsCb,
			MaxGeneratePosts: 10,
			DraftPost:        draftPostCb,
			MaxDraftPosts:    5,
		}, campaign_assistant.CampaignAssistantRepos{
			Messages:  messageRepo,
			Campaigns: campaignRepo,
			Posts:     postRepo,
			Assets:    assetRepo,
			Chunks:    chunksRepo,
			Proposals: proposalRepo,
		})
		Expect(err).NotTo(HaveOccurred())
		callback = campaignCallback(newAssistant(g, assistant.Repos{Messages: messageRepo, Proposals: proposalRepo}, pack))
	})

	AfterEach(func() {
		// Reset conversation + posts (and their notes) between specs; the campaign
		// persists. Notes are deleted first — they FK the posts.
		_, _ = db.NewDelete().TableExpr("assistant_messages").Where("scope_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("assistant_proposals").Where("scope_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_notes").Where("post_id IN (SELECT id FROM posts WHERE campaign_id = ?)", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("ideas").Where("campaign_id = ?", campaignID).Exec(ctx)
	})

	AfterAll(func() {
		if db == nil {
			return
		}
		_, _ = db.NewDelete().TableExpr("assistant_messages").Where("scope_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("assistant_proposals").Where("scope_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_notes").Where("post_id IN (SELECT id FROM posts WHERE campaign_id = ?)", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("posts").Where("campaign_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("ideas").Where("campaign_id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns").Where("id = ?", campaignID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("id = ?", userID).Exec(ctx)
		_, err := db.NewDelete().TableExpr("accounts").Where("id = ?", userID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("question answering", func() {
		It("answers a question about the campaign without mutating it", func() {
			resp, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "In one sentence, what is this campaign about? Do not change anything.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("answered"))
			Expect(resp.Explanation).NotTo(BeEmpty())
			Expect(resp.ContentPlan).To(BeNil(), "a question should not run the content-plan tool")
			Expect(resp.Brief).To(BeNil(), "a question should not run the enrich-brief tool")

			// No posts were created.
			posts, err := postRepo.ListByCampaign(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(posts).To(BeEmpty())
		})
	})

	Describe("brief enrichment", func() {
		It("enriches the brief and persists it to the campaign", func() {
			before, err := campaignRepo.GetByID(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())

			resp, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "Enrich and improve the campaign brief. Make the tone more technical and benchmark-driven.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("brief_enriched"))
			Expect(resp.Brief).NotTo(BeNil())
			Expect(resp.Brief.Applied).To(BeTrue())

			after, err := campaignRepo.GetByID(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			changed := after.Description != before.Description ||
				after.TargetPersona != before.TargetPersona ||
				after.KeyMessages != before.KeyMessages ||
				after.ToneGuidelines != before.ToneGuidelines
			Expect(changed).To(BeTrue(), "the enriched brief should be written back to the campaign")
			Expect(after.Description).NotTo(BeEmpty())
		})
	})

	Describe("content plan", func() {
		It("generates and persists draft posts, forwarding sub-flow events", func() {
			var gotContentPlanComplete, gotComplete bool
			onEvent := campaign_assistant.OnEventFunc(func(name campaign_assistant.SSEEventKind, _ any) {
				switch name {
				case campaign_assistant.SSEEventContentPlanComplete:
					gotContentPlanComplete = true
				case campaign_assistant.SSEEventKind(assistant.SSEEventComplete):
					gotComplete = true
				}
			})

			resp := proposeThenConfirm(ctx, callback, campaignID, "Generate a content plan for this campaign.", onEvent, func() {
				ideas, err := ideaRepo.List(ctx, repository.IdeaListFilter{CampaignID: campaignID})
				Expect(err).NotTo(HaveOccurred())
				Expect(ideas).To(BeEmpty(), "nothing is planned before the yes")
			})

			Expect(resp.Action).To(Equal("content_plan_generated"))
			Expect(resp.ContentPlan).NotTo(BeNil())
			Expect(resp.ContentPlan.IdeaCount).To(BeNumerically(">", 0))
			Expect(gotContentPlanComplete).To(BeTrue(), "the nested content_plan_complete event should be forwarded")
			Expect(gotComplete).To(BeTrue(), "complete signals the canonical final response")

			// The flow saves ideas inline; the DB count matches the report.
			ideas, err := ideaRepo.List(ctx, repository.IdeaListFilter{CampaignID: campaignID})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ideas)).To(Equal(resp.ContentPlan.IdeaCount))
		})
	})

	Describe("conversation history", func() {
		It("persists user and model turns", func() {
			_, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "Which platforms does this campaign target?",
			}, nil)
			Expect(err).NotTo(HaveOccurred())

			msgs, err := messageRepo.ListRecent(ctx, models.AssistantScopeCampaign, campaignID, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(msgs)).To(BeNumerically(">=", 2), "user instruction + model response should be persisted")

			var sawUser, sawModel bool
			for _, m := range msgs {
				switch m.Role {
				case "user":
					sawUser = true
				case "model":
					sawModel = true
					// Model turn is persisted as a compact JSON envelope.
					Expect(strings.HasPrefix(strings.TrimSpace(m.Content), "{")).To(BeTrue(),
						"model turn should be stored as JSON")
				}
			}
			Expect(sawUser).To(BeTrue())
			Expect(sawModel).To(BeTrue())
		})
	})

	Describe("campaign overview", func() {
		It("answers an overview question grounded in the getCampaignOverview tool", func() {
			resp, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "Give me a quick overview of this campaign and how the content is distributed across its phases.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("answered"))
			Expect(resp.Explanation).NotTo(BeEmpty())
			// Read-only: no content plan or brief mutation.
			Expect(resp.ContentPlan).To(BeNil())
			Expect(resp.Brief).To(BeNil())
		})
	})

	Describe("targeted generation", func() {
		It("adds a few targeted posts in the current phase and persists them", func() {
			resp, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "Add 2 LinkedIn posts in the current phase for the upcoming two weeks.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("posts_generated"))
			Expect(resp.GeneratedPosts).NotTo(BeNil())
			Expect(resp.GeneratedPosts.IdeaCount).To(BeNumerically(">", 0))

			ideas, err := ideaRepo.List(ctx, repository.IdeaListFilter{CampaignID: campaignID})
			Expect(err).NotTo(HaveOccurred())
			Expect(len(ideas)).To(Equal(resp.GeneratedPosts.IdeaCount))
			for _, i := range ideas {
				Expect(*i.Plan.PlatformID).To(Equal(platformID), "ideas should plan the requested platform")
			}
		})
	})

	Describe("draft post from research", func() {
		It("turns a prior research answer into a content-first draft with a Source research note", func() {
			// Turn 1: a research-style answer that persists as the latest model turn.
			_, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "In two or three sentences, summarise why Go suits production AI features for this campaign.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())

			// Turn 2: turn that research into a real LinkedIn post.
			var gotDraftComplete, gotComplete bool
			onEvent := campaign_assistant.OnEventFunc(func(name campaign_assistant.SSEEventKind, _ any) {
				switch name {
				case campaign_assistant.SSEEventDraftPostComplete:
					gotDraftComplete = true
				case campaign_assistant.SSEEventKind(assistant.SSEEventComplete):
					gotComplete = true
				}
			})
			resp, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "Great — create a LinkedIn post with this info for next week.",
			}, onEvent)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("post_drafted"))
			Expect(resp.DraftedPosts).NotTo(BeNil())
			Expect(resp.DraftedPosts.PostCount).To(BeNumerically(">", 0))
			Expect(gotDraftComplete).To(BeTrue(), "the draft_post_complete event should be forwarded")
			Expect(gotComplete).To(BeTrue(), "complete signals the canonical final response")

			posts, err := postRepo.ListByCampaign(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(posts)).To(Equal(resp.DraftedPosts.PostCount))

			p := posts[0]
			// Content-first: the finished copy lives in the post body (not the empty
			// content_plan body), status draft, on the requested platform.
			Expect(strings.TrimSpace(p.Content)).NotTo(BeEmpty(), "draft should carry full copy in Content")
			Expect(p.Status).To(Equal(models.PostStatusDraft))
			Expect(p.PlatformID).To(Equal(platformID))

			// A "Source research" note (type note, origin assistant) — and NO
			// draft_thesis note (that's the content_plan path, not this one).
			notes, err := postNoteRepo.ListByPostID(ctx, p.ID)
			Expect(err).NotTo(HaveOccurred())
			var sawSource bool
			for _, n := range notes {
				Expect(n.Type).NotTo(Equal(models.PostNoteTypeDraftThesis), "draftPost must not create a draft_thesis note")
				if n.Type == models.PostNoteTypeNote && n.Title == "Source research" {
					sawSource = true
					Expect(n.Origin).To(Equal(models.PostNoteOriginAssistant))
					Expect(strings.TrimSpace(n.Body)).NotTo(BeEmpty())
				}
			}
			Expect(sawSource).To(BeTrue(), "the source research should be kept as a reference note")
		})
	})

	Describe("change dates", func() {
		It("moves the campaign end date and saves it", func() {
			resp, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "Extend the campaign end date to 2026-09-30.",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())

			Expect(resp.Action).To(Equal("dates_updated"))
			Expect(resp.Dates).NotTo(BeNil())
			Expect(resp.Dates.EndDate).To(Equal("2026-09-30"))

			after, err := campaignRepo.GetByID(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(after.EndDate).NotTo(BeNil())
			Expect(after.EndDate.Format("2006-01-02")).To(Equal("2026-09-30"))
		})
	})

	Describe("redistribute", func() {
		It("re-dates draft posts across the campaign timeline", func() {
			full, err := campaignRepo.GetByID(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(full.CampaignType).NotTo(BeNil())
			Expect(full.CampaignType.Phases).NotTo(BeEmpty())
			phaseID := full.CampaignType.Phases[0].ID
			start, end := *full.StartDate, *full.EndDate

			// Seed 3 draft posts bunched on a single (out-of-order) date.
			for range 3 {
				id, _ := models.NewID()
				at := start.AddDate(0, 0, 1)
				Expect(postRepo.Create(ctx, &models.Post{
					ID:                  id,
					CampaignID:          campaignID,
					CampaignTypePhaseID: &phaseID,
					PlatformID:          platformID,
					PlatformPostType:    "text-post",
					Title:               "Draft",
					Content:             "c",
					Status:              models.PostStatusDraft,
					MediaURLs:           models.StringSlice{},
					UsedAssetIDs:        models.StringSlice{},
					CTAType:             models.CTATypeNone,
					CreatedBy:           userID,
					ScheduledAt:         &at,
				})).To(Succeed())
			}

			seeded := start.AddDate(0, 0, 1)
			resp := proposeThenConfirm(ctx, callback, campaignID, "Redistribute the drafts across the campaign.", nil, func() {
				posts, err := postRepo.ListByCampaign(ctx, campaignID)
				Expect(err).NotTo(HaveOccurred())
				Expect(posts).To(HaveLen(3))
				for _, p := range posts {
					Expect(p.ScheduledAt).NotTo(BeNil())
					Expect(p.ScheduledAt.Equal(seeded)).To(BeTrue(), "no post moves before the yes")
				}
			})

			Expect(resp.Action).To(Equal("posts_redistributed"))
			Expect(resp.Redistribute).NotTo(BeNil())
			Expect(resp.Redistribute.PostsUpdated).To(BeNumerically(">", 0))

			posts, err := postRepo.ListByCampaign(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			lo := start.AddDate(0, 0, -1)
			hi := end.AddDate(0, 0, 1)
			for _, p := range posts {
				if p.Status != models.PostStatusDraft {
					continue
				}
				Expect(p.ScheduledAt).NotTo(BeNil())
				Expect(p.ScheduledAt.After(lo)).To(BeTrue(), "redistributed date within range")
				Expect(p.ScheduledAt.Before(hi)).To(BeTrue(), "redistributed date within range")
			}
		})
	})

	Describe("attached assets (CON-118)", func() {
		It("auto-uses attached assets for generation, persists UseAssets, and grounds the binding", func() {
			// Seed a ready asset and attach it to the campaign for this spec only.
			assetID, err := models.NewID()
			Expect(err).NotTo(HaveOccurred())
			const assetTitle = "Go concurrency benchmark"
			Expect(assetRepo.Create(ctx, &models.Asset{
				ID:        assetID,
				Title:     assetTitle,
				Content:   "Benchmarks show goroutine pipelines sustain 1.2M msgs/sec on 8 cores with p99 latency under 3ms.",
				Status:    models.AssetStatusReady,
				TagIDs:    models.StringSlice{},
				CreatedBy: userID,
			})).To(Succeed())

			// Attach the asset but leave UseAssets OFF — the assistant should turn
			// it on because the campaign has ready attached assets.
			full, err := campaignRepo.GetByID(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			full.UseAssets = false
			full.AssetIDs = models.StringSlice{assetID}
			Expect(campaignRepo.Update(ctx, full)).To(Succeed())

			// Restore the campaign and remove the asset so later specs are unaffected.
			DeferCleanup(func() {
				if c, err := campaignRepo.GetByID(ctx, campaignID); err == nil {
					c.UseAssets = false
					c.AssetIDs = models.StringSlice{}
					_ = campaignRepo.Update(ctx, c)
				}
				_, _ = db.NewDelete().TableExpr("assets").Where("id = ?", assetID).Exec(ctx)
			})

			var assetsUsed []campaign_assistant.AssetRef
			onEvent := campaign_assistant.OnEventFunc(func(name campaign_assistant.SSEEventKind, data any) {
				if name == campaign_assistant.SSEEventAssetsUsed {
					if p, ok := data.(campaign_assistant.AssetsUsedEventPayload); ok {
						assetsUsed = p.Assets
					}
				}
			})

			resp := proposeThenConfirm(ctx, callback, campaignID, "Generate a content plan for this campaign.", onEvent, func() {
				ideas, err := ideaRepo.List(ctx, repository.IdeaListFilter{CampaignID: campaignID})
				Expect(err).NotTo(HaveOccurred())
				Expect(ideas).To(BeEmpty(), "nothing is planned before the yes")
				before, err := campaignRepo.GetByID(ctx, campaignID)
				Expect(err).NotTo(HaveOccurred())
				Expect(before.UseAssets).To(BeFalse(), "asset use isn't switched on before the yes")
			})
			Expect(resp.Action).To(Equal("content_plan_generated"))

			// Auto-use: the assistant enabled + persisted asset generation.
			updated, err := campaignRepo.GetByID(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(updated.UseAssets).To(BeTrue(), "assistant should persist UseAssets when the campaign has attached assets")

			// Provenance: the assets_used event names the attached asset.
			Expect(assetsUsed).To(ContainElement(campaign_assistant.AssetRef{ID: assetID, Title: assetTitle}))

			// Per-idea grounding: planning saves ideas, not posts. An idea
			// records only the assets the model drew on for it, never an id
			// outside the retrieved set, and its mode follows — and since the
			// single attached asset is highly relevant to this campaign, at
			// least one idea should cite it.
			ideas, err := ideaRepo.List(ctx, repository.IdeaListFilter{CampaignID: campaignID})
			Expect(err).NotTo(HaveOccurred())
			Expect(ideas).NotTo(BeEmpty())
			posts, err := postRepo.ListByCampaign(ctx, campaignID)
			Expect(err).NotTo(HaveOccurred())
			Expect(posts).To(BeEmpty(), "planning makes ideas, not posts")
			cited := false
			for _, idea := range ideas {
				for _, id := range idea.AssetIDs {
					Expect(id).To(Equal(assetID), "an idea cited an asset outside the retrieved set")
					cited = true
				}
				Expect(idea.GroundingMode).NotTo(BeNil())
				want := models.GroundingExploratory
				if len(idea.AssetIDs) > 0 {
					want = models.GroundingGrounded
				}
				Expect(*idea.GroundingMode).To(Equal(want))
			}
			Expect(cited).To(BeTrue(), "at least one idea should cite the attached asset")
		})

		It("handles an asset question without failing the turn", func() {
			// No embedder is wired in the harness, so askCampaignAssets degrades to
			// unavailable — the turn must still complete cleanly.
			resp, err := callback(ctx, campaignTurn{
				CampaignID:  campaignID,
				Instruction: "What do the attached assets say about concurrency benchmarks?",
			}, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp).NotTo(BeNil())
			Expect(resp.Explanation).NotTo(BeEmpty())
		})
	})
})

// proposeThenConfirm sends a plan-wide request, which the assistant proposes
// instead of running, checks with unchanged that nothing ran yet, then
// answers "yes" and returns the turn that ran it.
func proposeThenConfirm(
	ctx context.Context,
	callback func(context.Context, campaignTurn, campaign_assistant.OnEventFunc) (*campaignTurnResult, error),
	campaignID, instruction string,
	onEvent campaign_assistant.OnEventFunc,
	unchanged func(),
) *campaignTurnResult {
	GinkgoHelper()
	proposed, err := callback(ctx, campaignTurn{CampaignID: campaignID, Instruction: instruction}, nil)
	Expect(err).NotTo(HaveOccurred())
	Expect(proposed.Action).To(Equal("proposed"), "a plan-wide change waits for the user's yes")
	Expect(proposed.Proposals).To(HaveLen(1))
	unchanged()

	resp, err := callback(ctx, campaignTurn{CampaignID: campaignID, Instruction: "Yes, go ahead."}, onEvent)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp).NotTo(BeNil())
	return resp
}
