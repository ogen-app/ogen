//go:build integration

package integration_test

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows"
	"github.com/ogen-app/ogen/src/infra/repository"
)

var _ = Describe("Asset chunking", Ordered, func() {
	var (
		ctx    context.Context
		db     *bun.DB
		repo   repository.AssetChunksRepository
		userID string
	)

	BeforeAll(func() {
		ctx = tenantCtx()
		db = mustOpenIntegrationDB()
		repo = repository.NewAssetChunksRepository(db)

		// Initialise a dedicated Genkit instance + embedder for this suite so
		// it is self-contained and does not depend on embedding_test.go's BeforeAll.
		initGeminiEmbedder(ctx, repo)

		var err error
		userID, err = models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{
			ID:           userID,
			Email:        "chunking@test.local",
			PasswordHash: "placeholder",
			Name:         "Chunking Tester",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.User{
			ID:        userID,
			AccountID: userID,
			TenantID:  models.DefaultTenantID,
			Name:      "Chunking Tester",
			Email:     "chunking@test.local",
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, _ = db.NewDelete().TableExpr("assets_chunks").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("assets").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		_, err := db.NewDelete().TableExpr("accounts").Where("id = ?", userID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	// seedAsset inserts an asset row and returns its ID.
	seedAsset := func(title, content string) string {
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Asset{
			ID:        id,
			Title:     title,
			Content:   content,
			CreatedBy: userID,
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		return id
	}

	embedAsset := func(assetID, title, content string) {
		_, err := flows.EmbedAssetFlow.Run(ctx, flows.EmbedAssetInput{
			AssetID: assetID,
			Title:   title,
			Content: content,
		})
		Expect(err).NotTo(HaveOccurred())
	}

	// ── Short asset (single chunk) ────────────────────────────────────────────

	Describe("short asset (content < MaxEmbedChars)", Ordered, func() {
		const shortText = "A brief piece of content."
		var assetID string

		BeforeAll(func() {
			assetID = seedAsset("Short Asset", blocknoteDoc(shortText))
			embedAsset(assetID, "Short Asset", blocknoteDoc(shortText))
		})

		It("produces exactly one chunk", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks).To(HaveLen(1))
		})

		It("stores chunk_index 0", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks[0].ChunkIndex).To(Equal(0))
		})

		It("stores non-empty content in the chunk", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks[0].Content).NotTo(BeEmpty())
		})

		It("stores a valid 3072-dim embedding vector", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks[0].Embedding.Slice()).To(HaveLen(embedDimensions))
		})

		It("records a non-zero token count", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks[0].TokenCount).To(BeNumerically(">", 0))
		})

		It("records the model name", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks[0].Model).NotTo(BeEmpty())
		})
	})

	// ── Long asset (multiple chunks) ──────────────────────────────────────────

	Describe("long asset (content > MaxEmbedChars)", Ordered, func() {
		var assetID string
		var longContent string

		BeforeAll(func() {
			longContent = longBlocknoteDoc(flows.MaxEmbedChars*2 + 500)
			assetID = seedAsset("Long Asset", longContent)
			embedAsset(assetID, "Long Asset", longContent)
		})

		It("produces more than one chunk", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(chunks)).To(BeNumerically(">", 1))
		})

		It("assigns sequential chunk_index values starting at 0", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			for i, c := range chunks {
				Expect(c.ChunkIndex).To(Equal(i))
			}
		})

		It("stores a valid 3072-dim embedding on every chunk", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			for i, c := range chunks {
				Expect(c.Embedding.Slice()).To(HaveLen(embedDimensions),
					"chunk %d embedding dimension mismatch", i)
			}
		})

		It("keeps each chunk within the MaxEmbedChars limit", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			for i, c := range chunks {
				Expect(len(c.Content)).To(BeNumerically("<=", flows.MaxEmbedChars),
					"chunk %d exceeds MaxEmbedChars (%d chars)", i, len(c.Content))
			}
		})

		It("carries overlap text from the end of one chunk into the next", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			if len(chunks) < 2 {
				Skip("need at least 2 chunks to verify overlap")
			}
			tail := chunks[0].Content
			if len(tail) > flows.ChunkOverlap {
				tail = tail[len(tail)-flows.ChunkOverlap:]
			}
			tail = strings.TrimSpace(tail)
			probe := tail
			if len(probe) > 80 {
				probe = probe[:80]
			}
			Expect(chunks[1].Content).To(ContainSubstring(probe),
				"overlap from chunk[0] not found in chunk[1]")
		})

		It("returns all the asset's chunks via pgvector SearchSimilar", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks).NotTo(BeEmpty())

			// Query with a chunk's own embedding. minScore -1.0 disables the
			// similarity floor (cosine similarity is always >= -1), so every
			// chunk of this asset comes back regardless of its angle to the
			// query — this exercises scoping + ordering, not a threshold. (A
			// floor of 0.0 would drop any chunk with negative cosine
			// similarity to chunks[0], which long multi-chunk docs can have.)
			hits, err := repo.SearchSimilar(ctx, chunks[0].Embedding, []string{assetID}, -1.0, 0)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(hits)).To(Equal(len(chunks)))
			for _, h := range hits {
				Expect(h.AssetID).To(Equal(assetID))
			}
		})
	})

	// ── Re-embed: long → short ────────────────────────────────────────────────

	Describe("re-embedding: long content replaced by short content", Ordered, func() {
		var assetID string

		BeforeAll(func() {
			longContent := longBlocknoteDoc(flows.MaxEmbedChars*2 + 500)
			assetID = seedAsset("Re-embed Long→Short", longContent)
			embedAsset(assetID, "Re-embed Long→Short", longContent)
		})

		It("atomically replaces all chunks with a single new chunk", func() {
			embedAsset(assetID, "Re-embed Long→Short", blocknoteDoc("Short replacement."))

			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks).To(HaveLen(1))
			Expect(chunks[0].ChunkIndex).To(Equal(0))
		})

		It("embeds the new content (not the old)", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks[0].Content).To(ContainSubstring("Short replacement."))
		})
	})

	// ── Re-embed: short → long ────────────────────────────────────────────────

	Describe("re-embedding: short content replaced by long content", Ordered, func() {
		var assetID string

		BeforeAll(func() {
			assetID = seedAsset("Re-embed Short→Long", blocknoteDoc("Initial short content."))
			embedAsset(assetID, "Re-embed Short→Long", blocknoteDoc("Initial short content."))
		})

		It("atomically replaces the single chunk with multiple new chunks", func() {
			newLong := longBlocknoteDoc(flows.MaxEmbedChars*2 + 1000)
			embedAsset(assetID, "Re-embed Short→Long", newLong)

			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(chunks)).To(BeNumerically(">", 1))
		})

		It("contains no stale content from the previous embed", func() {
			chunks, err := repo.GetByAssetID(ctx, assetID)
			Expect(err).NotTo(HaveOccurred())
			for _, c := range chunks {
				Expect(c.Content).NotTo(ContainSubstring("Initial short content."))
			}
		})
	})

	// ── DeleteByAssetID ───────────────────────────────────────────────────────

	Describe("DeleteByAssetID", func() {
		It("removes all chunks for the asset", func() {
			id := seedAsset("Delete Me", blocknoteDoc("to be deleted"))
			embedAsset(id, "Delete Me", blocknoteDoc("to be deleted"))

			Expect(repo.DeleteByAssetID(ctx, id)).To(Succeed())

			chunks, err := repo.GetByAssetID(ctx, id)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunks).To(BeEmpty())
		})
	})
})

// longBlocknoteDoc generates a BlockNote JSON document whose extracted plain
// text totals approximately targetChars characters using distinct paragraphs.
func longBlocknoteDoc(targetChars int) string {
	const para = "This is a paragraph of content used to test chunking behaviour in the asset embedding flow. "
	numParas := (targetChars / len(para)) + 1

	var sb strings.Builder
	sb.WriteString("[")
	for i := range numParas {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"type":"paragraph","content":[{"type":"text","text":"`)
		sb.WriteString(para)
		sb.WriteString(`","styles":{}}],"children":[]}`)
	}
	sb.WriteString("]")
	return sb.String()
}
