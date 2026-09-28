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

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("AutoPublishAllowlistHandler", Ordered, func() {
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
		allowlistRepo := repository.NewAutoPublishAllowlistRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth, nil, nil).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewAutoPublishAllowlistHandler(allowlistRepo, auth).Register(app)

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
		ctx := context.Background()
		_, err := db.NewDelete().TableExpr("auto_publish_allowlist").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	// ── GET ──────────────────────────────────────────────────────────────────

	Describe("GET /api/auto-publish-allowlist", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/auto-publish-allowlist", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns an empty list by default", func() {
				req := httptest.NewRequest("GET", "/api/auto-publish-allowlist", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var body map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
				platforms, ok := body["platforms"].([]any)
				Expect(ok).To(BeTrue(), "platforms should be a JSON array, not null")
				Expect(platforms).To(BeEmpty())
			})
		})
	})

	// ── PUT ──────────────────────────────────────────────────────────────────

	Describe("PUT /api/auto-publish-allowlist", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin"}})
				req := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("persists a valid Z subset and returns it on subsequent GET", func() {
				body, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin", "twitter"}})
				req := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				platforms := got["platforms"].([]any)
				Expect(platforms).To(ConsistOf("linkedin", "twitter"))

				getReq := httptest.NewRequest("GET", "/api/auto-publish-allowlist", nil)
				getReq.AddCookie(authCookie)
				getResp, err := app.Test(getReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(getResp.StatusCode).To(Equal(200))

				var getBody map[string]any
				Expect(json.NewDecoder(getResp.Body).Decode(&getBody)).To(Succeed())
				Expect(getBody["platforms"]).To(ConsistOf("linkedin", "twitter"))
			})

			It("rejects a platform that is not in Z with 400", func() {
				body, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin", "tiktok"}})
				req := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))

				var body2 map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&body2)).To(Succeed())
				Expect(body2["error"]).To(ContainSubstring("tiktok"))
			})

			It("does not partially persist when one platform is invalid", func() {
				body, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin", "tiktok"}})
				req := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				_, _ = app.Test(req)

				getReq := httptest.NewRequest("GET", "/api/auto-publish-allowlist", nil)
				getReq.AddCookie(authCookie)
				getResp, err := app.Test(getReq)
				Expect(err).NotTo(HaveOccurred())

				var got map[string]any
				Expect(json.NewDecoder(getResp.Body).Decode(&got)).To(Succeed())
				Expect(got["platforms"]).To(BeEmpty())
			})

			It("clears the allowlist when given an empty array", func() {
				seedBody, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin"}})
				seedReq := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(seedBody))
				seedReq.Header.Set("Content-Type", "application/json")
				seedReq.AddCookie(authCookie)
				seedResp, err := app.Test(seedReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(seedResp.StatusCode).To(Equal(200))

				clearBody, _ := json.Marshal(fiber.Map{"platforms": []string{}})
				clearReq := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(clearBody))
				clearReq.Header.Set("Content-Type", "application/json")
				clearReq.AddCookie(authCookie)
				clearResp, err := app.Test(clearReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(clearResp.StatusCode).To(Equal(200))

				var got map[string]any
				Expect(json.NewDecoder(clearResp.Body).Decode(&got)).To(Succeed())
				Expect(got["platforms"]).To(BeEmpty())
			})

			It("replaces (not appends) when called with a different set", func() {
				seedBody, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin", "twitter"}})
				seedReq := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(seedBody))
				seedReq.Header.Set("Content-Type", "application/json")
				seedReq.AddCookie(authCookie)
				_, _ = app.Test(seedReq)

				replaceBody, _ := json.Marshal(fiber.Map{"platforms": []string{"facebook"}})
				replaceReq := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(replaceBody))
				replaceReq.Header.Set("Content-Type", "application/json")
				replaceReq.AddCookie(authCookie)
				replaceResp, err := app.Test(replaceReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(replaceResp.StatusCode).To(Equal(200))

				var got map[string]any
				Expect(json.NewDecoder(replaceResp.Body).Decode(&got)).To(Succeed())
				Expect(got["platforms"]).To(ConsistOf("facebook"))
			})

			It("is idempotent for repeated identical PUTs", func() {
				body, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin", "twitter"}})

				req1 := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(body))
				req1.Header.Set("Content-Type", "application/json")
				req1.AddCookie(authCookie)
				resp1, err := app.Test(req1)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp1.StatusCode).To(Equal(200))

				req2 := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(body))
				req2.Header.Set("Content-Type", "application/json")
				req2.AddCookie(authCookie)
				resp2, err := app.Test(req2)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp2.StatusCode).To(Equal(200))

				var got map[string]any
				Expect(json.NewDecoder(resp2.Body).Decode(&got)).To(Succeed())
				Expect(got["platforms"]).To(ConsistOf("linkedin", "twitter"))
			})

			It("deduplicates repeated platforms in the request body", func() {
				body, _ := json.Marshal(fiber.Map{"platforms": []string{"linkedin", "linkedin"}})
				req := httptest.NewRequest("PUT", "/api/auto-publish-allowlist", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got["platforms"]).To(ConsistOf("linkedin"))
			})
		})
	})
})
