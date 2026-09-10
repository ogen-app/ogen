package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"bytes"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// The tenant platforms surface is read-only (CON-292): platform lifecycle moved
// to the operator-only PlatformAdminService gRPC, so the former POST/PUT/DELETE
// specs were removed with those endpoints. GET filters to enabled platforms.
var _ = Describe("PlatformsHandler", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		app = fiber.New(fiber.Config{
			ErrorHandler: func(c *fiber.Ctx, err error) error {
				code := fiber.StatusInternalServerError
				if e, ok := err.(*fiber.Error); ok {
					code = e.Code
				}
				return c.Status(code).JSON(fiber.Map{"error": err.Error()})
			},
		})
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		settingRepo := repository.NewSettingRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewPlatformsHandler(platformRepo, nil, nil, auth).Register(app)

		// Seed an auth user and log in
		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("platforms").Where("id NOT IN ('AXqWG7U2qnpt','8S8bWQTG6qD','zBU1zqVICGfk','81mUCmc2xsKd','pQ4yxT3SuE57','rzgpTkARLH0L')").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// ── List ─────────────────────────────────────────────────────────────────

	Describe("GET /api/platforms", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/platforms", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the six enabled seeded platforms", func() {
				req := httptest.NewRequest("GET", "/api/platforms", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var platforms []models.Platform
				Expect(json.NewDecoder(resp.Body).Decode(&platforms)).To(Succeed())
				// The six seeded platforms are enabled; any disabled catalog rows
				// (e.g. TikTok/Pinterest/Reddit) are filtered out (CON-292 §11).
				Expect(platforms).To(HaveLen(6))
				for _, p := range platforms {
					Expect(p.Enabled).To(BeTrue())
					Expect(p.ZernioID).NotTo(BeEmpty())
				}
			})

			It("orders platforms by sort_order", func() {
				req := httptest.NewRequest("GET", "/api/platforms", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var platforms []models.Platform
				Expect(json.NewDecoder(resp.Body).Decode(&platforms)).To(Succeed())
				Expect(platforms).NotTo(BeEmpty())
				for i := 1; i < len(platforms); i++ {
					Expect(platforms[i].SortOrder >= platforms[i-1].SortOrder).To(BeTrue())
				}
			})
		})
	})

	// ── Get ──────────────────────────────────────────────────────────────────

	Describe("GET /api/platforms/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/platforms/AXqWG7U2qnpt", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the seeded LinkedIn platform with all post types, cadence, and constraints", func() {
				req := httptest.NewRequest("GET", "/api/platforms/AXqWG7U2qnpt", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var p models.Platform
				Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
				Expect(p.ID).To(Equal("AXqWG7U2qnpt"))
				Expect(p.Name).To(Equal("LinkedIn"))
				Expect(p.PostTypes).To(HaveKey("text-post"))
				Expect(p.PostTypes).To(HaveKey("article"))
				Expect(p.PostTypes).To(HaveLen(9))
				Expect(p.Cadence).To(Equal("1–2 posts per week"))
				Expect(p.Constraints).To(ContainSubstring("3000 chars"))
				// CON-292 backfill: the row carries its Zernio slug + publishable subset.
				Expect(p.ZernioID).To(Equal("linkedin"))
				Expect(p.SupportedPostTypes).To(ContainElement("article"))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("GET", "/api/platforms/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Post-type rules ────────────────────────────────────────────────────────

	Describe("GET /api/platforms/:id/post-type-rules", func() {
		const linkedInID = "AXqWG7U2qnpt"

		type resolvedRule struct {
			RequiresContent bool     `json:"requires_content"`
			AllowedKinds    []string `json:"allowed_kinds"`
			MinAttachments  int      `json:"min_attachments"`
			MaxAttachments  *int     `json:"max_attachments"`
		}
		type ruleView struct {
			Slug          string        `json:"slug"`
			Label         string        `json:"label"`
			WhitelistOnly bool          `json:"whitelist_only"`
			Rule          *resolvedRule `json:"rule"`
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/platforms/"+linkedInID+"/post-type-rules", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the resolved rules for every slug in PostTypes", func() {
				req := httptest.NewRequest("GET", "/api/platforms/"+linkedInID+"/post-type-rules", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got []ruleView
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())

				bySlug := map[string]ruleView{}
				for _, v := range got {
					bySlug[v.Slug] = v
				}
				// LinkedIn seed: text-post, image-post, carousel, video,
				// article, poll, newsletter, event, live-video.
				Expect(bySlug).To(HaveKey("text-post"))
				Expect(bySlug).To(HaveKey("image-post"))
				Expect(bySlug).To(HaveKey("carousel"))
				Expect(bySlug).To(HaveKey("event"))

				// image-post.max_attachments must resolve to LinkedIn's image cap
				// (20 since CON-123 corrected the seed against Zernio's docs).
				img := bySlug["image-post"]
				Expect(img.WhitelistOnly).To(BeFalse())
				Expect(img.Rule).NotTo(BeNil())
				Expect(img.Rule.AllowedKinds).To(ConsistOf("image"))
				Expect(img.Rule.MinAttachments).To(Equal(1))
				Expect(img.Rule.MaxAttachments).NotTo(BeNil())
				Expect(*img.Rule.MaxAttachments).To(Equal(20))

				// carousel — Min 2, Max resolves to the same image cap (20).
				car := bySlug["carousel"]
				Expect(car.Rule).NotTo(BeNil())
				Expect(car.Rule.MinAttachments).To(Equal(2))
				Expect(*car.Rule.MaxAttachments).To(Equal(20))

				// event has no rule entry — whitelist-only.
				ev := bySlug["event"]
				Expect(ev.WhitelistOnly).To(BeTrue())
				Expect(ev.Rule).To(BeNil())

				// poll — requires content, zero attachments.
				poll := bySlug["poll"]
				Expect(poll.Rule).NotTo(BeNil())
				Expect(poll.Rule.RequiresContent).To(BeTrue())
				Expect(poll.Rule.MaxAttachments).NotTo(BeNil())
				Expect(*poll.Rule.MaxAttachments).To(Equal(0))
			})

			It("returns 404 for an unknown platform id", func() {
				req := httptest.NewRequest("GET", "/api/platforms/nonexistent/post-type-rules", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})
})
