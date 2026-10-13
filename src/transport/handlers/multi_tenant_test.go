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
	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// Proves the central tenant-scoping (the TenantScoped model hooks) actually
// isolates tenants end-to-end: one tenant can never read, update, delete, or
// even see the existence of another tenant's rows. Because scoping is enforced
// by a single shared mixin, campaigns + tags here are representative of every
// tenant-owned entity.
var _ = Describe("Multi-tenant isolation (CON-97)", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		chunksRepo repository.AssetChunksRepository
	)

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

	BeforeEach(func() {
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		userRepo := repository.NewUserRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tagRepo := repository.NewTagRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewTenantsHandler(signup.New(db, repository.NewAccountRepository(db), tenantRepo, nil), tenantRepo, testCookieName, false, auth, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		handlers.NewTagsHandler(tagRepo, auth).Register(app)
		chunksRepo = repository.NewAssetChunksRepository(db)
		assetFileRepo := repository.NewAssetFileRepository(db)
		handlers.NewAssetsHandler(repository.NewAssetRepository(db, tagRepo, assetFileRepo), assetFileRepo, repository.NewAssetImageRepository(db), nil, nil, nil, nil, nil, nil, nil, auth, nil, handlers.AssetsOptions{}).Register(app)
	})

	AfterEach(func() {
		ctx := tenantCtx() // TableExpr deletes bypass hooks; tenant value is ignored
		for _, t := range []string{"assets_chunks", "assets", "campaigns", "tags", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(t).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(ctx)
	})

	// signup creates a fresh tenant + owner and returns the auth cookie.
	signup := func(tenantName, email string) *http.Cookie {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{
			"tenant": fiber.Map{"name": tenantName},
			"user":   fiber.Map{"name": "Owner", "email": email, "password": "password-owner"},
		})
		req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				return ck
			}
		}
		Fail("signup did not set a session cookie")
		return nil
	}

	post := func(cookie *http.Cookie, path string, payload fiber.Map) *http.Response {
		GinkgoHelper()
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	createCampaign := func(cookie *http.Cookie, name string) string {
		GinkgoHelper()
		resp := post(cookie, "/api/campaigns", fiber.Map{"name": name, "campaign_type_id": "Uk"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var c models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
		return c.ID
	}

	createTag := func(cookie *http.Cookie, name string) string {
		GinkgoHelper()
		resp := post(cookie, "/api/tags", fiber.Map{"name": name})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var t models.Tag
		Expect(json.NewDecoder(resp.Body).Decode(&t)).To(Succeed())
		return t.ID
	}

	withCookie := func(method, path string, cookie *http.Cookie, payload fiber.Map) *http.Response {
		GinkgoHelper()
		var r *bytes.Reader
		if payload != nil {
			body, _ := json.Marshal(payload)
			r = bytes.NewReader(body)
		} else {
			r = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	It("scopes list endpoints to the caller's tenant", func() {
		a := signup("Acme", "a@acme.test")
		b := signup("Beta", "b@beta.test")
		createCampaign(a, "Acme Campaign")
		createCampaign(a, "Acme Campaign 2")
		createCampaign(b, "Beta Campaign")

		resp := withCookie("GET", "/api/campaigns", a, nil)
		Expect(resp.StatusCode).To(Equal(200))
		var aCamps []models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&aCamps)).To(Succeed())
		Expect(aCamps).To(HaveLen(2))

		resp = withCookie("GET", "/api/campaigns", b, nil)
		var bCamps []models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&bCamps)).To(Succeed())
		Expect(bCamps).To(HaveLen(1))
		Expect(bCamps[0].Name).To(Equal("Beta Campaign"))
	})

	It("returns 404 reading another tenant's campaign", func() {
		a := signup("Acme", "a@acme.test")
		b := signup("Beta", "b@beta.test")
		aID := createCampaign(a, "Acme Campaign")

		Expect(withCookie("GET", "/api/campaigns/"+aID, b, nil).StatusCode).To(Equal(404))
		// owner can still read it
		Expect(withCookie("GET", "/api/campaigns/"+aID, a, nil).StatusCode).To(Equal(200))
	})

	It("returns 404 updating another tenant's campaign and leaves it unchanged", func() {
		a := signup("Acme", "a@acme.test")
		b := signup("Beta", "b@beta.test")
		aID := createCampaign(a, "Acme Campaign")

		Expect(withCookie("PUT", "/api/campaigns/"+aID, b,
			fiber.Map{"name": "Hacked", "campaign_type_id": "Uk"}).StatusCode).To(Equal(404))

		resp := withCookie("GET", "/api/campaigns/"+aID, a, nil)
		var c models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
		Expect(c.Name).To(Equal("Acme Campaign"))
	})

	It("lists, counts and searches only the caller's assets", func() {
		a := signup("Acme", "a@acme.test")
		b := signup("Beta", "b@beta.test")
		createTag(a, "Acme tag")
		createTag(b, "Beta tag")
		// seed stores an asset whose one chunk mentions "overwintering" under the
		// named tenant.
		seed := func(tenantName, assetID string) {
			GinkgoHelper()
			sys := tenantctx.WithSystem(context.Background())
			var tenant models.Tenant
			Expect(db.NewSelect().Model(&tenant).Where("name = ?", tenantName).Scan(sys)).To(Succeed())
			var owner models.User
			Expect(db.NewSelect().Model(&owner).Where("tenant_id = ?", tenant.ID).Limit(1).Scan(sys)).To(Succeed())
			ctx := tenantctx.With(context.Background(), tenant.ID)
			_, err := db.NewInsert().Model(&models.Asset{
				ID: assetID, Title: assetID, Content: "c", Status: models.AssetStatusReady, TagIDs: models.StringSlice{}, CreatedBy: owner.ID,
			}).Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(chunksRepo.UpsertChunks(ctx, assetID, []models.AssetChunk{{
				ID: assetID + ":0", AssetID: assetID, Content: tenantName + " notes on overwintering", TokenCount: 4,
				Embedding: pgvector.NewHalfVector(make([]float32, 3072)), Model: "m",
			}})).To(Succeed())
		}
		seed("Acme", "acme-asset")
		seed("Beta", "beta-asset")

		for cookie, want := range map[*http.Cookie]string{a: "acme-asset", b: "beta-asset"} {
			for _, query := range []string{"", "?text=overwintering&facets=tag,campaign,kind", "?ids_only=true"} {
				resp := withCookie("GET", "/api/content-bank/assets"+query, cookie, nil)
				Expect(resp.StatusCode).To(Equal(200))
				var body struct {
					Assets []struct {
						ID string `json:"id"`
					} `json:"assets"`
					IDs             []string                            `json:"ids"`
					TotalUnfiltered int                                 `json:"total_unfiltered"`
					Facets          map[string][]models.AssetFacetCount `json:"facets"`
				}
				Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
				if query == "?ids_only=true" {
					Expect(body.IDs).To(Equal([]string{want}))
					continue
				}
				Expect(body.Assets).To(HaveLen(1))
				Expect(body.Assets[0].ID).To(Equal(want))
				Expect(body.TotalUnfiltered).To(Equal(1))
				if tags := body.Facets["tag"]; tags != nil {
					Expect(tags).To(HaveLen(2)) // the caller's one tag, then none
				}
			}
		}
	})

	It("isolates tags across tenants", func() {
		a := signup("Acme", "a@acme.test")
		b := signup("Beta", "b@beta.test")
		aTag := createTag(a, "acme-tag")
		createTag(b, "beta-tag")

		// B's tag list excludes A's tag.
		resp := withCookie("GET", "/api/tags", b, nil)
		var bTags []models.Tag
		Expect(json.NewDecoder(resp.Body).Decode(&bTags)).To(Succeed())
		Expect(bTags).To(HaveLen(1))
		Expect(bTags[0].Name).To(Equal("beta-tag"))

		// B cannot delete A's tag (404), and it survives.
		Expect(withCookie("DELETE", "/api/tags/"+aTag, b, nil).StatusCode).To(Equal(404))
		resp = withCookie("GET", "/api/tags", a, nil)
		var aTags []models.Tag
		Expect(json.NewDecoder(resp.Body).Decode(&aTags)).To(Succeed())
		Expect(aTags).To(HaveLen(1))
	})
})
