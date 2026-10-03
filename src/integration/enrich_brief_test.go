//go:build integration

package integration_test

import (
	"context"
	"os"
	"unicode"

	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/infra/repository"
)

var _ = Describe("Enrich brief flow", Ordered, func() {
	var (
		ctx          context.Context
		db           *bun.DB
		userID       string
		campaignRepo repository.CampaignRepository
		callback     func(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error)
	)

	BeforeAll(func() {
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			Skip("ANTHROPIC_API_KEY not set — skipping enrich brief integration tests")
		}

		ctx = tenantCtx()
		db = mustOpenIntegrationDB()

		tagRepo := repository.NewTagRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo = repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)

		var err error
		userID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "eb-integration@test.local",
			PasswordHash: "placeholder",
			Name:         "Enrich Brief Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Enrich Brief Tester",
			Email:     "eb-integration@test.local",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		g := genkit.Init(ctx, genkit.WithPlugins(&anthropic.Anthropic{}))
		modelID := os.Getenv("MODEL_ID")
		if modelID == "" {
			modelID = "claude-haiku-4-5-20251001"
		}
		initModelConfig(ctx, modelID, modelID)
		Expect(enrich_brief.InitEnrichBrief(g, enrich_brief.EnrichBriefFlowConfig{}, enrich_brief.EnrichBriefRepos{
			Campaigns:     campaignRepo,
			CampaignTypes: campaignTypeRepo,
		})).To(Succeed())
		callback = enrich_brief.NewEnrichBriefCallback()
	})

	AfterEach(func() {
		_, _ = db.NewDelete().TableExpr("campaigns").Where("created_by = ?", userID).Exec(ctx)
	})

	AfterAll(func() {
		if db == nil {
			return
		}
		_, _ = db.NewDelete().TableExpr("campaigns").Where("created_by = ?", userID).Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("id = ?", userID).Exec(ctx)
		_, err := db.NewDelete().TableExpr("accounts").Where("id = ?", userID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	// seedCampaign creates a minimal campaign — only a title, a real type
	// ("Uk" = a seeded campaign type with phases), and a language — and
	// returns its id. The brief fields are intentionally left empty: this is
	// generation-from-minimal-input.
	seedCampaign := func(name, language string) string {
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		Expect(campaignRepo.Create(ctx, &models.Campaign{
			ID:             id,
			Name:           name,
			CampaignTypeID: "Uk",
			Status:         models.StatusDraft,
			Language:       language,
			CreatedBy:      userID,
		})).To(Succeed())
		return id
	}

	hasCyrillic := func(s string) bool {
		for _, r := range s {
			if unicode.Is(unicode.Cyrillic, r) {
				return true
			}
		}
		return false
	}

	It("generates all four brief fields from a title and type", func() {
		id := seedCampaign("Launch of our Go-native analytics platform", "English")

		resp, err := callback(ctx, enrich_brief.EnrichBriefRequest{CampaignID: id}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp).NotTo(BeNil())
		Expect(resp.Description).NotTo(BeEmpty())
		Expect(resp.TargetPersona).NotTo(BeEmpty())
		Expect(resp.KeyMessages).NotTo(BeEmpty())
		Expect(resp.ToneGuidelines).NotTo(BeEmpty())
	})

	It("writes the brief in the campaign's language", func() {
		id := seedCampaign("Запуск нашої платформи аналітики", "Ukrainian")

		resp, err := callback(ctx, enrich_brief.EnrichBriefRequest{CampaignID: id}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp).NotTo(BeNil())
		Expect(resp.Description).NotTo(BeEmpty())
		// The brief must follow the campaign's chosen language, not default
		// to English — so a Ukrainian campaign yields Cyrillic prose.
		Expect(hasCyrillic(resp.Description)).To(BeTrue(),
			"expected the description to be written in Ukrainian (Cyrillic), got: %s", resp.Description)
	})

	It("edits an existing brief instead of regenerating it from the type", func() {
		id := seedCampaign("Practitioner AI", "English")
		c, err := campaignRepo.GetByID(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		c.Description = "A content programme to position Alec as a practitioner's voice on AI integration — " +
			"someone who ships AI/ML systems for enterprise clients (Microsoft, Cambridge, financial institutions) " +
			"rather than someone commenting on AI from the sidelines."
		c.TargetPersona = "CTOs and heads of data at mid-size enterprises weighing their first production AI system."
		c.KeyMessages = "Shipping beats speculating.\nIntegration is the hard part, not the model.\nEvidence over enthusiasm."
		c.ToneGuidelines = "Anti-hype practitioner voice. British English. No second-person address."
		Expect(campaignRepo.Update(ctx, c)).To(Succeed())

		resp, err := callback(ctx, enrich_brief.EnrichBriefRequest{
			CampaignID:  id,
			Instruction: "Improve the brief. Keep the anti-hype practitioner voice and British English exactly as it is; just tighten it and make the key messages sharper.",
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp).NotTo(BeNil())
		// The subject survives: the AI-integration practitioner is still who the
		// campaign is about.
		Expect(resp.Description).To(ContainSubstring("Alec"))
		Expect(resp.Description).To(MatchRegexp(`(?i)\bAI\b`))
		// The stored tone guideline ("No second-person address") binds the output.
		Expect(resp.KeyMessages).NotTo(MatchRegexp(`(?i)\byou(r|'ll)?\b`))
	})
})
