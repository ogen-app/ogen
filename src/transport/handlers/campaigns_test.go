package handlers_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/summaries"
)

var _ = Describe("CampaignsHandler", Ordered, func() {
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
		tagRepo := repository.NewTagRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewCampaignTypesHandler(campaignTypeRepo, auth).Register(app)
		// CON-291: overview + summaries live on the read handler. Nil deps here so
		// the routes exist (401 unauthenticated, 503 authenticated) for the default-
		// app route-level specs; registered before CampaignsHandler so /summaries
		// wins over /:id.
		handlers.NewCampaignReadHandler(nil, nil, auth).Register(app)
		// CON-291: generate-posts + brief/posts-review live on the generation
		// handler. Nil deps so the routes exist (401 unauthenticated) for the
		// default-app route-level specs.
		handlers.NewCampaignGenerationHandler(campaignRepo, nil, 0, nil, nil, nil, nil, auth).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(app)
		handlers.NewTagsHandler(tagRepo, auth).Register(app)

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
		_, err := db.NewDelete().TableExpr("campaigns").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("tags").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// helper: create a campaign via the API and return it
	createCampaign := func(name string, campaignTypeID string) models.Campaign {
		body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": campaignTypeID})
		req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var c models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
		return c
	}

	// ── List ─────────────────────────────────────────────────────────────────

	Describe("GET /api/campaigns", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaigns", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns an empty list when no campaigns exist", func() {
				req := httptest.NewRequest("GET", "/api/campaigns", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var campaigns []models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&campaigns)).To(Succeed())
				Expect(campaigns).To(BeEmpty())
			})

			It("returns all campaigns", func() {
				createCampaign("Summer Push", "Uk")
				createCampaign("Q4 Retargeting", "Ef")

				req := httptest.NewRequest("GET", "/api/campaigns", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var campaigns []models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&campaigns)).To(Succeed())
				Expect(campaigns).To(HaveLen(2))
			})
		})
	})

	// ── Create ───────────────────────────────────────────────────────────────

	Describe("POST /api/campaigns", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"name": "Test", "campaign_type_id": "Uk"})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("creates a campaign with required fields and returns 201", func() {
				body, _ := json.Marshal(fiber.Map{"name": "Launch Campaign", "campaign_type_id": "gb"})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var c models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
				Expect(c.ID).NotTo(BeEmpty())
				Expect(c.Name).To(Equal("Launch Campaign"))
				Expect(c.CampaignTypeID).To(Equal("gb"))
				// CON-156 BE 6: campaigns are created active, not draft.
				Expect(c.Status).To(Equal(models.StatusActive))
				Expect(c.CreatedBy).NotTo(BeEmpty())
				Expect(c.AssetIDs).To(BeEmpty())
				Expect(c.TargetPlatforms).To(BeEmpty())
				Expect(c.Tags).To(BeEmpty())
			})

			It("creates a campaign with all optional fields", func() {
				count := 20
				body, _ := json.Marshal(fiber.Map{
					"name":             "Full Campaign",
					"campaign_type_id": "Ef",
					"description":      "Drive sign-ups",
					"target_persona":   "Tech-savvy millennials",
					"key_messages":     "Fast, reliable, affordable",
					"tone_guidelines":  "Friendly and professional",
					"use_assets":       true,
					"asset_ids":        []string{"abc", "def"},
					"target_platforms": []fiber.Map{
						{"id": "rzgpTkARLH0L", "post_types": []string{"image-post", "reel"}},
						{"id": "tiktok", "post_types": []string{"video"}},
					},
					"status":               "scheduled",
					"estimated_post_count": count,
					"budget":               1500.00,
					"currency":             "USD",
					"language":             "en",
					"tag_ids":              []string{},
				})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var c models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
				Expect(c.Status).To(Equal(models.StatusScheduled))
				Expect(c.UseAssets).To(BeTrue())
				Expect(c.AssetIDs).To(ConsistOf("abc", "def"))
				Expect(c.TagIDs).To(BeEmpty())
				Expect(c.EstimatedPostCount).NotTo(BeNil())
				Expect(*c.EstimatedPostCount).To(Equal(count))
				Expect(c.Budget).NotTo(BeNil())
				Expect(*c.Budget).To(BeNumerically("~", 1500.00, 0.01))
				Expect(c.Currency).To(Equal("USD"))
				Expect(c.Language).To(Equal("en"))
			})

			It("stores nil estimated_post_count when omitted", func() {
				body, _ := json.Marshal(fiber.Map{"name": "No Count", "campaign_type_id": "Uk"})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var c models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
				Expect(c.EstimatedPostCount).To(BeNil())
			})

			It("returns 400 when name is missing", func() {
				body, _ := json.Marshal(fiber.Map{"campaign_type_id": "Uk"})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when campaign_type_id is missing", func() {
				body, _ := json.Marshal(fiber.Map{"name": "No Type"})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when campaign_type_id is invalid", func() {
				body, _ := json.Marshal(fiber.Map{"name": "Bad Type", "campaign_type_id": "nonexistent"})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when status is invalid", func() {
				body, _ := json.Marshal(fiber.Map{"name": "Bad Status", "campaign_type_id": "Uk", "status": "unknown"})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Get ──────────────────────────────────────────────────────────────────

	Describe("GET /api/campaigns/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaigns/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the campaign with hydrated campaign_type and phases", func() {
				c := createCampaign("Brand Awareness", "Uk")

				req := httptest.NewRequest("GET", "/api/campaigns/"+c.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.ID).To(Equal(c.ID))
				Expect(got.Name).To(Equal("Brand Awareness"))
				Expect(got.CampaignType).NotTo(BeNil())
				Expect(got.CampaignType.Name).To(Equal("awareness"))
				Expect(got.CampaignType.Phases).To(HaveLen(2))
				Expect(got.CampaignType.Phases[0].Name).To(Equal("Launch & Distribution"))
			})

			It("returns hydrated platforms for known target_platforms", func() {
				body, _ := json.Marshal(fiber.Map{
					"name":             "Platform Hydration Test",
					"campaign_type_id": "Uk",
					"target_platforms": []fiber.Map{
						{"id": "rzgpTkARLH0L", "post_types": []string{"image-post"}},
						{"id": "tiktok", "post_types": []string{"video"}},
					},
				})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, _ := app.Test(req)
				var created models.Campaign
				json.NewDecoder(resp.Body).Decode(&created)

				req = httptest.NewRequest("GET", "/api/campaigns/"+created.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				// rzgpTkARLH0L is the seeded Instagram platform; "tiktok" has no match
				Expect(got.Platforms).To(HaveLen(1))
				Expect(got.Platforms[0].ID).To(Equal("rzgpTkARLH0L"))
				Expect(got.Platforms[0].Name).To(Equal("Instagram"))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("GET", "/api/campaigns/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Update ───────────────────────────────────────────────────────────────

	Describe("PUT /api/campaigns/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"name": "Updated", "campaign_type_id": "Vq"})
				req := httptest.NewRequest("PUT", "/api/campaigns/someid", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("updates the campaign and returns the updated resource", func() {
				c := createCampaign("Original Name", "Uk")

				count := 15
				body, _ := json.Marshal(fiber.Map{
					"name":                 "Updated Name",
					"campaign_type_id":     "Vq",
					"status":               "active",
					"language":             "fr",
					"estimated_post_count": count,
				})
				req := httptest.NewRequest("PUT", "/api/campaigns/"+c.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Name).To(Equal("Updated Name"))
				Expect(got.CampaignTypeID).To(Equal("Vq"))
				Expect(got.Status).To(Equal(models.StatusActive))
				Expect(got.Language).To(Equal("fr"))
				Expect(got.EstimatedPostCount).NotTo(BeNil())
				Expect(*got.EstimatedPostCount).To(Equal(count))
			})

			It("returns 404 for an unknown id", func() {
				body, _ := json.Marshal(fiber.Map{"name": "Ghost", "campaign_type_id": "Uk"})
				req := httptest.NewRequest("PUT", "/api/campaigns/nonexistent", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("returns 400 when name is missing", func() {
				c := createCampaign("Has Name", "gb")

				body, _ := json.Marshal(fiber.Map{"campaign_type_id": "Uk"})
				req := httptest.NewRequest("PUT", "/api/campaigns/"+c.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when campaign_type_id is invalid", func() {
				c := createCampaign("Valid", "gb")

				body, _ := json.Marshal(fiber.Map{"name": "Valid", "campaign_type_id": "nonexistent"})
				req := httptest.NewRequest("PUT", "/api/campaigns/"+c.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Asset membership (CON-233) ───────────────────────────────────────────

	Describe("asset membership on /api/campaigns/:id/assets", func() {
		// addAssets POSTs a membership add and returns the raw response.
		addAssets := func(id string, assetIDs []string) *http.Response {
			body, _ := json.Marshal(fiber.Map{"asset_ids": assetIDs})
			req := httptest.NewRequest("POST", "/api/campaigns/"+id+"/assets", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}
		removeAsset := func(id, assetID string) *http.Response {
			req := httptest.NewRequest("DELETE", "/api/campaigns/"+id+"/assets/"+assetID, nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}

		Context("when not authenticated", func() {
			It("returns 401 for add", func() {
				body, _ := json.Marshal(fiber.Map{"asset_ids": []string{"a"}})
				req := httptest.NewRequest("POST", "/api/campaigns/x/assets", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
			It("returns 401 for remove", func() {
				req := httptest.NewRequest("DELETE", "/api/campaigns/x/assets/a", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("adds ids to an empty set and returns the updated campaign", func() {
				c := createCampaign("Bank Campaign", "Uk")
				resp := addAssets(c.ID, []string{"asset-a", "asset-b"})
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.AssetIDs)).To(Equal([]string{"asset-a", "asset-b"}))
			})

			It("unions without duplicating and preserves existing order", func() {
				c := createCampaign("Union Campaign", "Uk")
				Expect(addAssets(c.ID, []string{"a", "b"}).StatusCode).To(Equal(200))
				// Re-add an existing id plus a new one; only the new one appends.
				resp := addAssets(c.ID, []string{"b", "c"})
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.AssetIDs)).To(Equal([]string{"a", "b", "c"}))
			})

			It("leaves omitted fields untouched — the whole point of CON-233", func() {
				// Create a campaign with non-default scheduling + meta, so a naive
				// full-record write would reset them.
				body, _ := json.Marshal(fiber.Map{
					"name":             "Keep My Fields",
					"campaign_type_id": "Uk",
					"status":           "active",
					"language":         "fr",
					"publishing_days":  []string{"mon", "tue"},
					"publishing_time":  "07:30",
					"spread_minutes":   5,
				})
				req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
				var created models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&created)).To(Succeed())

				addResp := addAssets(created.ID, []string{"asset-a"})
				Expect(addResp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(addResp.Body).Decode(&got)).To(Succeed())

				Expect([]string(got.AssetIDs)).To(Equal([]string{"asset-a"}))
				// None of these reset to defaults.
				Expect(got.Status).To(Equal(models.StatusActive))
				Expect(got.Language).To(Equal("fr"))
				Expect([]string(got.PublishingDays)).To(Equal([]string{"mon", "tue"}))
				Expect(got.PublishingTime).To(Equal("07:30"))
				Expect(got.SpreadMinutes).To(Equal(5))
			})

			It("returns 404 when adding to an unknown campaign", func() {
				Expect(addAssets("nonexistent", []string{"a"}).StatusCode).To(Equal(404))
			})

			It("removes an id and returns the updated campaign", func() {
				c := createCampaign("Remove Campaign", "Uk")
				Expect(addAssets(c.ID, []string{"a", "b", "c"}).StatusCode).To(Equal(200))
				resp := removeAsset(c.ID, "b")
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.AssetIDs)).To(Equal([]string{"a", "c"}))
			})

			It("is a no-op when removing an id that isn't present", func() {
				c := createCampaign("Idempotent Remove", "Uk")
				Expect(addAssets(c.ID, []string{"a"}).StatusCode).To(Equal(200))
				resp := removeAsset(c.ID, "does-not-exist")
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.AssetIDs)).To(Equal([]string{"a"}))
			})

			It("returns 404 when removing from an unknown campaign", func() {
				Expect(removeAsset("nonexistent", "a").StatusCode).To(Equal(404))
			})

			It("survives two concurrent adds of different ids (CON-233 acceptance)", func() {
				c := createCampaign("Concurrent Campaign", "Uk")
				var wg sync.WaitGroup
				for _, id := range []string{"concurrent-1", "concurrent-2"} {
					wg.Go(func() {
						defer GinkgoRecover()
						Expect(addAssets(c.ID, []string{id}).StatusCode).To(Equal(200))
					})
				}
				wg.Wait()

				req := httptest.NewRequest("GET", "/api/campaigns/"+c.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				// Neither add clobbered the other: both ids present, in some order.
				Expect([]string(got.AssetIDs)).To(ConsistOf("concurrent-1", "concurrent-2"))
			})

			// ── use_assets stays derived from the set (CON-233 review) ──────────
			// The FE derives use_assets purely from the membership set (CON-210
			// retired the three-mode picker), so these endpoints must keep it in
			// lockstep — otherwise generation silently ignores a first attach, or
			// a last-detach flips the campaign to whole-workspace mode.
			It("turns use_assets on when the first document is attached", func() {
				c := createCampaign("Brief Only", "Uk")
				Expect(c.UseAssets).To(BeFalse()) // brief-only campaign

				resp := addAssets(c.ID, []string{"asset-a"})
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.UseAssets).To(BeTrue())
				Expect([]string(got.AssetIDs)).To(Equal([]string{"asset-a"}))
			})

			It("turns use_assets off when the last document is detached", func() {
				c := createCampaign("Single Source", "Uk")
				Expect(addAssets(c.ID, []string{"only"}).StatusCode).To(Equal(200))

				resp := removeAsset(c.ID, "only")
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				// Never leaves use_assets=true over an empty list — the whole-bank trap.
				Expect(got.UseAssets).To(BeFalse())
				Expect([]string(got.AssetIDs)).To(BeEmpty())
			})

			It("keeps use_assets on while some documents remain", func() {
				c := createCampaign("Multi Source", "Uk")
				Expect(addAssets(c.ID, []string{"a", "b"}).StatusCode).To(Equal(200))

				resp := removeAsset(c.ID, "a")
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.UseAssets).To(BeTrue())
				Expect([]string(got.AssetIDs)).To(Equal([]string{"b"}))
			})

			// CON-233 #2: membership fields are presence-aware on PUT, so an
			// unrelated whole-record save (a brief edit) that omits them must not
			// clobber a set attached via the membership endpoints.
			It("preserves use_assets + asset_ids when a PUT omits them", func() {
				c := createCampaign("Brief + Bank", "Uk")
				Expect(addAssets(c.ID, []string{"a"}).StatusCode).To(Equal(200))

				body, _ := json.Marshal(fiber.Map{
					"name":             "Brief + Bank (edited)",
					"campaign_type_id": "Uk",
				})
				req := httptest.NewRequest("PUT", "/api/campaigns/"+c.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Name).To(Equal("Brief + Bank (edited)"))
				Expect(got.UseAssets).To(BeTrue())
				Expect([]string(got.AssetIDs)).To(Equal([]string{"a"}))
			})

			// A PUT that DOES restate membership still full-replaces (back-compat
			// for clients that send it): explicit values win.
			It("still replaces use_assets + asset_ids when a PUT sends them", func() {
				c := createCampaign("Explicit Bank", "Uk")
				Expect(addAssets(c.ID, []string{"a"}).StatusCode).To(Equal(200))

				body, _ := json.Marshal(fiber.Map{
					"name":             "Explicit Bank",
					"campaign_type_id": "Uk",
					"use_assets":       false,
					"asset_ids":        []string{},
				})
				req := httptest.NewRequest("PUT", "/api/campaigns/"+c.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Campaign
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.UseAssets).To(BeFalse())
				Expect([]string(got.AssetIDs)).To(BeEmpty())
			})
		})
	})

	// ── Tag hydration ────────────────────────────────────────────────────────

	Describe("tag hydration on GET /api/campaigns and /:id", func() {
		var tagID string

		BeforeEach(func() {
			tagBody, _ := json.Marshal(fiber.Map{"name": "Q4", "color": "#3498DB"})
			tagReq := httptest.NewRequest("POST", "/api/tags", bytes.NewReader(tagBody))
			tagReq.Header.Set("Content-Type", "application/json")
			tagReq.AddCookie(authCookie)
			tagResp, err := app.Test(tagReq)
			Expect(err).NotTo(HaveOccurred())
			Expect(tagResp.StatusCode).To(Equal(fiber.StatusCreated))
			var t map[string]any
			Expect(json.NewDecoder(tagResp.Body).Decode(&t)).To(Succeed())
			tagID = t["id"].(string)
		})

		It("returns tags populated on List", func() {
			body, _ := json.Marshal(fiber.Map{
				"name":             "Tagged Campaign",
				"campaign_type_id": "Uk",
				"tag_ids":          []string{tagID},
			})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

			listReq := httptest.NewRequest("GET", "/api/campaigns", nil)
			listReq.AddCookie(authCookie)
			listResp, err := app.Test(listReq)
			Expect(err).NotTo(HaveOccurred())
			Expect(listResp.StatusCode).To(Equal(200))

			var campaigns []models.Campaign
			Expect(json.NewDecoder(listResp.Body).Decode(&campaigns)).To(Succeed())
			Expect(campaigns).To(HaveLen(1))
			Expect(campaigns[0].TagIDs).To(ConsistOf(tagID))
			Expect(campaigns[0].Tags).To(HaveLen(1))
			Expect(campaigns[0].Tags[0].ID).To(Equal(tagID))
			Expect(campaigns[0].Tags[0].Name).To(Equal("Q4"))
		})

		It("returns tags populated on GetByID", func() {
			c := createCampaign("Untagged", "Uk")

			body, _ := json.Marshal(fiber.Map{
				"name":             "Untagged",
				"campaign_type_id": "Uk",
				"tag_ids":          []string{tagID},
			})
			req := httptest.NewRequest("PUT", "/api/campaigns/"+c.ID, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			_, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())

			getReq := httptest.NewRequest("GET", "/api/campaigns/"+c.ID, nil)
			getReq.AddCookie(authCookie)
			getResp, err := app.Test(getReq)
			Expect(err).NotTo(HaveOccurred())
			Expect(getResp.StatusCode).To(Equal(200))

			var got models.Campaign
			Expect(json.NewDecoder(getResp.Body).Decode(&got)).To(Succeed())
			Expect(got.Tags).To(HaveLen(1))
			Expect(got.Tags[0].Name).To(Equal("Q4"))
		})

		It("returns empty tags array when no tag_ids set", func() {
			c := createCampaign("No Tags", "gb")

			getReq := httptest.NewRequest("GET", "/api/campaigns/"+c.ID, nil)
			getReq.AddCookie(authCookie)
			getResp, err := app.Test(getReq)
			Expect(err).NotTo(HaveOccurred())

			var got models.Campaign
			Expect(json.NewDecoder(getResp.Body).Decode(&got)).To(Succeed())
			Expect(got.Tags).NotTo(BeNil())
			Expect(got.Tags).To(BeEmpty())
		})
	})

	// ── GenerateDraft ────────────────────────────────────────────────────────

	Describe("POST /api/campaigns/:id/generate-draft", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("POST", "/api/campaigns/someid/generate-draft", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when generateDraft is nil", func() {
				// The test app is wired with generateDraft=nil.
				c := createCampaign("Draft Campaign", "Uk")
				req := httptest.NewRequest("POST", "/api/campaigns/"+c.ID+"/generate-draft", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns 404 for an unknown campaign id", func() {
				// Wire a no-op generateDraft so we don't hit 503 first.
				appWithDraft := fiber.New(fiber.Config{
					ErrorHandler: func(c *fiber.Ctx, err error) error {
						code := fiber.StatusInternalServerError
						if e, ok := err.(*fiber.Error); ok {
							code = e.Code
						}
						return c.Status(code).JSON(fiber.Map{"error": err.Error()})
					},
				})
				campaignTypeRepo2 := repository.NewCampaignTypeRepository(db)
				campaignRepo2 := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), campaignTypeRepo2)
				sessionRepo := repository.NewSessionRepository(db)
				settingRepo := repository.NewSettingRepository(db)
				userRepo := repository.NewUserRepository(db)
				auth2 := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth2).Register(appWithDraft)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(appWithDraft)
				noop := func(_ context.Context, _ string, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
					return &content_plan.ContentPlanResponse{}, nil
				}
				handlers.NewCampaignsHandler(campaignRepo2, campaignTypeRepo2, auth2, noop, nil, nil, nil, nil).Register(appWithDraft)

				req := httptest.NewRequest("POST", "/api/campaigns/nonexistent/generate-draft", nil)
				req.AddCookie(authCookie)
				resp, err := appWithDraft.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("returns 403 for another user's campaign", func() {
				// Create a second user whose campaign the first user should not access.
				appWithDraft := fiber.New(fiber.Config{
					ErrorHandler: func(c *fiber.Ctx, err error) error {
						code := fiber.StatusInternalServerError
						if e, ok := err.(*fiber.Error); ok {
							code = e.Code
						}
						return c.Status(code).JSON(fiber.Map{"error": err.Error()})
					},
				})
				campaignTypeRepo2 := repository.NewCampaignTypeRepository(db)
				campaignRepo2 := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), campaignTypeRepo2)
				sessionRepo := repository.NewSessionRepository(db)
				settingRepo := repository.NewSettingRepository(db)
				userRepo := repository.NewUserRepository(db)
				auth2 := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth2).Register(appWithDraft)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(appWithDraft)
				noop := func(_ context.Context, _ string, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
					return &content_plan.ContentPlanResponse{}, nil
				}
				handlers.NewCampaignsHandler(campaignRepo2, campaignTypeRepo2, auth2, noop, nil, nil, nil, nil).Register(appWithDraft)

				// Register and log in as a second user.
				seedTenantUser(db, "Other", "other@example.com", "other-password")

				loginBody, _ := json.Marshal(fiber.Map{"email": "other@example.com", "password": "other-password"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := appWithDraft.Test(loginReq)
				Expect(err).NotTo(HaveOccurred())
				var otherCookie *http.Cookie
				for _, ck := range loginResp.Cookies() {
					otherCookie = ck
				}

				// Create a campaign as the second user.
				campBody, _ := json.Marshal(fiber.Map{"name": "Other's Campaign", "campaign_type_id": "Uk"})
				campReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(campBody))
				campReq.Header.Set("Content-Type", "application/json")
				campReq.AddCookie(otherCookie)
				campResp, err := appWithDraft.Test(campReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(campResp.StatusCode).To(Equal(fiber.StatusCreated))
				var otherCampaign models.Campaign
				Expect(json.NewDecoder(campResp.Body).Decode(&otherCampaign)).To(Succeed())

				// Try to generate draft as the first user (authCookie) — should get 403.
				draftReq := httptest.NewRequest("POST", "/api/campaigns/"+otherCampaign.ID+"/generate-draft", nil)
				draftReq.AddCookie(authCookie)
				draftResp, err := appWithDraft.Test(draftReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(draftResp.StatusCode).To(Equal(403))
			})

			It("streams step and complete SSE events on success", func() {
				appWithDraft := fiber.New(fiber.Config{
					ErrorHandler: func(c *fiber.Ctx, err error) error {
						code := fiber.StatusInternalServerError
						if e, ok := err.(*fiber.Error); ok {
							code = e.Code
						}
						return c.Status(code).JSON(fiber.Map{"error": err.Error()})
					},
				})
				campaignTypeRepo2 := repository.NewCampaignTypeRepository(db)
				campaignRepo2 := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), campaignTypeRepo2)
				sessionRepo := repository.NewSessionRepository(db)
				settingRepo := repository.NewSettingRepository(db)
				userRepo := repository.NewUserRepository(db)
				auth2 := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth2).Register(appWithDraft)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(appWithDraft)

				stub := func(_ context.Context, _ string, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
					onEvent(content_plan.SSEEventStep, content_plan.StepEventPayload{Step: "validateInput", Status: "done"})
					onEvent(content_plan.SSEEventPost, content_plan.PostEventPayload{
						Post:  content_plan.DraftPost{Title: "First Post", Body: "Body text", PlatformID: "linkedin", ContentType: "text-post", PublishDate: "2026-05-01"},
						Index: 0,
					})
					onEvent(content_plan.SSEEventStep, content_plan.StepEventPayload{Step: "generatePosts", Status: "done"})
					return &content_plan.ContentPlanResponse{CampaignID: "test"}, nil
				}
				handlers.NewCampaignsHandler(campaignRepo2, campaignTypeRepo2, auth2, stub, nil, nil, nil, nil).Register(appWithDraft)

				// Seed user/session for appWithDraft.
				seedTenantUser(db, "SSE User", "sse@example.com", "sse-password")
				loginBody, _ := json.Marshal(fiber.Map{"email": "sse@example.com", "password": "sse-password"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := appWithDraft.Test(loginReq)
				Expect(err).NotTo(HaveOccurred())
				var sseCookie *http.Cookie
				for _, ck := range loginResp.Cookies() {
					sseCookie = ck
				}

				// Create campaign.
				campBody, _ := json.Marshal(fiber.Map{"name": "SSE Campaign", "campaign_type_id": "Uk"})
				campReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(campBody))
				campReq.Header.Set("Content-Type", "application/json")
				campReq.AddCookie(sseCookie)
				campResp, err := appWithDraft.Test(campReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(campResp.StatusCode).To(Equal(fiber.StatusCreated))
				var camp models.Campaign
				Expect(json.NewDecoder(campResp.Body).Decode(&camp)).To(Succeed())

				// Call generate-draft.
				draftReq := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/generate-draft", nil)
				draftReq.AddCookie(sseCookie)
				draftResp, err := appWithDraft.Test(draftReq, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(draftResp.StatusCode).To(Equal(200))
				Expect(draftResp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				// Parse SSE lines.
				type sseEvent struct{ event, data string }
				var events []sseEvent
				scanner := bufio.NewScanner(draftResp.Body)
				var curEvent, curData string
				for scanner.Scan() {
					line := scanner.Text()
					switch {
					case strings.HasPrefix(line, "event: "):
						curEvent = strings.TrimPrefix(line, "event: ")
					case strings.HasPrefix(line, "data: "):
						curData = strings.TrimPrefix(line, "data: ")
					case line == "":
						if curEvent != "" {
							events = append(events, sseEvent{curEvent, curData})
						}
						curEvent, curData = "", ""
					}
				}

				Expect(events).To(HaveLen(4)) // 2 step + 1 post + 1 complete
				Expect(events[0].event).To(Equal("step"))
				Expect(events[1].event).To(Equal("post"))
				Expect(events[2].event).To(Equal("step"))
				Expect(events[3].event).To(Equal("complete"))

				var postPayload content_plan.PostEventPayload
				Expect(json.Unmarshal([]byte(events[1].data), &postPayload)).To(Succeed())
				Expect(postPayload.Index).To(Equal(0))
				Expect(postPayload.Post.Title).To(Equal("First Post"))

				var completePayload content_plan.ContentPlanResponse
				Expect(json.Unmarshal([]byte(events[3].data), &completePayload)).To(Succeed())
				Expect(completePayload.CampaignID).To(Equal("test"))
			})

			It("streams an error SSE event on failure", func() {
				appWithDraft := fiber.New(fiber.Config{
					ErrorHandler: func(c *fiber.Ctx, err error) error {
						code := fiber.StatusInternalServerError
						if e, ok := err.(*fiber.Error); ok {
							code = e.Code
						}
						return c.Status(code).JSON(fiber.Map{"error": err.Error()})
					},
				})
				campaignTypeRepo2 := repository.NewCampaignTypeRepository(db)
				campaignRepo2 := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), campaignTypeRepo2)
				sessionRepo := repository.NewSessionRepository(db)
				settingRepo := repository.NewSettingRepository(db)
				userRepo := repository.NewUserRepository(db)
				auth2 := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth2).Register(appWithDraft)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(appWithDraft)

				stub := func(_ context.Context, _ string, _ content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
					return nil, &content_plan.ValidationError{Msg: "missing required fields"}
				}
				handlers.NewCampaignsHandler(campaignRepo2, campaignTypeRepo2, auth2, stub, nil, nil, nil, nil).Register(appWithDraft)

				// Seed user/session.
				seedTenantUser(db, "Err User", "err@example.com", "err-password")
				loginBody, _ := json.Marshal(fiber.Map{"email": "err@example.com", "password": "err-password"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := appWithDraft.Test(loginReq)
				Expect(err).NotTo(HaveOccurred())
				var errCookie *http.Cookie
				for _, ck := range loginResp.Cookies() {
					errCookie = ck
				}

				campBody, _ := json.Marshal(fiber.Map{"name": "Err Campaign", "campaign_type_id": "Uk"})
				campReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(campBody))
				campReq.Header.Set("Content-Type", "application/json")
				campReq.AddCookie(errCookie)
				campResp, err := appWithDraft.Test(campReq)
				Expect(err).NotTo(HaveOccurred())
				var camp models.Campaign
				Expect(json.NewDecoder(campResp.Body).Decode(&camp)).To(Succeed())

				draftReq := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/generate-draft", nil)
				draftReq.AddCookie(errCookie)
				draftResp, err := appWithDraft.Test(draftReq, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(draftResp.StatusCode).To(Equal(200))

				scanner := bufio.NewScanner(draftResp.Body)
				var eventType, eventData string
				for scanner.Scan() {
					line := scanner.Text()
					if after, ok := strings.CutPrefix(line, "event: "); ok {
						eventType = after
					} else if after, ok := strings.CutPrefix(line, "data: "); ok {
						eventData = after
					}
				}
				Expect(eventType).To(Equal("error"))
				var errPayload content_plan.ErrorEventPayload
				Expect(json.Unmarshal([]byte(eventData), &errPayload)).To(Succeed())
				Expect(errPayload.Code).To(Equal(400))
				Expect(errPayload.Message).To(Equal("missing required fields"))
			})
		})
	})

	// ── EnrichBrief ──────────────────────────────────────────────────────────

	Describe("POST /api/campaigns/:id/enrich-brief", func() {
		// errorHandler matches the production fiber error shape so status
		// codes surface as the response code rather than a panic.
		errorHandler := func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}

		// buildBriefApp wires a fresh app whose campaigns handler uses the
		// given enrichBrief stub (nil exercises the 503 path).
		buildBriefApp := func(brief func(context.Context, enrich_brief.EnrichBriefRequest, enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error)) *fiber.App {
			a := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			ctRepo := repository.NewCampaignTypeRepository(db)
			cRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
			sRepo := repository.NewSessionRepository(db)
			setRepo := repository.NewSettingRepository(db)
			uRepo := repository.NewUserRepository(db)
			a2 := handlers.RequireAuth(sRepo, uRepo, testCookieName)
			handlers.NewUsersHandler(db, uRepo, repository.NewAccountRepository(db), setRepo, a2).Register(a)
			handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(a)
			handlers.NewCampaignsHandler(cRepo, ctRepo, a2, nil, nil, brief, nil, nil).Register(a)
			return a
		}

		// seedCookie registers + logs in a user on the given app and returns
		// its session cookie.
		seedCookie := func(a *fiber.App, email string) *http.Cookie {
			seedTenantUser(db, "Brief User", email, "brief-password")
			loginBody, _ := json.Marshal(fiber.Map{"email": email, "password": "brief-password"})
			loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
			loginReq.Header.Set("Content-Type", "application/json")
			loginResp, err := a.Test(loginReq)
			Expect(err).NotTo(HaveOccurred())
			var ck *http.Cookie
			for _, c := range loginResp.Cookies() {
				ck = c
			}
			return ck
		}

		// createCampaignOn creates a campaign on the given app with the given
		// cookie and returns it.
		createCampaignOn := func(a *fiber.App, ck *http.Cookie, name, typeID string) models.Campaign {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": typeID})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(ck)
			resp, err := a.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c
		}

		// parseSSE reads an event-stream body into ordered (event, data) pairs.
		type sseEvent struct{ event, data string }
		parseSSE := func(r *bufio.Scanner) []sseEvent {
			var events []sseEvent
			var curEvent, curData string
			for r.Scan() {
				line := r.Text()
				switch {
				case strings.HasPrefix(line, "event: "):
					curEvent = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					curData = strings.TrimPrefix(line, "data: ")
				case line == "":
					if curEvent != "" {
						events = append(events, sseEvent{curEvent, curData})
					}
					curEvent, curData = "", ""
				}
			}
			return events
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("POST", "/api/campaigns/someid/enrich-brief", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when enrichBrief is nil", func() {
				// The default app is wired with enrichBrief=nil.
				c := createCampaign("Brief Campaign", "Uk")
				req := httptest.NewRequest("POST", "/api/campaigns/"+c.ID+"/enrich-brief", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns 404 for an unknown campaign id", func() {
				noop := func(_ context.Context, _ enrich_brief.EnrichBriefRequest, _ enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error) {
					return &enrich_brief.EnrichBriefResponse{}, nil
				}
				a := buildBriefApp(noop)
				ck := seedCookie(a, "brief404@example.com")
				req := httptest.NewRequest("POST", "/api/campaigns/nonexistent/enrich-brief", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("streams step, per-field delta, and complete SSE events on success", func() {
				stub := func(_ context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error) {
					onEvent(enrich_brief.SSEEventStep, enrich_brief.StepEventPayload{Step: "buildContext", Status: "done"})
					onEvent(enrich_brief.SSEEventDescriptionDelta, enrich_brief.DeltaEventPayload{Delta: "A bold "})
					onEvent(enrich_brief.SSEEventDescriptionDelta, enrich_brief.DeltaEventPayload{Delta: "launch."})
					onEvent(enrich_brief.SSEEventPersonaDelta, enrich_brief.DeltaEventPayload{Delta: "Founders"})
					onEvent(enrich_brief.SSEEventStep, enrich_brief.StepEventPayload{Step: "generate", Status: "done"})
					// The handler emits the single canonical complete from the
					// returned value, so the flow does not self-emit it.
					return &enrich_brief.EnrichBriefResponse{
						Description:    "A bold launch.",
						TargetPersona:  "Founders",
						KeyMessages:    "Ship faster\nStay lean",
						ToneGuidelines: "Confident and concise",
					}, nil
				}
				a := buildBriefApp(stub)
				ck := seedCookie(a, "briefok@example.com")
				camp := createCampaignOn(a, ck, "Enrich Me", "Uk")

				body, _ := json.Marshal(fiber.Map{"instruction": "Lean, technical"})
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/enrich-brief", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				events := parseSSE(bufio.NewScanner(resp.Body))
				Expect(events).To(HaveLen(6)) // 2 step + 2 desc delta + 1 persona delta + 1 complete
				Expect(events[0].event).To(Equal("step"))
				Expect(events[1].event).To(Equal("description_delta"))
				Expect(events[2].event).To(Equal("description_delta"))
				Expect(events[3].event).To(Equal("persona_delta"))
				Expect(events[4].event).To(Equal("step"))
				Expect(events[5].event).To(Equal("complete"))

				var completePayload enrich_brief.EnrichBriefResponse
				Expect(json.Unmarshal([]byte(events[5].data), &completePayload)).To(Succeed())
				Expect(completePayload.Description).To(Equal("A bold launch."))
				Expect(completePayload.TargetPersona).To(Equal("Founders"))
				Expect(completePayload.KeyMessages).To(Equal("Ship faster\nStay lean"))
				Expect(completePayload.ToneGuidelines).To(Equal("Confident and concise"))
			})

			It("streams an error SSE event with code 400 on a validation error", func() {
				stub := func(_ context.Context, _ enrich_brief.EnrichBriefRequest, _ enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error) {
					return nil, &enrich_brief.ValidationError{Msg: "campaign type is required to enrich the brief"}
				}
				a := buildBriefApp(stub)
				ck := seedCookie(a, "brieferr@example.com")
				camp := createCampaignOn(a, ck, "No Type Brief", "Uk")

				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/enrich-brief", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				events := parseSSE(bufio.NewScanner(resp.Body))
				Expect(events).To(HaveLen(1))
				Expect(events[0].event).To(Equal("error"))
				var errPayload enrich_brief.ErrorEventPayload
				Expect(json.Unmarshal([]byte(events[0].data), &errPayload)).To(Succeed())
				Expect(errPayload.Code).To(Equal(400))
				Expect(errPayload.Message).To(Equal("campaign type is required to enrich the brief"))
			})
		})
	})

	// ── Campaign Assistant (CON-112) ───────────────────────────────────────────

	Describe("POST /api/campaigns/:id/assistant", func() {
		errorHandler := func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}

		// buildAssistantApp wires a fresh app whose campaigns handler uses the
		// given assistant stub (nil exercises the 503 path).
		buildAssistantApp := func(assistant func(context.Context, campaign_assistant.CampaignAssistantRequest, campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error)) *fiber.App {
			a := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			ctRepo := repository.NewCampaignTypeRepository(db)
			cRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
			sRepo := repository.NewSessionRepository(db)
			setRepo := repository.NewSettingRepository(db)
			uRepo := repository.NewUserRepository(db)
			msgRepo := repository.NewCampaignAssistantMessageRepository(db)
			a2 := handlers.RequireAuth(sRepo, uRepo, testCookieName)
			handlers.NewUsersHandler(db, uRepo, repository.NewAccountRepository(db), setRepo, a2).Register(a)
			handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(a)
			handlers.NewCampaignsHandler(cRepo, ctRepo, a2, nil, nil, nil, msgRepo, assistant).Register(a)
			return a
		}

		seedCookie := func(a *fiber.App, email string) *http.Cookie {
			seedTenantUser(db, "Assistant User", email, "assistant-password")
			loginBody, _ := json.Marshal(fiber.Map{"email": email, "password": "assistant-password"})
			loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
			loginReq.Header.Set("Content-Type", "application/json")
			loginResp, err := a.Test(loginReq)
			Expect(err).NotTo(HaveOccurred())
			var ck *http.Cookie
			for _, c := range loginResp.Cookies() {
				ck = c
			}
			return ck
		}

		createCampaignOn := func(a *fiber.App, ck *http.Cookie, name, typeID string) models.Campaign {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": typeID})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(ck)
			resp, err := a.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c
		}

		type sseEvent struct{ event, data string }
		parseSSE := func(r *bufio.Scanner) []sseEvent {
			var events []sseEvent
			var curEvent, curData string
			for r.Scan() {
				line := r.Text()
				switch {
				case strings.HasPrefix(line, "event: "):
					curEvent = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					curData = strings.TrimPrefix(line, "data: ")
				case line == "":
					if curEvent != "" {
						events = append(events, sseEvent{curEvent, curData})
					}
					curEvent, curData = "", ""
				}
			}
			return events
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("POST", "/api/campaigns/someid/assistant", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when the assistant is nil", func() {
				// The default app is wired with assistant=nil.
				c := createCampaign("Assistant Campaign", "Uk")
				body, _ := json.Marshal(fiber.Map{"instruction": "hi"})
				req := httptest.NewRequest("POST", "/api/campaigns/"+c.ID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns 400 when the instruction is missing", func() {
				noop := func(_ context.Context, _ campaign_assistant.CampaignAssistantRequest, _ campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
					return &campaign_assistant.CampaignAssistantResponse{}, nil
				}
				a := buildAssistantApp(noop)
				ck := seedCookie(a, "assist400@example.com")
				camp := createCampaignOn(a, ck, "Needs Instruction", "Uk")

				body, _ := json.Marshal(fiber.Map{}) // no instruction
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("streams explanation_delta then complete on success", func() {
				final := &campaign_assistant.CampaignAssistantResponse{
					Explanation: "Here's a summary of the brief.",
					Action:      "answered",
				}
				stub := func(_ context.Context, _ campaign_assistant.CampaignAssistantRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
					onEvent(campaign_assistant.SSEEventExplanationDelta, campaign_assistant.DeltaEventPayload{Delta: "Here's "})
					onEvent(campaign_assistant.SSEEventExplanationDelta, campaign_assistant.DeltaEventPayload{Delta: "a summary."})
					onEvent(campaign_assistant.SSEEventComplete, final)
					return final, nil
				}
				a := buildAssistantApp(stub)
				ck := seedCookie(a, "assistok@example.com")
				camp := createCampaignOn(a, ck, "Ask Me", "Uk")

				body, _ := json.Marshal(fiber.Map{"instruction": "summarise the brief"})
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				events := parseSSE(bufio.NewScanner(resp.Body))
				Expect(events).To(HaveLen(3))
				Expect(events[0].event).To(Equal("explanation_delta"))
				Expect(events[1].event).To(Equal("explanation_delta"))
				Expect(events[2].event).To(Equal("complete"))

				var completePayload campaign_assistant.CampaignAssistantResponse
				Expect(json.Unmarshal([]byte(events[2].data), &completePayload)).To(Succeed())
				Expect(completePayload.Action).To(Equal("answered"))
				Expect(completePayload.Explanation).To(Equal("Here's a summary of the brief."))
			})

			It("forwards namespaced sub-flow events when a tool runs", func() {
				final := &campaign_assistant.CampaignAssistantResponse{
					Explanation: "Generated a content plan.",
					Action:      "content_plan_generated",
					ContentPlan: &campaign_assistant.ContentPlanResult{PostCount: 3},
				}
				stub := func(_ context.Context, _ campaign_assistant.CampaignAssistantRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
					onEvent(campaign_assistant.SSEEventContentPlanStarted, campaign_assistant.ContentPlanStartedEventPayload{})
					onEvent(campaign_assistant.SSEEventContentPlanPost, map[string]any{"index": 0})
					onEvent(campaign_assistant.SSEEventContentPlanComplete, campaign_assistant.ContentPlanCompleteEventPayload{PostCount: 3})
					onEvent(campaign_assistant.SSEEventComplete, final)
					return final, nil
				}
				a := buildAssistantApp(stub)
				ck := seedCookie(a, "assisttool@example.com")
				camp := createCampaignOn(a, ck, "Plan Me", "Uk")

				body, _ := json.Marshal(fiber.Map{"instruction": "generate a content plan"})
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				events := parseSSE(bufio.NewScanner(resp.Body))
				names := make([]string, len(events))
				for i, e := range events {
					names[i] = e.event
				}
				Expect(names).To(Equal([]string{
					"content_plan_started",
					"content_plan_post",
					"content_plan_complete",
					"complete",
				}))
			})

			It("streams an error SSE event with code 502 on an AI error", func() {
				stub := func(_ context.Context, _ campaign_assistant.CampaignAssistantRequest, _ campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
					return nil, &campaign_assistant.AIError{Msg: "model call failed"}
				}
				a := buildAssistantApp(stub)
				ck := seedCookie(a, "assisterr@example.com")
				camp := createCampaignOn(a, ck, "Boom", "Uk")

				body, _ := json.Marshal(fiber.Map{"instruction": "do something"})
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				events := parseSSE(bufio.NewScanner(resp.Body))
				Expect(events).To(HaveLen(1))
				Expect(events[0].event).To(Equal("error"))
				var errPayload campaign_assistant.ErrorEventPayload
				Expect(json.Unmarshal([]byte(events[0].data), &errPayload)).To(Succeed())
				Expect(errPayload.Code).To(Equal(502))
				Expect(errPayload.Message).To(Equal("model call failed"))
			})
		})
	})

	Describe("GET /api/campaigns/:id/messages", func() {
		errorHandler := func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}

		buildMessagesApp := func() (*fiber.App, repository.CampaignAssistantMessageRepository) {
			a := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			ctRepo := repository.NewCampaignTypeRepository(db)
			cRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
			sRepo := repository.NewSessionRepository(db)
			setRepo := repository.NewSettingRepository(db)
			uRepo := repository.NewUserRepository(db)
			msgRepo := repository.NewCampaignAssistantMessageRepository(db)
			a2 := handlers.RequireAuth(sRepo, uRepo, testCookieName)
			handlers.NewUsersHandler(db, uRepo, repository.NewAccountRepository(db), setRepo, a2).Register(a)
			handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(a)
			handlers.NewCampaignsHandler(cRepo, ctRepo, a2, nil, nil, nil, msgRepo, nil).Register(a)
			return a, msgRepo
		}

		seedCookie := func(a *fiber.App, email string) *http.Cookie {
			seedTenantUser(db, "Messages User", email, "messages-password")
			loginBody, _ := json.Marshal(fiber.Map{"email": email, "password": "messages-password"})
			loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
			loginReq.Header.Set("Content-Type", "application/json")
			loginResp, err := a.Test(loginReq)
			Expect(err).NotTo(HaveOccurred())
			var ck *http.Cookie
			for _, c := range loginResp.Cookies() {
				ck = c
			}
			return ck
		}

		createCampaignOn := func(a *fiber.App, ck *http.Cookie, name, typeID string) models.Campaign {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": typeID})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(ck)
			resp, err := a.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaigns/someid/messages", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns an empty array when there are no messages", func() {
				a, _ := buildMessagesApp()
				ck := seedCookie(a, "msgempty@example.com")
				req := httptest.NewRequest("GET", "/api/campaigns/whatever/messages", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				// Must be a JSON array, not null (decoding null also yields an
				// empty slice, so assert the raw body to guard the contract).
				body, err := io.ReadAll(resp.Body)
				Expect(err).NotTo(HaveOccurred())
				Expect(strings.TrimSpace(string(body))).To(Equal("[]"))
				var msgs []models.CampaignAssistantMessage
				Expect(json.Unmarshal(body, &msgs)).To(Succeed())
				Expect(msgs).To(BeEmpty())
			})

			It("returns the campaign's messages oldest-first", func() {
				a, msgRepo := buildMessagesApp()
				ck := seedCookie(a, "msgok@example.com")
				camp := createCampaignOn(a, ck, "Msgs Campaign", "Uk")

				// Persist a user + model turn directly, in the default tenant
				// that the seeded session carries.
				tctx := tenantctx.With(context.Background(), models.DefaultTenantID)
				base := time.Now().UTC().Truncate(time.Second)
				uID, _ := models.NewID()
				Expect(msgRepo.Create(tctx, &models.CampaignAssistantMessage{
					ID: uID, CampaignID: camp.ID, Role: "user", Content: "generate a plan", CreatedAt: base,
				})).To(Succeed())
				mID, _ := models.NewID()
				Expect(msgRepo.Create(tctx, &models.CampaignAssistantMessage{
					ID: mID, CampaignID: camp.ID, Role: "model", Content: `{"action":"content_plan_generated"}`, CreatedAt: base.Add(time.Second),
				})).To(Succeed())

				req := httptest.NewRequest("GET", "/api/campaigns/"+camp.ID+"/messages", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				var msgs []models.CampaignAssistantMessage
				Expect(json.NewDecoder(resp.Body).Decode(&msgs)).To(Succeed())
				Expect(msgs).To(HaveLen(2))
				Expect(msgs[0].Role).To(Equal("user"))
				Expect(msgs[0].Content).To(Equal("generate a plan"))
				Expect(msgs[1].Role).To(Equal("model"))
			})
		})
	})

	// ── Campaign overview (CON-113) ────────────────────────────────────────────

	Describe("GET /api/campaigns/:id/overview", func() {
		errorHandler := func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}

		// buildOverviewApp wires an app whose campaigns handler has the overview
		// service set, and returns a post repository for seeding.
		buildOverviewApp := func() (*fiber.App, repository.PostRepository) {
			a := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			ctRepo := repository.NewCampaignTypeRepository(db)
			cRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
			pRepo := repository.NewPlatformRepository(db)
			postRepo := repository.NewPostRepository(db)
			sRepo := repository.NewSessionRepository(db)
			setRepo := repository.NewSettingRepository(db)
			uRepo := repository.NewUserRepository(db)
			a2 := handlers.RequireAuth(sRepo, uRepo, testCookieName)
			handlers.NewUsersHandler(db, uRepo, repository.NewAccountRepository(db), setRepo, a2).Register(a)
			handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(a)
			ch := handlers.NewCampaignsHandler(cRepo, ctRepo, a2, nil, nil, nil, nil, nil)
			ch.Register(a)
			handlers.NewCampaignReadHandler(overview.New(cRepo, postRepo, pRepo), nil, a2).Register(a)
			return a, postRepo
		}

		seedCookie := func(a *fiber.App, email string) *http.Cookie {
			seedTenantUser(db, "Overview User", email, "overview-password")
			loginBody, _ := json.Marshal(fiber.Map{"email": email, "password": "overview-password"})
			loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
			loginReq.Header.Set("Content-Type", "application/json")
			loginResp, err := a.Test(loginReq)
			Expect(err).NotTo(HaveOccurred())
			var ck *http.Cookie
			for _, c := range loginResp.Cookies() {
				ck = c
			}
			return ck
		}

		createCampaignOn := func(a *fiber.App, ck *http.Cookie, name, typeID string) models.Campaign {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": typeID})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(ck)
			resp, err := a.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaigns/someid/overview", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when the overview service is unset", func() {
				// The default app never calls SetOverviewService.
				c := createCampaign("No Overview", "Uk")
				req := httptest.NewRequest("GET", "/api/campaigns/"+c.ID+"/overview", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns 404 for an unknown campaign", func() {
				a, _ := buildOverviewApp()
				ck := seedCookie(a, "ov404@example.com")
				req := httptest.NewRequest("GET", "/api/campaigns/nonexistent/overview", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("returns the brief, phases with per-phase counts, and distribution", func() {
				a, postRepo := buildOverviewApp()
				ck := seedCookie(a, "ovok@example.com")
				camp := createCampaignOn(a, ck, "Overview Campaign", "Uk")

				// Resolve a real phase id from the campaign's (hydrated) type.
				tctx := tenantctx.With(context.Background(), models.DefaultTenantID)
				full, err := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db)).GetByID(tctx, camp.ID)
				Expect(err).NotTo(HaveOccurred())
				Expect(full.CampaignType).NotTo(BeNil())
				Expect(full.CampaignType.Phases).NotTo(BeEmpty())
				phaseID := full.CampaignType.Phases[0].ID

				seedPost := func(id, phase, ptype string, status models.PostStatus) {
					var ph *string
					if phase != "" {
						ph = &phase
					}
					Expect(postRepo.Create(tctx, &models.Post{
						ID:                  id,
						CampaignID:          camp.ID,
						CampaignTypePhaseID: ph,
						PlatformID:          "AXqWG7U2qnpt", // seeded platform (Sqid)
						PlatformPostType:    ptype,
						Title:               "t " + id,
						Content:             "c",
						Status:              status,
						MediaURLs:           models.StringSlice{},
						UsedAssetIDs:        models.StringSlice{},
						CTAType:             models.CTATypeNone,
						CreatedBy:           camp.CreatedBy,
					})).To(Succeed())
				}
				seedPost("ov-a", phaseID, "text-post", models.PostStatusDraft)
				seedPost("ov-b", phaseID, "article", models.PostStatusPublished)
				seedPost("ov-c", "", "text-post", models.PostStatusDraft) // no phase → unassigned

				req := httptest.NewRequest("GET", "/api/campaigns/"+camp.ID+"/overview", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var ov overview.Overview
				Expect(json.NewDecoder(resp.Body).Decode(&ov)).To(Succeed())
				Expect(ov.CampaignID).To(Equal(camp.ID))
				Expect(ov.Brief.Description).To(Equal(full.Description))
				Expect(ov.Phases).NotTo(BeEmpty(), "campaign type phases should be surfaced")
				Expect(ov.TotalPosts).To(Equal(3))
				Expect(ov.Distribution.UnassignedPhasePostCount).To(Equal(1))

				var chosen int
				for _, p := range ov.Phases {
					if p.ID == phaseID {
						chosen = p.PostCount
					}
				}
				Expect(chosen).To(Equal(2), "the seeded phase should hold 2 posts")

				statusTotal := 0
				for _, b := range ov.Distribution.ByStatus {
					statusTotal += b.Count
				}
				Expect(statusTotal).To(Equal(3), "byStatus should reconcile with totalPosts")
			})
		})
	})

	// ── Batched summaries (CON-152) ─────────────────────────────────────────────

	Describe("GET /api/campaigns/summaries", func() {
		errorHandler := func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}

		// buildSummariesApp wires an app whose campaigns handler has the summaries
		// service set, and returns a post repository for seeding.
		buildSummariesApp := func() (*fiber.App, repository.PostRepository) {
			a := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			ctRepo := repository.NewCampaignTypeRepository(db)
			cRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
			postRepo := repository.NewPostRepository(db)
			sRepo := repository.NewSessionRepository(db)
			setRepo := repository.NewSettingRepository(db)
			uRepo := repository.NewUserRepository(db)
			a2 := handlers.RequireAuth(sRepo, uRepo, testCookieName)
			handlers.NewUsersHandler(db, uRepo, repository.NewAccountRepository(db), setRepo, a2).Register(a)
			handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(a)
			ch := handlers.NewCampaignsHandler(cRepo, ctRepo, a2, nil, nil, nil, nil, nil)
			// Read handler before ch so the static /summaries route wins over /:id.
			handlers.NewCampaignReadHandler(nil, summaries.New(postRepo), a2).Register(a)
			ch.Register(a)
			return a, postRepo
		}

		seedCookie := func(a *fiber.App, email string) *http.Cookie {
			seedTenantUser(db, "Summaries User", email, "summaries-password")
			loginBody, _ := json.Marshal(fiber.Map{"email": email, "password": "summaries-password"})
			loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
			loginReq.Header.Set("Content-Type", "application/json")
			loginResp, err := a.Test(loginReq)
			Expect(err).NotTo(HaveOccurred())
			var ck *http.Cookie
			for _, c := range loginResp.Cookies() {
				ck = c
			}
			return ck
		}

		createCampaignOn := func(a *fiber.App, ck *http.Cookie, name, typeID string) models.Campaign {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": typeID})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(ck)
			resp, err := a.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaigns/summaries", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when unset — proving /summaries is not shadowed by /:id", func() {
				// The default app registers the route but never calls
				// SetSummariesService. A 503 (not a 404 "campaign not found")
				// proves the static /summaries segment resolves to the Summaries
				// handler rather than being captured as an :id by GET /:id.
				req := httptest.NewRequest("GET", "/api/campaigns/summaries", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns slim post projections grouped by campaign, omitting empty campaigns", func() {
				a, postRepo := buildSummariesApp()
				ck := seedCookie(a, "sumok@example.com")
				withPosts := createCampaignOn(a, ck, "Has Posts", "Uk")
				empty := createCampaignOn(a, ck, "No Posts", "Uk")

				tctx := tenantctx.With(context.Background(), models.DefaultTenantID)
				scheduled := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
				seedPost := func(id string, status models.PostStatus, at *time.Time) {
					Expect(postRepo.Create(tctx, &models.Post{
						ID:               id,
						CampaignID:       withPosts.ID,
						PlatformID:       "AXqWG7U2qnpt",
						PlatformPostType: "text-post",
						Title:            "title " + id,
						Content:          "body " + id,
						Status:           status,
						ScheduledAt:      at,
						MediaURLs:        models.StringSlice{},
						UsedAssetIDs:     models.StringSlice{},
						CTAType:          models.CTATypeNone,
						CreatedBy:        withPosts.CreatedBy,
					})).To(Succeed())
				}
				seedPost("sum-a", models.PostStatusDraft, nil)
				seedPost("sum-b", models.PostStatusScheduled, &scheduled)

				req := httptest.NewRequest("GET", "/api/campaigns/summaries", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var out summaries.Summaries
				Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
				Expect(out.GeneratedAt).NotTo(BeZero())

				// Only the campaign with posts appears; the empty one is absent.
				Expect(out.Summaries).To(HaveLen(1))
				Expect(out.Summaries[0].CampaignID).To(Equal(withPosts.ID))
				for _, s := range out.Summaries {
					Expect(s.CampaignID).NotTo(Equal(empty.ID), "a campaign with no posts must be absent")
				}

				posts := out.Summaries[0].Posts
				Expect(posts).To(HaveLen(2))
				byID := map[string]summaries.PostSummary{}
				for _, p := range posts {
					byID[p.ID] = p
					// media_urls serialises as [] not null.
					Expect(p.MediaURLs).NotTo(BeNil())
				}
				Expect(byID["sum-a"].Status).To(Equal("draft"))
				Expect(byID["sum-b"].Status).To(Equal("scheduled"))
				Expect(byID["sum-b"].ScheduledAt).NotTo(BeNil())
			})
		})
	})

	// ── Targeted generation (CON-114) ──────────────────────────────────────────

	Describe("POST /api/campaigns/:id/generate-posts", func() {
		errorHandler := func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}

		buildGenApp := func(stub func(context.Context, content_plan.GeneratePostsRequest, content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)) *fiber.App {
			a := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			ctRepo := repository.NewCampaignTypeRepository(db)
			cRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
			sRepo := repository.NewSessionRepository(db)
			setRepo := repository.NewSettingRepository(db)
			uRepo := repository.NewUserRepository(db)
			a2 := handlers.RequireAuth(sRepo, uRepo, testCookieName)
			handlers.NewUsersHandler(db, uRepo, repository.NewAccountRepository(db), setRepo, a2).Register(a)
			handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(a)
			ch := handlers.NewCampaignsHandler(cRepo, ctRepo, a2, nil, nil, nil, nil, nil)
			ch.Register(a)
			handlers.NewCampaignGenerationHandler(cRepo, stub, 10, nil, nil, nil, nil, a2).Register(a)
			return a
		}

		seedCookie := func(a *fiber.App, email string) *http.Cookie {
			seedTenantUser(db, "Gen User", email, "gen-password")
			loginBody, _ := json.Marshal(fiber.Map{"email": email, "password": "gen-password"})
			loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
			loginReq.Header.Set("Content-Type", "application/json")
			loginResp, err := a.Test(loginReq)
			Expect(err).NotTo(HaveOccurred())
			var ck *http.Cookie
			for _, c := range loginResp.Cookies() {
				ck = c
			}
			return ck
		}

		createCampaignOn := func(a *fiber.App, ck *http.Cookie, name, typeID string) models.Campaign {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": typeID})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(ck)
			resp, err := a.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c
		}

		type sseEvent struct{ event, data string }
		parseSSE := func(r *bufio.Scanner) []sseEvent {
			var events []sseEvent
			var curEvent, curData string
			for r.Scan() {
				line := r.Text()
				switch {
				case strings.HasPrefix(line, "event: "):
					curEvent = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					curData = strings.TrimPrefix(line, "data: ")
				case line == "":
					if curEvent != "" {
						events = append(events, sseEvent{curEvent, curData})
					}
					curEvent, curData = "", ""
				}
			}
			return events
		}

		okStub := func(_ context.Context, req content_plan.GeneratePostsRequest, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
			onEvent(content_plan.SSEEventStep, content_plan.StepEventPayload{Step: "resolveTargets", Status: "done"})
			onEvent(content_plan.SSEEventPost, content_plan.PostEventPayload{Post: content_plan.DraftPost{Title: "Draft"}, Index: 0, ID: "post-1"})
			return &content_plan.ContentPlanResponse{CampaignID: req.CampaignID, Posts: []content_plan.DraftPost{{Title: "Draft"}}}, nil
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("POST", "/api/campaigns/someid/generate-posts", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when targeted generation is unwired", func() {
				c := createCampaign("No Gen", "Uk")
				body, _ := json.Marshal(fiber.Map{"platformIds": []string{"x"}, "phaseId": "p", "count": 3})
				req := httptest.NewRequest("POST", "/api/campaigns/"+c.ID+"/generate-posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns 400 when count is below 1", func() {
				a := buildGenApp(okStub)
				ck := seedCookie(a, "gen400@example.com")
				camp := createCampaignOn(a, ck, "Gen 400", "Uk")
				body, _ := json.Marshal(fiber.Map{"platformIds": []string{"x"}, "phaseId": "p", "count": 0})
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/generate-posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 404 for an unknown campaign", func() {
				a := buildGenApp(okStub)
				ck := seedCookie(a, "gen404@example.com")
				body, _ := json.Marshal(fiber.Map{"platformIds": []string{"x"}, "phaseId": "p", "count": 3})
				req := httptest.NewRequest("POST", "/api/campaigns/nonexistent/generate-posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("streams step, post, and complete on success", func() {
				a := buildGenApp(okStub)
				ck := seedCookie(a, "genok@example.com")
				camp := createCampaignOn(a, ck, "Gen OK", "Uk")
				body, _ := json.Marshal(fiber.Map{"platformIds": []string{"AXqWG7U2qnpt"}, "phaseId": "p", "count": 2})
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/generate-posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				events := parseSSE(bufio.NewScanner(resp.Body))
				names := make([]string, len(events))
				for i, e := range events {
					names[i] = e.event
				}
				Expect(names).To(Equal([]string{"step", "post", "complete"}))
			})
		})
	})

	// ── Consistency reviews (CON-116) ──────────────────────────────────────────

	Describe("POST /api/campaigns/:id/brief-review and /posts-review", func() {
		errorHandler := func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}

		buildReviewApp := func(
			checkBrief func(context.Context, string, consistency.OnEventFunc) (*consistency.BriefReview, error),
			checkPosts func(context.Context, consistency.PostsCheckRequest, consistency.OnEventFunc) (*consistency.PostsReview, error),
		) *fiber.App {
			a := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			ctRepo := repository.NewCampaignTypeRepository(db)
			cRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
			sRepo := repository.NewSessionRepository(db)
			setRepo := repository.NewSettingRepository(db)
			uRepo := repository.NewUserRepository(db)
			a2 := handlers.RequireAuth(sRepo, uRepo, testCookieName)
			handlers.NewUsersHandler(db, uRepo, repository.NewAccountRepository(db), setRepo, a2).Register(a)
			handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(a)
			ch := handlers.NewCampaignsHandler(cRepo, ctRepo, a2, nil, nil, nil, nil, nil)
			ch.Register(a)
			handlers.NewCampaignGenerationHandler(cRepo, nil, 0, checkBrief, checkPosts, nil, nil, a2).Register(a)
			return a
		}

		seedCookie := func(a *fiber.App, email string) *http.Cookie {
			seedTenantUser(db, "Review User", email, "review-password")
			loginBody, _ := json.Marshal(fiber.Map{"email": email, "password": "review-password"})
			loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
			loginReq.Header.Set("Content-Type", "application/json")
			loginResp, err := a.Test(loginReq)
			Expect(err).NotTo(HaveOccurred())
			var ck *http.Cookie
			for _, c := range loginResp.Cookies() {
				ck = c
			}
			return ck
		}

		createCampaignOn := func(a *fiber.App, ck *http.Cookie, name, typeID string) models.Campaign {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": typeID})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(ck)
			resp, err := a.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c
		}

		type sseEvent struct{ event, data string }
		parseSSE := func(r *bufio.Scanner) []sseEvent {
			var events []sseEvent
			var curEvent, curData string
			for r.Scan() {
				line := r.Text()
				switch {
				case strings.HasPrefix(line, "event: "):
					curEvent = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "data: "):
					curData = strings.TrimPrefix(line, "data: ")
				case line == "":
					if curEvent != "" {
						events = append(events, sseEvent{curEvent, curData})
					}
					curEvent, curData = "", ""
				}
			}
			return events
		}

		briefStub := func(_ context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error) {
			onEvent(consistency.SSEEventStep, consistency.StepEventPayload{Step: "analyze", Status: "done"})
			return &consistency.BriefReview{
				CampaignID: campaignID,
				Consistent: false,
				Findings:   []consistency.Finding{{Aspect: "persona", Severity: "high", Issue: "vague", Suggestion: "sharpen"}},
				Summary:    "one issue",
			}, nil
		}
		postsStub := func(_ context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error) {
			onEvent(consistency.SSEEventStep, consistency.StepEventPayload{Step: "analyze", Status: "done"})
			return &consistency.PostsReview{
				CampaignID: req.CampaignID,
				Checked:    2,
				Total:      2,
				Findings:   []consistency.PostFinding{},
				Summary:    "all aligned",
			}, nil
		}

		Context("when not authenticated", func() {
			It("returns 401 for brief-review", func() {
				req := httptest.NewRequest("POST", "/api/campaigns/someid/brief-review", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
			It("returns 401 for posts-review", func() {
				req := httptest.NewRequest("POST", "/api/campaigns/someid/posts-review", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when the reviews are unwired", func() {
				c := createCampaign("No Review", "Uk")
				req := httptest.NewRequest("POST", "/api/campaigns/"+c.ID+"/brief-review", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})

			It("returns 404 for an unknown campaign", func() {
				a := buildReviewApp(briefStub, postsStub)
				ck := seedCookie(a, "review404@example.com")
				req := httptest.NewRequest("POST", "/api/campaigns/nonexistent/brief-review", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("streams step and complete on brief-review success", func() {
				a := buildReviewApp(briefStub, postsStub)
				ck := seedCookie(a, "briefok@example.com")
				camp := createCampaignOn(a, ck, "Brief OK", "Uk")
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/brief-review", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				events := parseSSE(bufio.NewScanner(resp.Body))
				names := make([]string, len(events))
				for i, e := range events {
					names[i] = e.event
				}
				Expect(names).To(Equal([]string{"step", "complete"}))
			})

			It("streams step and complete on posts-review success with an empty body", func() {
				a := buildReviewApp(briefStub, postsStub)
				ck := seedCookie(a, "postsok@example.com")
				camp := createCampaignOn(a, ck, "Posts OK", "Uk")
				req := httptest.NewRequest("POST", "/api/campaigns/"+camp.ID+"/posts-review", nil)
				req.AddCookie(ck)
				resp, err := a.Test(req, 10000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				events := parseSSE(bufio.NewScanner(resp.Body))
				names := make([]string, len(events))
				for i, e := range events {
					names[i] = e.event
				}
				Expect(names).To(Equal([]string{"step", "complete"}))
			})
		})
	})

	// ── Delete ───────────────────────────────────────────────────────────────

	Describe("DELETE /api/campaigns/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/campaigns/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("soft-deletes the campaign: 204, then gone from reads and list", func() {
				c := createCampaign("To Delete", "Ef")

				req := httptest.NewRequest("DELETE", "/api/campaigns/"+c.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(204))

				// GET the deleted campaign → 404 (soft-deleted rows read as gone).
				getReq := httptest.NewRequest("GET", "/api/campaigns/"+c.ID, nil)
				getReq.AddCookie(authCookie)
				getResp, err := app.Test(getReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(getResp.StatusCode).To(Equal(404))

				// It no longer appears in the active list.
				listReq := httptest.NewRequest("GET", "/api/campaigns", nil)
				listReq.AddCookie(authCookie)
				listResp, err := app.Test(listReq)
				Expect(err).NotTo(HaveOccurred())
				var campaigns []models.Campaign
				Expect(json.NewDecoder(listResp.Body).Decode(&campaigns)).To(Succeed())
				Expect(campaigns).To(BeEmpty())
			})

			It("returns 404 when deleting an already-deleted campaign", func() {
				c := createCampaign("Delete Twice", "Ef")
				del := func() int {
					req := httptest.NewRequest("DELETE", "/api/campaigns/"+c.ID, nil)
					req.AddCookie(authCookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					return resp.StatusCode
				}
				Expect(del()).To(Equal(204))
				Expect(del()).To(Equal(404))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("DELETE", "/api/campaigns/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Archive / Unarchive (CON-156 BE 6) ─────────────────────────────────────

	Describe("POST /api/campaigns/:id/archive and /unarchive", func() {
		archive := func(id string) int {
			req := httptest.NewRequest("POST", "/api/campaigns/"+id+"/archive", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp.StatusCode
		}
		unarchive := func(id string) int {
			req := httptest.NewRequest("POST", "/api/campaigns/"+id+"/unarchive", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp.StatusCode
		}
		listActive := func() []models.Campaign {
			req := httptest.NewRequest("GET", "/api/campaigns", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			var out []models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			return out
		}
		listArchived := func() []models.Campaign {
			req := httptest.NewRequest("GET", "/api/campaigns?archived=true", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			var out []models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			return out
		}

		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("POST", "/api/campaigns/someid/archive", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("archives then unarchives, moving the campaign between lists", func() {
				c := createCampaign("Archive Me", "Ef")

				Expect(archive(c.ID)).To(Equal(204))
				// Gone from active, present in archived.
				Expect(listActive()).To(BeEmpty())
				arch := listArchived()
				Expect(arch).To(HaveLen(1))
				Expect(arch[0].ID).To(Equal(c.ID))
				Expect(arch[0].ArchivedAt).NotTo(BeNil())
				// Still directly readable (so it can be unarchived).
				getReq := httptest.NewRequest("GET", "/api/campaigns/"+c.ID, nil)
				getReq.AddCookie(authCookie)
				getResp, err := app.Test(getReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(getResp.StatusCode).To(Equal(200))

				Expect(unarchive(c.ID)).To(Equal(204))
				Expect(listArchived()).To(BeEmpty())
				active := listActive()
				Expect(active).To(HaveLen(1))
				Expect(active[0].ID).To(Equal(c.ID))
				Expect(active[0].ArchivedAt).To(BeNil())
			})

			It("returns 404 when archiving an unknown id", func() {
				Expect(archive("nonexistent")).To(Equal(404))
			})

			It("returns 404 when archiving a soft-deleted campaign", func() {
				c := createCampaign("Deleted Then Archived", "Ef")
				req := httptest.NewRequest("DELETE", "/api/campaigns/"+c.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(204))
				Expect(archive(c.ID)).To(Equal(404))
			})
		})
	})
})
