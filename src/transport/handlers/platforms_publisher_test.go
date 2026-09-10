package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers"
	pubzernio "github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// PlatformsHandler enrichment tests (CON-63). Sibling file to
// platforms_test.go so the original tests stay focused on CRUD.
var _ = Describe("PlatformsHandler publishers enrichment", Ordered, func() {
	var (
		app           *fiber.App
		db            *bun.DB
		authCookie    *http.Cookie
		platformRepo  repository.PlatformRepository
		accountRepo   repository.SocialAccountRepository
		settingRepo   repository.SettingRepository
		allowlistRepo repository.AutoPublishAllowlistRepository
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	// setupApp wires a fresh Fiber app with the supplied publisher
	// slice, seeds a login user, and stores the session cookie.
	setupApp := func(pubs []publishers.Publisher) {
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
		settingRepo = repository.NewSettingRepository(db)
		platformRepo = repository.NewPlatformRepository(db)
		accountRepo = repository.NewSocialAccountRepository(db)
		allowlistRepo = repository.NewAutoPublishAllowlistRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewPlatformsHandler(platformRepo, pubs, allowlistRepo, auth).Register(app)

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
	}

	BeforeEach(func() {
		// Ensure the linkedin platform row exists. Other tests in the
		// suite may delete seeded rows; INSERT...ON CONFLICT keeps this
		// test resilient to ordering.
		_, err := db.NewInsert().Model(&models.Platform{
			ID:      "linkedin",
			Name:    "LinkedIn",
			Enabled: true, // CON-292: enabled defaults to false; this row must be live to enrich.
			PostTypes: models.PostTypeMap{
				"text-post":  "Text post",
				"image-post": "Image post",
				"carousel":   "Carousel",
				"video":      "Video",
				"article":    "Article",
			},
		}).On("CONFLICT (id) DO NOTHING").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		ctx := tenantCtx()
		_, _ = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("social_accounts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("settings").Where("key LIKE ?", "zernio.%").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("auto_publish_allowlist").Where("1 = 1").Exec(ctx)
	})

	// ── No publishers configured ────────────────────────────────────

	Describe("with no publishers", func() {
		BeforeEach(func() { setupApp(nil) })

		It("emits publishers: [] (not null) for every platform on List", func() {
			req := httptest.NewRequest("GET", "/api/platforms", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body []map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body).NotTo(BeEmpty())
			for _, p := range body {
				pubs, ok := p["publishers"].([]any)
				Expect(ok).To(BeTrue(), "publishers should be a JSON array, not null")
				Expect(pubs).To(BeEmpty())
			}
		})

		It("emits publishers: [] on Get", func() {
			req := httptest.NewRequest("GET", "/api/platforms/linkedin", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			pubs, ok := body["publishers"].([]any)
			Expect(ok).To(BeTrue())
			Expect(pubs).To(BeEmpty())
		})
	})

	// ── Zernio publisher configured ─────────────────────────────────

	Describe("with Zernio configured", func() {
		buildZernioPublisher := func() publishers.Publisher {
			integ := pubzernio.NewIntegration(pubzernio.NewClient(pubzernio.StaticKey("test-key"), "http://stub", pubzernio.ClientOpts{Timeout: time.Second}))
			integ.SetState(pubzernio.StateOK)
			store := &settingStoreFromRepo{repo: settingRepo}
			Expect(store.Set(tenantCtx(), pubzernio.SettingProfileID, "p_test")).To(Succeed())
			return pubzernio.NewPublisher(integ, accountRepo, store)
		}

		It("with no accounts: connected=false, supported_post_types populated, accounts=[]", func() {
			setupApp([]publishers.Publisher{buildZernioPublisher()})

			req := httptest.NewRequest("GET", "/api/platforms/linkedin", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())

			pubs := body["publishers"].([]any)
			Expect(pubs).To(HaveLen(1))
			z := pubs[0].(map[string]any)
			Expect(z["id"]).To(Equal("zernio"))
			Expect(z["name"]).To(Equal("Zernio"))
			Expect(z["state"]).To(Equal("ok"))
			Expect(z["connected"]).To(Equal(false))

			spt := z["supported_post_types"].([]any)
			Expect(spt).NotTo(BeEmpty())

			accts, ok := z["accounts"].([]any)
			Expect(ok).To(BeTrue(), "accounts should be a JSON array, not null")
			Expect(accts).To(BeEmpty())
		})

		It("matches platforms by name when local IDs differ from the allowlist (Sqid IDs)", func() {
			// Simulate a deployment where platforms.id holds a Sqid
			// rather than the seeded "instagram". The publisher
			// allowlist still uses "instagram" — the name fallback
			// should match it to the local row.
			ctx := tenantCtx()
			_, err := db.NewInsert().Model(&models.Platform{
				ID:      "rzgpTkARLH0L",
				Name:    "Instagram",
				Enabled: true, // CON-292: explicit since enabled now defaults to false.
				PostTypes: models.PostTypeMap{
					"image-post": "Image post",
					"reel":       "Reel",
				},
			}).On("CONFLICT (id) DO NOTHING").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())

			setupApp([]publishers.Publisher{buildZernioPublisher()})

			now := time.Now().UTC()
			Expect(accountRepo.ApplyPlan(ctx, []models.SocialAccount{{
				ID:           "acc_ig",
				Platform:     "instagram",
				ProfileID:    "p_test",
				Username:     "ogen-team",
				DisplayName:  "Ogen",
				IsActive:     true,
				RawJSON:      "{}",
				ConnectedAt:  now,
				LastSyncedAt: now,
			}}, nil, now)).To(Succeed())

			req := httptest.NewRequest("GET", "/api/platforms/rzgpTkARLH0L", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			pubs := body["publishers"].([]any)
			Expect(pubs).To(HaveLen(1), "name fallback should attach Zernio publisher despite ID mismatch")
			z := pubs[0].(map[string]any)
			Expect(z["connected"]).To(Equal(true))
			Expect(z["accounts"].([]any)).To(HaveLen(1))
		})

		It("with a connected account: connected=true and the account in the response", func() {
			setupApp([]publishers.Publisher{buildZernioPublisher()})

			now := time.Now().UTC()
			Expect(accountRepo.ApplyPlan(tenantCtx(), []models.SocialAccount{{
				ID:           "acc1",
				Platform:     "linkedin", // Zernio's platform identifier
				ProfileID:    "p_test",
				Username:     "ogen-team",
				DisplayName:  "Ogen",
				AvatarURL:    "https://example.com/a.png",
				IsActive:     true,
				RawJSON:      "{}",
				ConnectedAt:  now,
				LastSyncedAt: now,
			}}, nil, now)).To(Succeed())

			req := httptest.NewRequest("GET", "/api/platforms/linkedin", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())

			z := body["publishers"].([]any)[0].(map[string]any)
			Expect(z["connected"]).To(Equal(true))
			accts := z["accounts"].([]any)
			Expect(accts).To(HaveLen(1))
			acc := accts[0].(map[string]any)
			Expect(acc["id"]).To(Equal("acc1"))
			Expect(acc["username"]).To(Equal("ogen-team"))
			Expect(acc["display_name"]).To(Equal("Ogen"))
			Expect(acc["is_active"]).To(Equal(true))
		})
	})

	// ── auto_publish_allowed (CON-65) ───────────────────────────────

	Describe("auto_publish_allowed", func() {
		// Initialise the package-level repos directly so this Describe
		// is self-contained under `-ginkgo.focus`. The other Describes
		// rely on a sibling BeforeEach having run first to populate
		// settingRepo/accountRepo — fine for the full suite, broken
		// under focus.
		BeforeEach(func() {
			settingRepo = repository.NewSettingRepository(db)
			accountRepo = repository.NewSocialAccountRepository(db)
			allowlistRepo = repository.NewAutoPublishAllowlistRepository(db)
		})

		buildZernioPublisher := func() publishers.Publisher {
			integ := pubzernio.NewIntegration(pubzernio.NewClient(pubzernio.StaticKey("test-key"), "http://stub", pubzernio.ClientOpts{Timeout: time.Second}))
			integ.SetState(pubzernio.StateOK)
			store := &settingStoreFromRepo{repo: settingRepo}
			Expect(store.Set(tenantCtx(), pubzernio.SettingProfileID, "p_test")).To(Succeed())
			return pubzernio.NewPublisher(integ, accountRepo, store)
		}

		It("is false for every publisher when the allowlist is empty", func() {
			setupApp([]publishers.Publisher{buildZernioPublisher()})

			req := httptest.NewRequest("GET", "/api/platforms", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body []map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body).NotTo(BeEmpty())
			for _, p := range body {
				for _, pub := range p["publishers"].([]any) {
					z := pub.(map[string]any)
					Expect(z["auto_publish_allowed"]).To(Equal(false))
				}
			}
		})

		It("is true only for the allowlisted platform's publisher entry", func() {
			setupApp([]publishers.Publisher{buildZernioPublisher()})

			Expect(allowlistRepo.Set(tenantCtx(), []string{"linkedin"})).To(Succeed())

			req := httptest.NewRequest("GET", "/api/platforms", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body []map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())

			seenLinkedInAllowed := false
			for _, p := range body {
				name, _ := p["name"].(string)
				for _, pub := range p["publishers"].([]any) {
					z := pub.(map[string]any)
					allowed, _ := z["auto_publish_allowed"].(bool)
					if name == "LinkedIn" {
						Expect(allowed).To(BeTrue(), "LinkedIn publisher should be auto-publish-allowed")
						seenLinkedInAllowed = true
					} else {
						Expect(allowed).To(BeFalse(), "non-allowlisted platform %q should not be auto-publish-allowed", name)
					}
				}
			}
			Expect(seenLinkedInAllowed).To(BeTrue(), "expected to see LinkedIn enriched in the response")
		})

		It("maps Zernio's wire ID to the correct platform for X (Twitter)", func() {
			// Zernio uses "twitter" on the wire; Ogen's local OgenID is
			// "x-twitter". The allowlist stores Zernio wire IDs, so
			// putting "twitter" in must mark the X platform's publisher
			// view as auto-publish-allowed — proving the
			// PublisherPlatformID join key works for vocabularies that
			// drift between publisher and Ogen.
			setupApp([]publishers.Publisher{buildZernioPublisher()})

			Expect(allowlistRepo.Set(tenantCtx(), []string{"twitter"})).To(Succeed())

			req := httptest.NewRequest("GET", "/api/platforms", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body []map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())

			seenXAllowed := false
			for _, p := range body {
				name, _ := p["name"].(string)
				for _, pub := range p["publishers"].([]any) {
					z := pub.(map[string]any)
					allowed, _ := z["auto_publish_allowed"].(bool)
					if name == "X (Twitter)" {
						Expect(allowed).To(BeTrue())
						seenXAllowed = true
					} else {
						Expect(allowed).To(BeFalse())
					}
				}
			}
			Expect(seenXAllowed).To(BeTrue(), "expected to see X (Twitter) enriched in the response")
		})

		It("reflects allowlist changes between requests", func() {
			setupApp([]publishers.Publisher{buildZernioPublisher()})

			Expect(allowlistRepo.Set(tenantCtx(), []string{"threads"})).To(Succeed())

			extract := func() map[string]bool {
				req := httptest.NewRequest("GET", "/api/platforms", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				var body []map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
				out := map[string]bool{}
				for _, p := range body {
					name, _ := p["name"].(string)
					for _, pub := range p["publishers"].([]any) {
						z := pub.(map[string]any)
						allowed, _ := z["auto_publish_allowed"].(bool)
						out[name] = allowed
					}
				}
				return out
			}

			before := extract()
			Expect(before["Threads"]).To(BeTrue())
			Expect(before["LinkedIn"]).To(BeFalse())

			Expect(allowlistRepo.Set(tenantCtx(), []string{"linkedin"})).To(Succeed())

			after := extract()
			Expect(after["Threads"]).To(BeFalse())
			Expect(after["LinkedIn"]).To(BeTrue())
		})
	})
})
