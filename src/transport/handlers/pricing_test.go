package handlers_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("PricingHandler", func() {
	var app *fiber.App

	BeforeEach(func() {
		db := mustOpenTestDB()
		versionRepo := repository.NewTenantTierVersionRepository(db)
		assignmentRepo := repository.NewTenantTierAssignmentRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		cat, err := entitlements.LoadCatalog()
		Expect(err).NotTo(HaveOccurred())
		resolver := entitlements.NewResolver(versionRepo, assignmentRepo, tenantRepo, cat)

		app = fiber.New()
		noAuth := func(c *fiber.Ctx) error { return c.Next() }
		handlers.NewPricingHandler(resolver, versionRepo, cat, noAuth).Register(app)
	})

	Describe("GET /api/public/pricing", func() {
		It("returns the offered tiers unauthenticated with cache + CORS headers", func() {
			req := httptest.NewRequest("GET", "/api/public/pricing", nil)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))
			Expect(resp.Header.Get("Access-Control-Allow-Origin")).To(Equal("*"))
			Expect(resp.Header.Get("Cache-Control")).To(ContainSubstring("max-age"))

			body, _ := io.ReadAll(resp.Body)
			var payload struct {
				Tiers []struct {
					TierID       string `json:"tier_id"`
					Entitlements []struct {
						Key   string `json:"key"`
						Value any    `json:"value"`
					} `json:"entitlements"`
					Prices []struct {
						NetMinor int64  `json:"net_minor"`
						Currency string `json:"currency"`
					} `json:"prices"`
				} `json:"tiers"`
			}
			Expect(json.Unmarshal(body, &payload)).To(Succeed())

			// Only the Trial tier is active + purchasable at seed time (Pro/Max are
			// still drafts, the internal default tier is not purchasable).
			Expect(payload.Tiers).To(HaveLen(1))
			Expect(payload.Tiers[0].TierID).To(Equal("trial"))
			Expect(payload.Tiers[0].Entitlements).NotTo(BeEmpty())
			Expect(payload.Tiers[0].Prices).To(HaveLen(1))
			Expect(payload.Tiers[0].Prices[0].NetMinor).To(Equal(int64(0)))
			Expect(payload.Tiers[0].Prices[0].Currency).To(Equal("EUR"))
		})
	})
})
