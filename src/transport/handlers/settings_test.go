package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("SettingsHandler", Ordered, func() {
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
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewSettingsHandler(settingRepo, auth).Register(app)

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
		_, err := db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Reset settings to their seeded defaults; remove any extra keys created by tests
		_, err = db.NewUpdate().TableExpr("settings").Set("value = ?", "false").
			Where("key = ?", "setup_complete").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("settings").Where("key != ?", "setup_complete").
			Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// ── List ─────────────────────────────────────────────────────────────────

	Describe("GET /api/settings", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/settings", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns all settings including the seeded default", func() {
				req := httptest.NewRequest("GET", "/api/settings", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var settings []models.Setting
				Expect(json.NewDecoder(resp.Body).Decode(&settings)).To(Succeed())
				Expect(settings).To(HaveLen(1))
				Expect(settings[0].Key).To(Equal("setup_complete"))
				Expect(settings[0].Value).To(Equal("false"))
			})
		})
	})

	// ── Get ──────────────────────────────────────────────────────────────────

	Describe("GET /api/settings/:key", func() {
		// CON-97: GET /:key is always authenticated. The setup_complete bootstrap
		// gate (unauthenticated reads while first-run setup was incomplete) was
		// removed once signup via POST /api/tenants became the sole onboarding path.
		Context("when not authenticated", func() {
			It("returns 401 for an existing key", func() {
				req := httptest.NewRequest("GET", "/api/settings/setup_complete", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})

			It("returns 401 for an unknown key (auth runs before the lookup)", func() {
				req := httptest.NewRequest("GET", "/api/settings/nonexistent", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the setting", func() {
				req := httptest.NewRequest("GET", "/api/settings/setup_complete", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var s models.Setting
				Expect(json.NewDecoder(resp.Body).Decode(&s)).To(Succeed())
				Expect(s.Key).To(Equal("setup_complete"))
				Expect(s.Value).To(Equal("false"))
			})

			It("returns 404 for an unknown key", func() {
				req := httptest.NewRequest("GET", "/api/settings/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Upsert ───────────────────────────────────────────────────────────────

	Describe("PUT /api/settings/:key", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"value": "true"})
				req := httptest.NewRequest("PUT", "/api/settings/setup_complete", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("updates an existing setting", func() {
				body, _ := json.Marshal(fiber.Map{"value": "true"})
				req := httptest.NewRequest("PUT", "/api/settings/setup_complete", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var s models.Setting
				Expect(json.NewDecoder(resp.Body).Decode(&s)).To(Succeed())
				Expect(s.Key).To(Equal("setup_complete"))
				Expect(s.Value).To(Equal("true"))
			})

			It("creates a new setting when the key does not exist", func() {
				body, _ := json.Marshal(fiber.Map{"value": "hello"})
				req := httptest.NewRequest("PUT", "/api/settings/custom_key", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var s models.Setting
				Expect(json.NewDecoder(resp.Body).Decode(&s)).To(Succeed())
				Expect(s.Key).To(Equal("custom_key"))
				Expect(s.Value).To(Equal("hello"))
			})

			It("returns 400 for a missing value", func() {
				body, _ := json.Marshal(fiber.Map{})
				req := httptest.NewRequest("PUT", "/api/settings/setup_complete", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			// CON-78: the workspace timezone must be a valid IANA zone.
			It("accepts a valid IANA timezone", func() {
				body, _ := json.Marshal(fiber.Map{"value": "Europe/Kyiv"})
				req := httptest.NewRequest("PUT", "/api/settings/timezone", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
			})

			It("rejects an invalid timezone with 400", func() {
				body, _ := json.Marshal(fiber.Map{"value": "Mars/Olympus_Mons"})
				req := httptest.NewRequest("PUT", "/api/settings/timezone", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Delete ───────────────────────────────────────────────────────────────

	Describe("DELETE /api/settings/:key", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/settings/setup_complete", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("deletes an existing setting and returns 204", func() {
				// Create a custom setting to delete
				upsertBody, _ := json.Marshal(fiber.Map{"value": "to-be-deleted"})
				upsertReq := httptest.NewRequest("PUT", "/api/settings/temp_key", bytes.NewReader(upsertBody))
				upsertReq.Header.Set("Content-Type", "application/json")
				upsertReq.AddCookie(authCookie)
				upsertResp, err := app.Test(upsertReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(upsertResp.StatusCode).To(Equal(200))

				delReq := httptest.NewRequest("DELETE", "/api/settings/temp_key", nil)
				delReq.AddCookie(authCookie)
				delResp, err := app.Test(delReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(delResp.StatusCode).To(Equal(204))
			})

			It("returns 404 for a non-existent key", func() {
				req := httptest.NewRequest("DELETE", "/api/settings/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})
})
