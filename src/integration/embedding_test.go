//go:build integration

package integration_test

import (
	"context"
	"os"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/googlegenai"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/genkit/flows"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Embedding integration tests run against the live Gemini Embedding 2
// API and skip when GEMINI_API_KEY is unset, so the suite still passes without
// API credentials.
const (
	embedModel      = "gemini-embedding-2"
	embedDimensions = 3072
)

// initGeminiEmbedder builds a Gemini-backed embedder for a suite and wires it
// into flows.Init. It Skips the calling spec when GEMINI_API_KEY is unset.
func initGeminiEmbedder(ctx context.Context, repo repository.AssetChunksRepository) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		Skip("GEMINI_API_KEY not set — skipping live Gemini Embedding 2 test")
	}
	embedopts.Dimensions = embedDimensions
	plugin := &googlegenai.GoogleAI{APIKey: apiKey}
	g := genkit.Init(ctx, genkit.WithPlugins(plugin))
	embedder, err := plugin.DefineEmbedder(g, embedModel, &ai.EmbedderOptions{Dimensions: embedDimensions})
	Expect(err).NotTo(HaveOccurred())
	flows.Init(g, embedder, repo, nil, nil, embedModel)
}

var _ = Describe("Asset embedding flow", Ordered, func() {
	var (
		ctx     context.Context
		db      *bun.DB
		repo    repository.AssetChunksRepository
		assetID string
	)

	BeforeAll(func() {
		ctx = tenantCtx()
		db = mustOpenIntegrationDB()
		repo = repository.NewAssetChunksRepository(db)

		// Initialise Genkit + the Gemini embedder once for the whole suite.
		initGeminiEmbedder(ctx, repo)

		// Seed a user and an asset to satisfy foreign-key constraints.
		userID, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "integration@test.local",
			PasswordHash: "placeholder",
			Name:         "Integration Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		user := &models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Integration Tester",
			Email:     "integration@test.local",
		}
		_, err = db.NewInsert().Model(user).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		assetID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		asset := &models.Asset{
			ID:        assetID,
			Title:     "Integration Asset",
			Content:   blocknoteDoc("Hello, embedding world!"),
			CreatedBy: userID,
		}
		_, err = db.NewInsert().Model(asset).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = db.NewDelete().TableExpr("assets_chunks").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("assets").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		_, err := db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	// ── Create ───────────────────────────────────────────────────────────────

	Describe("on asset create", func() {
		It("stores one or more chunks with non-empty embedding vectors", func() {
			_, err := flows.EmbedAssetFlow.Run(ctx, flows.EmbedAssetInput{
				AssetID: assetID,
				Title:   "Integration Asset",
				Content: blocknoteDoc("Hello, embedding world!"),
			})
			Expect(err).NotTo(HaveOccurred())

			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks).NotTo(BeEmpty())
			Expect(chunks[0].Embedding.Slice()).NotTo(BeEmpty())
			Expect(chunks[0].Model).NotTo(BeEmpty())
			Expect(chunks[0].ChunkIndex).To(Equal(0))

			// Verify the embedding decodes to the expected dimension (3072 for Gemini Embedding 2).
			vec := chunks[0].Embedding.Slice()
			Expect(vec).To(HaveLen(embedDimensions))
		})
	})

	// ── Update ───────────────────────────────────────────────────────────────

	Describe("on asset update", func() {
		It("replaces the chunks with fresh vectors", func() {
			_, err := flows.EmbedAssetFlow.Run(ctx, flows.EmbedAssetInput{
				AssetID: assetID,
				Title:   "Integration Asset — Revised",
				Content: blocknoteDoc("The content has been updated."),
			})
			Expect(err).NotTo(HaveOccurred())

			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks).NotTo(BeEmpty())
			Expect(chunks[0].Embedding.Slice()).NotTo(BeEmpty())
			Expect(chunks[0].Model).NotTo(BeEmpty())
		})

		It("returns an empty slice for an unknown asset ID", func() {
			chunks, err := repo.GetByAssetID(ctx, "nonexistent-id")
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks).To(BeEmpty())
		})
	})
})

// blocknoteDoc returns a minimal single-paragraph BlockNote JSON document.
func blocknoteDoc(text string) string {
	return `[{"type":"paragraph","content":[{"type":"text","text":"` + text + `","styles":{}}],"children":[]}]`
}
