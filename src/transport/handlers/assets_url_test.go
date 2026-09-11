package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
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

// urlEnqueueCall captures one EnqueueProcessURLTx invocation.
type urlEnqueueCall struct {
	assetID, tenantID, sourceURL string
	refresh                      bool
}

// fakeURLEnqueuer records EnqueueProcessURLTx calls so the /url test can assert
// scraping was enqueued (in the insert's tx) without a real River job table.
type fakeURLEnqueuer struct {
	calls []urlEnqueueCall
}

func (f *fakeURLEnqueuer) EnqueueProcessURLTx(_ context.Context, _ *sql.Tx, assetID, tenantID, sourceURL string, refresh bool) error {
	f.calls = append(f.calls, urlEnqueueCall{assetID, tenantID, sourceURL, refresh})
	return nil
}

// fakeScrapeGate stubs URL-scrape key presence; has is flipped per spec.
type fakeScrapeGate struct{ has bool }

func (g *fakeScrapeGate) HasKey(_ context.Context) bool { return g.has }

var _ = Describe("AssetsHandler POST /url", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		enq        *fakeURLEnqueuer
		gate       *fakeScrapeGate
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
		tagRepo := repository.NewTagRepository(db)
		pieceRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		enq = &fakeURLEnqueuer{}
		gate = &fakeScrapeGate{has: true}
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewAssetsHandler(
			pieceRepo, repository.NewAssetFileRepository(db), repository.NewAssetImageRepository(db),
			nil, db, nil, enq, gate, nil, nil, auth, nil,
		).Register(app)

		seedTenantUser(db, "Admin", "url@example.com", "admin-password")
		loginBody, _ := json.Marshal(fiber.Map{"email": "url@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]
	})

	AfterEach(func() {
		for _, tbl := range []string{"asset_images", "assets_chunks", "assets", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	post := func(url string) *http.Response {
		body, _ := json.Marshal(fiber.Map{"url": url})
		req := httptest.NewRequest("POST", "/api/content-bank/assets/url", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	It("returns 401 when unauthenticated", func() {
		body, _ := json.Marshal(fiber.Map{"url": "https://example.com/a"})
		req := httptest.NewRequest("POST", "/api/content-bank/assets/url", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(401))
	})

	It("returns 409 when scraping is not configured", func() {
		gate.has = false
		// Literal public IP: the handler's SSRF resolve-check short-circuits on a
		// literal IP, so the test needs no DNS resolution.
		resp := post("https://8.8.8.8/a")
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		Expect(enq.calls).To(BeEmpty())
	})

	It("returns 400 for a non-http(s) URL", func() {
		resp := post("ftp://example.com/a")
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("creates a pending URL asset and enqueues a scrape (201)", func() {
		// Literal public IP so the SSRF resolve-check short-circuits (no DNS).
		resp := post("https://8.8.8.8/Path/")
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

		var a models.Asset
		Expect(json.NewDecoder(resp.Body).Decode(&a)).To(Succeed())
		Expect(a.ID).NotTo(BeEmpty())
		Expect(a.Status).To(Equal(models.AssetStatusPending))
		Expect(a.Type).NotTo(BeNil())
		Expect(*a.Type).To(Equal(models.AssetTypeURL))
		// Trailing slash trimmed, path case preserved.
		Expect(a.SourceURL).NotTo(BeNil())
		Expect(*a.SourceURL).To(Equal("https://8.8.8.8/Path"))

		Expect(enq.calls).To(HaveLen(1))
		Expect(enq.calls[0].assetID).To(Equal(a.ID))
		Expect(enq.calls[0].sourceURL).To(Equal("https://8.8.8.8/Path"))
		Expect(enq.calls[0].refresh).To(BeFalse())
	})

	It("refreshes in place on a duplicate URL (200, same id, refresh=true)", func() {
		first := post("https://8.8.8.8/dup")
		Expect(first.StatusCode).To(Equal(fiber.StatusCreated))
		var a1 models.Asset
		Expect(json.NewDecoder(first.Body).Decode(&a1)).To(Succeed())

		second := post("https://8.8.8.8/dup")
		Expect(second.StatusCode).To(Equal(fiber.StatusOK))
		var a2 models.Asset
		Expect(json.NewDecoder(second.Body).Decode(&a2)).To(Succeed())
		Expect(a2.ID).To(Equal(a1.ID), "duplicate URL must refresh the same asset")

		// Exactly one asset row exists for this URL.
		n, err := db.NewSelect().Model((*models.Asset)(nil)).
			Where("source_url = ?", "https://8.8.8.8/dup").Count(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(1))

		Expect(enq.calls).To(HaveLen(2))
		Expect(enq.calls[0].refresh).To(BeFalse())
		Expect(enq.calls[1].refresh).To(BeTrue())
	})
})
