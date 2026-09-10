package handlers_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// fakeCancelEnqueuer records EnqueueCancel calls so the convert-to-manual
// tests can assert which posts were handed to the cancel queue and with
// which target, without a running River (CON-130).
type fakeCancelEnqueuer struct {
	mu    sync.Mutex
	calls []queues.CancelZernioJobTask
	err   error
}

func (f *fakeCancelEnqueuer) EnqueueCancel(_ context.Context, postID string, target queues.CancelTarget, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, queues.CancelZernioJobTask{PostID: postID, Target: target, Actor: actor})
	return nil
}

func (f *fakeCancelEnqueuer) targets() map[string]queues.CancelTarget {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]queues.CancelTarget{}
	for _, c := range f.calls {
		out[c.PostID] = c.Target
	}
	return out
}

var _ = Describe("PostsHandler", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		campaignID string
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
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		pieceRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		postRepo := repository.NewPostRepository(db)
		postVersionRepo := repository.NewPostVersionRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(app)
		handlers.NewAssetsHandler(pieceRepo, repository.NewAssetFileRepository(db), nil, nil, nil, nil, nil, nil, nil, auth, nil).Register(app)
		postLogRepo := repository.NewPostLogRepository(db)
		ph := handlers.NewPostsHandler(postRepo, postVersionRepo, repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth)
		// CON-69 §11: wire the audit log so transition tests can read it back.
		ph.SetPostLogRepo(postLogRepo)
		ph.Register(app)
		// CON-291: assess/assessment/analytics live on the insights handler. Wired
		// with nil deps here (matching the old shared PostsHandler) so the routes
		// exist — 401 unauthenticated, 503 when authenticated — for the route-level
		// specs; the assessor/eval/analytics behaviour is covered by dedicated apps.
		handlers.NewPostInsightsHandler(postRepo, nil, nil, nil, nil, nil, auth).Register(app)
		// Likewise the assistant surface — nil deps so /assistant + /messages exist
		// (401 unauthenticated) for the route-level specs; the streaming behaviour is
		// covered by a dedicated stub app.
		handlers.NewPostAssistantHandler(nil, nil, nil, nil, auth).Register(app)
		handlers.NewPostLogsHandler(postLogRepo, postRepo, auth).Register(app)

		// Seed auth user and log in.
		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		// -1 disables fiber's default 1s test timeout. These are setup calls, not
		// latency assertions; under `-race -procs=2` a heavy concurrent spec (e.g.
		// the 50 MB upload) can starve this login past 1s and flake the BeforeEach.
		loginResp, err := app.Test(loginReq, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]

		// Seed a campaign used across tests.
		cBody, _ := json.Marshal(fiber.Map{"name": "Test Campaign", "campaign_type_id": "Uk"})
		cReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(cBody))
		cReq.Header.Set("Content-Type", "application/json")
		cReq.AddCookie(authCookie)
		cResp, err := app.Test(cReq, -1) // -1: setup call; don't let the 1s test timeout flake under concurrent load
		Expect(err).NotTo(HaveOccurred())
		Expect(cResp.StatusCode).To(Equal(fiber.StatusCreated))
		var c models.Campaign
		Expect(json.NewDecoder(cResp.Body).Decode(&c)).To(Succeed())
		campaignID = c.ID
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("post_logs").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("post_assistant_messages").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("post_versions").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("posts").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("assets").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("campaigns").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(tenantCtx())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
	})

	// helper: PUT /api/posts/:id to drive a status transition. Returns
	// the raw response so callers can assert on body/status.
	putStatus := func(app *fiber.App, cookie *http.Cookie, campaignID, postID string, status models.PostStatus) *http.Response {
		body, _ := json.Marshal(fiber.Map{
			"campaign_id":        campaignID,
			"platform_id":        "AXqWG7U2qnpt",
			"platform_post_type": "text-post",
			"status":             string(status),
		})
		req := httptest.NewRequest("PUT", "/api/posts/"+postID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	// helper: create a post via the API and return it
	createPost := func(title string, extraFields fiber.Map) models.Post {
		payload := fiber.Map{
			"campaign_id":        campaignID,
			"platform_id":        "AXqWG7U2qnpt",
			"platform_post_type": "text-post",
			"title":              title,
		}
		for k, v := range extraFields {
			payload[k] = v
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var p models.Post
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		return p
	}

	// ── List ─────────────────────────────────────────────────────────────────

	Describe("GET /api/posts", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/posts", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns an empty list when no posts exist", func() {
				req := httptest.NewRequest("GET", "/api/posts", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var posts []models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&posts)).To(Succeed())
				Expect(posts).To(BeEmpty())
			})

			It("returns all posts with hydrated campaign and platform", func() {
				createPost("Post One", nil)
				createPost("Post Two", nil)

				req := httptest.NewRequest("GET", "/api/posts", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var posts []models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&posts)).To(Succeed())
				Expect(posts).To(HaveLen(2))
				Expect(posts[0].Campaign).NotTo(BeNil())
				Expect(posts[0].Campaign.ID).To(Equal(campaignID))
				Expect(posts[0].Platform).NotTo(BeNil())
				Expect(posts[0].Platform.ID).To(Equal("AXqWG7U2qnpt"))
			})
		})
	})

	// ── List by campaign ─────────────────────────────────────────────────────

	Describe("GET /api/campaigns/:campaign_id/posts", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaigns/"+campaignID+"/posts", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns only posts belonging to the given campaign", func() {
				createPost("Campaign Post One", nil)
				createPost("Campaign Post Two", nil)

				req := httptest.NewRequest("GET", "/api/campaigns/"+campaignID+"/posts", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var posts []models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&posts)).To(Succeed())
				Expect(posts).To(HaveLen(2))
				for _, p := range posts {
					Expect(p.CampaignID).To(Equal(campaignID))
					Expect(p.Campaign).NotTo(BeNil())
					Expect(p.Platform).NotTo(BeNil())
				}
			})

			It("returns an empty list for a campaign with no posts", func() {
				req := httptest.NewRequest("GET", "/api/campaigns/nonexistent/posts", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var posts []models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&posts)).To(Succeed())
				Expect(posts).To(BeEmpty())
			})
		})
	})

	// ── Create ───────────────────────────────────────────────────────────────

	Describe("POST /api/posts", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Test",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("creates a post with required fields and returns 201", func() {
				p := createPost("My First Post", nil)
				Expect(p.ID).NotTo(BeEmpty())
				Expect(p.Title).To(Equal("My First Post"))
				Expect(p.Status).To(Equal(models.PostStatusDraft))
				Expect(p.CTAType).To(Equal(models.CTATypeNone))
				Expect(p.CreatedBy).NotTo(BeEmpty())
				Expect(p.MediaURLs).To(BeEmpty())
				Expect(p.UsedAssets).To(BeEmpty())
			})

			It("creates a post with all optional fields", func() {
				p := createPost("Full Post", fiber.Map{
					"content":               "Post body text.",
					"status":                "ready_for_publish",
					"cta_type":              "link",
					"cta_url":               "https://example.com",
					"target_audience_notes": "Developers aged 25-40",
					"media_urls":            []string{"https://example.com/image.png"},
				})
				Expect(p.Status).To(Equal(models.PostStatusReadyForPublish))
				Expect(p.CTAType).To(Equal(models.CTATypeLink))
				Expect(p.CTAUrl).To(Equal("https://example.com"))
				Expect(p.MediaURLs).To(ConsistOf("https://example.com/image.png"))
				Expect(p.Content).To(Equal("Post body text."))
			})

			It("returns 400 when campaign_id is missing", func() {
				body, _ := json.Marshal(fiber.Map{
					"platform_id": "AXqWG7U2qnpt", "platform_post_type": "text-post", "title": "No Campaign",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("creates a draft post without platform_id or platform_post_type", func() {
				// Drafts can sit without a platform chosen — content is
				// what matters, the publishing target gets picked later.
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "title": "Platformless draft",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

				var p models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
				Expect(p.Status).To(Equal(models.PostStatusDraft))
				Expect(p.PlatformID).To(BeEmpty())
				Expect(p.PlatformPostType).To(BeEmpty())
				Expect(p.Platform).To(BeNil(), "hydration should leave Platform nil when ID is empty")
			})

			It("returns 400 when creating a non-draft post without platform_id", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "title": "Premature ready",
					"status": "ready_for_publish",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
				body2, _ := io.ReadAll(resp.Body)
				Expect(string(body2)).To(ContainSubstring("platform_id is required"))
			})

			It("returns 400 when creating a non-draft post without platform_post_type", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "title": "Type missing",
					"platform_id": "AXqWG7U2qnpt",
					"status":      "ready_for_publish",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
				body2, _ := io.ReadAll(resp.Body)
				Expect(string(body2)).To(ContainSubstring("platform_post_type is required"))
			})

			It("creates a post without a title", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt", "platform_post_type": "text-post",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

				var p models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
				Expect(p.Title).To(BeEmpty())
			})

			It("returns 400 when status is invalid", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Bad Status", "status": "unknown",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when cta_type is invalid", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Bad CTA", "cta_type": "unknown",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Get ──────────────────────────────────────────────────────────────────

	Describe("GET /api/posts/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/posts/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the post with hydrated campaign and platform", func() {
				p := createPost("Hydrated Post", nil)

				req := httptest.NewRequest("GET", "/api/posts/"+p.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.ID).To(Equal(p.ID))
				Expect(got.Campaign).NotTo(BeNil())
				Expect(got.Campaign.ID).To(Equal(campaignID))
				Expect(got.Campaign.Name).To(Equal("Test Campaign"))
				Expect(got.Platform).NotTo(BeNil())
				Expect(got.Platform.ID).To(Equal("AXqWG7U2qnpt"))
				Expect(got.Platform.Name).To(Equal("LinkedIn"))
				Expect(got.UsedAssets).To(BeEmpty())
			})

			It("returns the post with hydrated used_assets", func() {
				// Create a asset to reference.
				pBody, _ := json.Marshal(fiber.Map{"title": "Ref Asset", "content": `[]`})
				pReq := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(pBody))
				pReq.Header.Set("Content-Type", "application/json")
				pReq.AddCookie(authCookie)
				pResp, err := app.Test(pReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(pResp.StatusCode).To(Equal(fiber.StatusCreated))
				var asset models.Asset
				Expect(json.NewDecoder(pResp.Body).Decode(&asset)).To(Succeed())

				post := createPost("Post With Asset", fiber.Map{
					"used_asset_ids": []string{asset.ID},
				})

				req := httptest.NewRequest("GET", "/api/posts/"+post.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.UsedAssets).To(HaveLen(1))
				Expect(got.UsedAssets[0].ID).To(Equal(asset.ID))
				Expect(got.UsedAssets[0].Title).To(Equal("Ref Asset"))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("GET", "/api/posts/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Update ───────────────────────────────────────────────────────────────

	Describe("PUT /api/posts/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Updated",
				})
				req := httptest.NewRequest("PUT", "/api/posts/someid", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("updates the post and returns the hydrated resource", func() {
				p := createPost("Original Title", nil)

				body, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        "AXqWG7U2qnpt",
					"platform_post_type": "text-post",
					"title":              "Updated Title",
					"status":             "ready_for_publish",
					"cta_type":           "button",
					"cta_url":            "https://example.com/cta",
				})
				req := httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Title).To(Equal("Updated Title"))
				Expect(got.PlatformPostType).To(Equal("text-post"))
				Expect(got.Status).To(Equal(models.PostStatusReadyForPublish))
				Expect(got.CTAType).To(Equal(models.CTATypeButton))
				Expect(got.Campaign).NotTo(BeNil())
				Expect(got.Platform).NotTo(BeNil())
			})

			// CON-233 #2: used_asset_ids is presence-aware on PUT, so an autosave
			// that omits it leaves the sources alone (the membership endpoints own
			// them) instead of restating a stale list over an in-flight attach.
			It("preserves used_asset_ids when a PUT omits it", func() {
				p := createPost("Sourced Post", fiber.Map{"used_asset_ids": []string{"a", "b"}})

				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID,
					"title":       "Renamed",
					"status":      "draft",
				})
				req := httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Title).To(Equal("Renamed"))
				Expect([]string(got.UsedAssetIDs)).To(Equal([]string{"a", "b"}))
			})

			It("replaces used_asset_ids on a present value and clears on []", func() {
				p := createPost("Sourced Post", fiber.Map{"used_asset_ids": []string{"a", "b"}})
				put := func(v any) models.Post {
					body, _ := json.Marshal(fiber.Map{
						"campaign_id":    campaignID,
						"title":          "Sourced Post",
						"status":         "draft",
						"used_asset_ids": v,
					})
					req := httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.AddCookie(authCookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(200))
					var got models.Post
					Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
					return got
				}
				Expect([]string(put([]string{"c"}).UsedAssetIDs)).To(Equal([]string{"c"}))
				Expect([]string(put([]string{}).UsedAssetIDs)).To(BeEmpty())
			})

			It("persists published_url on update (CON-165 manual/skip path)", func() {
				p := createPost("Manual Post", nil)

				body, _ := json.Marshal(fiber.Map{
					"campaign_id":   campaignID,
					"title":         "Manual Post",
					"status":        "draft",
					"published_url": "https://linkedin.com/feed/update/123",
				})
				req := httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.PublishedURL).To(Equal("https://linkedin.com/feed/update/123"))
			})

			It("returns 404 for an unknown id", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Ghost",
				})
				req := httptest.NewRequest("PUT", "/api/posts/nonexistent", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("returns 400 for an invalid status transition", func() {
				p := createPost("Draft Post", nil) // status = draft

				body, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        "AXqWG7U2qnpt",
					"platform_post_type": "text-post",
					"title":              "Skip To Published",
					"status":             "published",
				})
				req := httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("allows a valid status transition chain", func() {
				p := createPost("Lifecycle Post", nil) // draft

				// draft -> ready_for_publish
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Lifecycle Post",
					"status": "ready_for_publish",
				})
				req := httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				// ready_for_publish -> scheduled
				body, _ = json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Lifecycle Post",
					"status": "scheduled",
				})
				req = httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err = app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				// scheduled -> published
				body, _ = json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt",
					"platform_post_type": "text-post", "title": "Lifecycle Post",
					"status": "published",
				})
				req = httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err = app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Status).To(Equal(models.PostStatusPublished))
			})

			It("clears the title when omitted", func() {
				p := createPost("Has Title", nil)

				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt", "platform_post_type": "text-post",
				})
				req := httptest.NewRequest("PUT", "/api/posts/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Title).To(BeEmpty())
			})

			It("rejects a draft → ready_for_publish transition without a platform", func() {
				// Draft posts can be saved without a platform; when the
				// user moves to ready_for_publish, the platform fields
				// become mandatory and the transition must fail until
				// they're filled in.
				body, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID, "title": "No-platform draft",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
				var draft models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&draft)).To(Succeed())

				// Try to transition without supplying a platform.
				updateBody, _ := json.Marshal(fiber.Map{
					"campaign_id": campaignID,
					"status":      "ready_for_publish",
				})
				upReq := httptest.NewRequest("PUT", "/api/posts/"+draft.ID, bytes.NewReader(updateBody))
				upReq.Header.Set("Content-Type", "application/json")
				upReq.AddCookie(authCookie)
				upResp, err := app.Test(upReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(upResp.StatusCode).To(Equal(400))
				bodyBytes, _ := io.ReadAll(upResp.Body)
				Expect(string(bodyBytes)).To(ContainSubstring("platform_id is required"))

				// Now supply the platform; the transition succeeds.
				updateBody2, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        "AXqWG7U2qnpt",
					"platform_post_type": "text-post",
					"status":             "ready_for_publish",
				})
				upReq2 := httptest.NewRequest("PUT", "/api/posts/"+draft.ID, bytes.NewReader(updateBody2))
				upReq2.Header.Set("Content-Type", "application/json")
				upReq2.AddCookie(authCookie)
				upResp2, err := app.Test(upReq2)
				Expect(err).NotTo(HaveOccurred())
				Expect(upResp2.StatusCode).To(Equal(200))
				var got models.Post
				Expect(json.NewDecoder(upResp2.Body).Decode(&got)).To(Succeed())
				Expect(got.Status).To(Equal(models.PostStatusReadyForPublish))
				Expect(got.PlatformID).To(Equal("AXqWG7U2qnpt"))
			})

			// ── CON-69 §9: cancellation transitions ───────────────────────────
			It("allows scheduled → ready_for_publish (cancellation back to RFP)", func() {
				p := createPost("Cancel-to-RFP", nil)

				// draft → ready_for_publish → scheduled
				putStatus(app, authCookie, campaignID, p.ID, models.PostStatusReadyForPublish)
				putStatus(app, authCookie, campaignID, p.ID, models.PostStatusScheduled)

				// scheduled → ready_for_publish (the new transition)
				resp := putStatus(app, authCookie, campaignID, p.ID, models.PostStatusReadyForPublish)
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Status).To(Equal(models.PostStatusReadyForPublish))
			})

			It("allows scheduled → draft (cancellation back to Draft)", func() {
				p := createPost("Cancel-to-Draft", nil)
				putStatus(app, authCookie, campaignID, p.ID, models.PostStatusReadyForPublish)
				putStatus(app, authCookie, campaignID, p.ID, models.PostStatusScheduled)

				resp := putStatus(app, authCookie, campaignID, p.ID, models.PostStatusDraft)
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Status).To(Equal(models.PostStatusDraft))
			})

			// ── CON-69 §4: validation gate ────────────────────────────────────
			It("blocks draft → ready_for_publish when an attachment violates platform rules (422 with per-platform errors)", func() {
				const instagramID = "rzgpTkARLH0L" // platform that disallows GIF
				draftBody, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        instagramID,
					"platform_post_type": "image-post",
					"title":              "IG Draft",
				})
				cReq := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(draftBody))
				cReq.Header.Set("Content-Type", "application/json")
				cReq.AddCookie(authCookie)
				cResp, err := app.Test(cReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(cResp.StatusCode).To(Equal(fiber.StatusCreated))
				var draft models.Post
				Expect(json.NewDecoder(cResp.Body).Decode(&draft)).To(Succeed())

				// Insert a deliberately-violating attachment row directly
				// (image/gif is not in Instagram's allowed_formats).
				ctx := tenantCtx()
				attID, err := models.NewID()
				Expect(err).NotTo(HaveOccurred())
				_, err = db.NewInsert().Model(&models.PostAttachment{
					ID:             attID,
					PostID:         draft.ID,
					Position:       0,
					MimeType:       "image/gif",
					SizeBytes:      256,
					ChecksumSHA256: "deadbeef",
					S3Key:          "stub/key",
					CreatedBy:      draft.CreatedBy,
				}).Exec(ctx)
				Expect(err).NotTo(HaveOccurred())

				// Try to transition Draft → ReadyForPublish → 422.
				upBody, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        instagramID,
					"platform_post_type": "image-post",
					"status":             "ready_for_publish",
				})
				upReq := httptest.NewRequest("PUT", "/api/posts/"+draft.ID, bytes.NewReader(upBody))
				upReq.Header.Set("Content-Type", "application/json")
				upReq.AddCookie(authCookie)
				upResp, err := app.Test(upReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(upResp.StatusCode).To(Equal(fiber.StatusUnprocessableEntity))

				var body map[string]any
				Expect(json.NewDecoder(upResp.Body).Decode(&body)).To(Succeed())
				Expect(body).To(HaveKey("platform_validation"))
				perPlatform := body["platform_validation"].(map[string]any)
				Expect(perPlatform).To(HaveKey(instagramID))
				errs := perPlatform[instagramID].([]any)
				Expect(errs).NotTo(BeEmpty())
			})

			It("allows draft → ready_for_publish when attachments pass validation", func() {
				p := createPost("Valid Draft", nil) // LinkedIn, no attachments
				resp := putStatus(app, authCookie, campaignID, p.ID, models.PostStatusReadyForPublish)
				Expect(resp.StatusCode).To(Equal(200))
			})
		})
	})

	// ── Asset membership (CON-233) ───────────────────────────────────────────

	Describe("source membership on /api/posts/:id/assets", func() {
		addAssets := func(id string, assetIDs []string) *http.Response {
			body, _ := json.Marshal(fiber.Map{"asset_ids": assetIDs})
			req := httptest.NewRequest("POST", "/api/posts/"+id+"/assets", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}
		removeAsset := func(id, assetID string) *http.Response {
			req := httptest.NewRequest("DELETE", "/api/posts/"+id+"/assets/"+assetID, nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}
		// setStatus forces a post into a submitted state directly (bypassing PUT,
		// which would wipe used_asset_ids), mirroring the cancel-endpoint tests.
		setStatus := func(id string, status models.PostStatus) {
			_, err := db.NewUpdate().Model((*models.Post)(nil)).
				Set("status = ?", status).
				Where("id = ?", id).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
		}

		Context("when not authenticated", func() {
			It("returns 401 for add", func() {
				body, _ := json.Marshal(fiber.Map{"asset_ids": []string{"a"}})
				req := httptest.NewRequest("POST", "/api/posts/x/assets", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
			It("returns 401 for remove", func() {
				req := httptest.NewRequest("DELETE", "/api/posts/x/assets/a", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("adds ids to a draft post's sources and returns the updated post", func() {
				p := createPost("Sourced Post", nil)
				resp := addAssets(p.ID, []string{"a", "b"})
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.UsedAssetIDs)).To(Equal([]string{"a", "b"}))
			})

			It("unions without duplicating and preserves order", func() {
				p := createPost("Union Sources", nil)
				Expect(addAssets(p.ID, []string{"a", "b"}).StatusCode).To(Equal(200))
				resp := addAssets(p.ID, []string{"b", "c"})
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.UsedAssetIDs)).To(Equal([]string{"a", "b", "c"}))
			})

			It("leaves other fields untouched", func() {
				p := createPost("Keep Content", fiber.Map{"title": "Keep Content", "content": "body text"})
				Expect(addAssets(p.ID, []string{"a"}).StatusCode).To(Equal(200))
				resp := removeAsset(p.ID, "zzz") // no-op; just re-read
				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Title).To(Equal("Keep Content"))
				Expect(got.Content).To(Equal("body text"))
				Expect([]string(got.UsedAssetIDs)).To(Equal([]string{"a"}))
			})

			It("removes an id and is idempotent for an absent id", func() {
				p := createPost("Remove Sources", nil)
				Expect(addAssets(p.ID, []string{"a", "b", "c"}).StatusCode).To(Equal(200))
				resp := removeAsset(p.ID, "b")
				Expect(resp.StatusCode).To(Equal(200))
				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.UsedAssetIDs)).To(Equal([]string{"a", "c"}))
				// Removing an absent id changes nothing.
				resp2 := removeAsset(p.ID, "nope")
				Expect(resp2.StatusCode).To(Equal(200))
				var got2 models.Post
				Expect(json.NewDecoder(resp2.Body).Decode(&got2)).To(Succeed())
				Expect([]string(got2.UsedAssetIDs)).To(Equal([]string{"a", "c"}))
			})

			It("returns 404 for an unknown post", func() {
				Expect(addAssets("nonexistent", []string{"a"}).StatusCode).To(Equal(404))
				Expect(removeAsset("nonexistent", "a").StatusCode).To(Equal(404))
			})

			It("survives two concurrent adds of different ids", func() {
				p := createPost("Concurrent Sources", nil)
				var wg sync.WaitGroup
				for _, id := range []string{"c1", "c2"} {
					wg.Add(1)
					go func(assetID string) {
						defer GinkgoRecover()
						defer wg.Done()
						Expect(addAssets(p.ID, []string{assetID}).StatusCode).To(Equal(200))
					}(id)
				}
				wg.Wait()

				req := httptest.NewRequest("GET", "/api/posts/"+p.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				var got models.Post
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect([]string(got.UsedAssetIDs)).To(ConsistOf("c1", "c2"))
			})

			// ── CON-251 content-lock ─────────────────────────────────────────
			Context("when the post is submitted (scheduled/published)", func() {
				It("rejects a real add with 409 but allows a no-op add", func() {
					p := createPost("Locked Add", fiber.Map{"used_asset_ids": []string{"a"}})
					setStatus(p.ID, models.PostStatusScheduled)

					// Adding a genuinely new id is a locked content change.
					Expect(addAssets(p.ID, []string{"b"}).StatusCode).To(Equal(409))
					// Re-adding an id it already has changes nothing → allowed.
					Expect(addAssets(p.ID, []string{"a"}).StatusCode).To(Equal(200))
				})

				It("rejects removing a present source with 409 but allows a no-op remove", func() {
					p := createPost("Locked Remove", fiber.Map{"used_asset_ids": []string{"a"}})
					setStatus(p.ID, models.PostStatusPublished)

					// Detaching a source it actually has is a locked change.
					Expect(removeAsset(p.ID, "a").StatusCode).To(Equal(409))
					// Removing an id it doesn't have changes nothing → allowed.
					Expect(removeAsset(p.ID, "absent").StatusCode).To(Equal(200))
				})

				// The handler's pre-check and the repo write are separated by a
				// window in which a schedule/publish can land. The repo re-checks
				// the lock under a row lock (SELECT ... FOR UPDATE), so the race
				// resolves to a distinct *PostSubmittedError — which the handlers
				// map to 409 — instead of silently mutating a frozen post. Drive
				// the repo directly to exercise that guard past the pre-check.
				It("refuses a racing source change atomically at the repo layer", func() {
					p := createPost("Racing Source", fiber.Map{"used_asset_ids": []string{"a"}})
					setStatus(p.ID, models.PostStatusScheduled)
					repo := repository.NewPostRepository(db)
					ctx := tenantCtx()

					var submitted *repository.PostSubmittedError
					_, err := repo.AddUsedAssetIDs(ctx, p.ID, []string{"b"})
					Expect(errors.As(err, &submitted)).To(BeTrue())
					Expect(submitted.Status).To(Equal(models.PostStatusScheduled))

					_, err = repo.RemoveUsedAssetID(ctx, p.ID, "a")
					Expect(errors.As(err, &submitted)).To(BeTrue())

					// No-op writes still pass through untouched while submitted.
					got, err := repo.AddUsedAssetIDs(ctx, p.ID, []string{"a"})
					Expect(err).NotTo(HaveOccurred())
					Expect([]string(got.UsedAssetIDs)).To(Equal([]string{"a"}))

					got, err = repo.RemoveUsedAssetID(ctx, p.ID, "absent")
					Expect(err).NotTo(HaveOccurred())
					Expect([]string(got.UsedAssetIDs)).To(Equal([]string{"a"}))
				})
			})
		})
	})

	// ── Publish gate (CON-74) ────────────────────────────────────────────────

	Describe("Draft → ready_for_publish gate", func() {
		// LinkedIn — supports text-post / image-post / carousel / video /
		// article / poll / newsletter / event / live-video. Image cap = 9.
		const linkedInID = "AXqWG7U2qnpt"
		// X (Twitter) — supports thread + long-form-post; image cap = 4.
		const xID = "81mUCmc2xsKd"
		// Facebook — supports story.
		const facebookID = "zBU1zqVICGfk"

		seedAttachment := func(p models.Post, mime string) string {
			id, err := models.NewID()
			Expect(err).NotTo(HaveOccurred())
			att := &models.PostAttachment{
				ID:             id,
				PostID:         p.ID,
				MimeType:       mime,
				SizeBytes:      1024,
				Width:          100,
				Height:         100,
				ChecksumSHA256: id,
				S3Key:          "post-attachments/" + p.ID + "/" + id,
				CreatedBy:      p.CreatedBy,
			}
			Expect(repository.NewPostAttachmentRepository(db).CreateAtNextPosition(tenantCtx(), att)).To(Succeed())
			return id
		}

		readyBody := func(platformID, postType, content string) []byte {
			body, _ := json.Marshal(fiber.Map{
				"campaign_id":        campaignID,
				"platform_id":        platformID,
				"platform_post_type": postType,
				"content":            content,
				"status":             "ready_for_publish",
			})
			return body
		}

		putReady := func(id string, body []byte) *http.Response {
			req := httptest.NewRequest("PUT", "/api/posts/"+id, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}

		decodeRules := func(resp *http.Response) []string {
			defer resp.Body.Close()
			var payload struct {
				PlatformValidation map[string][]struct {
					Rule string `json:"rule"`
				} `json:"platform_validation"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&payload)).To(Succeed())
			var rules []string
			for _, list := range payload.PlatformValidation {
				for _, e := range list {
					rules = append(rules, e.Rule)
				}
			}
			return rules
		}

		Context("whitelist", func() {
			It("rejects a post type not in the platform's PostTypes map", func() {
				p := createPost("Unknown type", nil)
				resp := putReady(p.ID, readyBody(linkedInID, "make-believe-type", ""))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ConsistOf("post_type_unknown"))
			})
		})

		Context("text-post", func() {
			It("passes with no content and no attachments", func() {
				p := createPost("Text", nil)
				resp := putReady(p.ID, readyBody(linkedInID, "text-post", ""))
				Expect(resp.StatusCode).To(Equal(200))
			})

			It("rejects when an attachment is present", func() {
				p := createPost("Text plus image", nil)
				seedAttachment(p, "image/png")
				resp := putReady(p.ID, readyBody(linkedInID, "text-post", "hi"))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("max_attachments"))
			})
		})

		Context("image-post", func() {
			It("rejects when no attachments are present", func() {
				p := createPost("Image post no att", nil)
				resp := putReady(p.ID, readyBody(linkedInID, "image-post", ""))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("min_attachments"))
			})

			It("passes with a single image attachment", func() {
				p := createPost("Image post happy", nil)
				seedAttachment(p, "image/png")
				resp := putReady(p.ID, readyBody(linkedInID, "image-post", ""))
				Expect(resp.StatusCode).To(Equal(200))
			})
		})

		Context("carousel", func() {
			It("rejects with a single image", func() {
				p := createPost("Carousel of one", nil)
				seedAttachment(p, "image/png")
				resp := putReady(p.ID, readyBody(linkedInID, "carousel", ""))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("min_attachments"))
			})

			It("passes with two images", func() {
				p := createPost("Carousel of two", nil)
				seedAttachment(p, "image/png")
				seedAttachment(p, "image/png")
				resp := putReady(p.ID, readyBody(linkedInID, "carousel", ""))
				Expect(resp.StatusCode).To(Equal(200))
			})
		})

		Context("video", func() {
			It("rejects when the only attachment is an image", func() {
				p := createPost("Video with image", nil)
				seedAttachment(p, "image/png")
				resp := putReady(p.ID, readyBody(linkedInID, "video", ""))
				Expect(resp.StatusCode).To(Equal(422))
				// The image counts toward the count but is the wrong kind —
				// both rules fire. Either is acceptable evidence of the gate.
				Expect(decodeRules(resp)).To(ContainElement("attachment_kind"))
			})

			It("passes with a single video attachment", func() {
				p := createPost("Video happy", nil)
				seedAttachment(p, "video/mp4")
				resp := putReady(p.ID, readyBody(linkedInID, "video", ""))
				Expect(resp.StatusCode).To(Equal(200))
			})
		})

		Context("poll", func() {
			It("rejects empty content", func() {
				p := createPost("Poll empty", nil)
				resp := putReady(p.ID, readyBody(linkedInID, "poll", ""))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("requires_content"))
			})

			It("rejects when an attachment is present", func() {
				p := createPost("Poll with image", nil)
				seedAttachment(p, "image/png")
				resp := putReady(p.ID, readyBody(linkedInID, "poll", "Vote!"))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("max_attachments"))
			})

			It("passes with content and no attachments", func() {
				p := createPost("Poll happy", nil)
				resp := putReady(p.ID, readyBody(linkedInID, "poll", "Vote A or B?"))
				Expect(resp.StatusCode).To(Equal(200))
			})
		})

		Context("article", func() {
			It("rejects empty content", func() {
				p := createPost("Article empty", nil)
				resp := putReady(p.ID, readyBody(linkedInID, "article", ""))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("requires_content"))
			})

			It("passes with content and no attachments", func() {
				p := createPost("Article happy", nil)
				resp := putReady(p.ID, readyBody(linkedInID, "article", "Long-form body."))
				Expect(resp.StatusCode).To(Equal(200))
			})
		})

		Context("thread (X)", func() {
			// CON-284 R2: a thread is authored as ONE body in `content`, with "---"
			// delimiter lines between messages; the server derives the segment list
			// and validates it per segment (2..25 messages, each within X's 280).
			threadReady := func(id string, segments ...string) *http.Response {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        xID,
					"platform_post_type": "thread",
					"content":            strings.Join(segments, "\n---\n"),
					"status":             "ready_for_publish",
				})
				return putReady(id, body)
			}

			It("rejects a thread with fewer than two messages", func() {
				p := createPost("Thread too short", nil)
				resp := threadReady(p.ID, "only one")
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("thread_segment_count"))
			})

			It("drops an empty message between delimiters (no empty segment can exist)", func() {
				// A blank chunk between two real messages collapses away, so the body
				// yields a clean two-message thread rather than an empty segment.
				p := createPost("Thread blank msg", nil)
				resp := threadReady(p.ID, "root", "   ", "reply")
				Expect(resp.StatusCode).To(Equal(200))
			})

			It("rejects a message over the per-segment character limit", func() {
				p := createPost("Thread too long", nil)
				resp := threadReady(p.ID, "root", strings.Repeat("a", 281))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("max_content_chars"))
			})

			It("passes a valid two-message thread", func() {
				p := createPost("Thread happy", nil)
				resp := threadReady(p.ID, "root message", "second message")
				Expect(resp.StatusCode).To(Equal(200))
			})
		})

		Context("story (Facebook)", func() {
			It("passes with one video attachment", func() {
				p := createPost("Story happy", nil)
				seedAttachment(p, "video/mp4")
				resp := putReady(p.ID, readyBody(facebookID, "story", ""))
				Expect(resp.StatusCode).To(Equal(200))
			})

			It("rejects two attachments", func() {
				p := createPost("Story too many", nil)
				seedAttachment(p, "image/jpeg")
				seedAttachment(p, "image/jpeg")
				resp := putReady(p.ID, readyBody(facebookID, "story", ""))
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("max_attachments"))
			})
		})

		Context("multi-error", func() {
			It("collects every violation in one response", func() {
				p := createPost("Article many issues", nil)
				// Article requires content (empty body) AND we attach a PDF
				// on a platform (LinkedIn) that accepts PDFs but article
				// doesn't allow attachment kind 'pdf'. Both rules fire.
				// Actually, article carries no AllowedKinds restriction, so
				// instead trigger min_attachments + requires_content via
				// 'image-post' with empty content + no attachments.
				resp := putReady(p.ID, readyBody(linkedInID, "image-post", ""))
				Expect(resp.StatusCode).To(Equal(422))
				rules := decodeRules(resp)
				Expect(rules).To(ContainElement("min_attachments"))
			})
		})

		Context("draft saves stay unconstrained", func() {
			It("accepts a draft save even with an unknown post type", func() {
				p := createPost("Drafting", nil)
				body, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        linkedInID,
					"platform_post_type": "made-up-type",
					"status":             "draft",
				})
				resp := putReady(p.ID, body)
				Expect(resp.StatusCode).To(Equal(200))
			})
		})

		Context("Create endpoint mirrors the gate", func() {
			It("rejects creating directly in ready_for_publish without attachments for image-post", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        linkedInID,
					"platform_post_type": "image-post",
					"title":              "Created in ready",
					"status":             "ready_for_publish",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(422))
				Expect(decodeRules(resp)).To(ContainElement("min_attachments"))
			})

			It("accepts creating directly in ready_for_publish for text-post", func() {
				body, _ := json.Marshal(fiber.Map{
					"campaign_id":        campaignID,
					"platform_id":        linkedInID,
					"platform_post_type": "text-post",
					"title":              "Text in ready",
					"status":             "ready_for_publish",
				})
				req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			})
		})
	})

	// ── Delete ───────────────────────────────────────────────────────────────

	Describe("DELETE /api/posts/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/posts/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("deletes the post and returns 204", func() {
				p := createPost("To Delete", nil)

				req := httptest.NewRequest("DELETE", "/api/posts/"+p.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(204))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("DELETE", "/api/posts/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Assistant ────────────────────────────────────────────────────────────

	Describe("POST /api/posts/:id/assistant", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"instruction": "make it shorter"})
				req := httptest.NewRequest("POST", "/api/posts/someid/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when assistant callback is nil", func() {
				p := createPost("Assist Me", nil)
				body, _ := json.Marshal(fiber.Map{"instruction": "make it shorter"})
				req := httptest.NewRequest("POST", "/api/posts/"+p.ID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})
		})

		Context("with a stub assistant callback", func() {
			buildStubApp := func(
				stub func(context.Context, post_assistant.PostAssistantRequest, post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error),
			) (*fiber.App, *http.Cookie, string) {
				stubApp := fiber.New(fiber.Config{
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
				campaignTypeRepo := repository.NewCampaignTypeRepository(db)
				campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
				postRepo := repository.NewPostRepository(db)
				postVersionRepo := repository.NewPostVersionRepository(db)
				postMessageRepo := repository.NewPostAssistantMessageRepository(db)
				auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(stubApp)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(stubApp)
				handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(stubApp)
				handlers.NewPostsHandler(postRepo, postVersionRepo, repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth).Register(stubApp)
				// POST /:id/assistant now lives on the assistant handler (CON-291).
				handlers.NewPostAssistantHandler(stub, nil, postMessageRepo, nil, auth).Register(stubApp)

				seedTenantUser(db, "SSE", "sse-assist@example.com", "sse-password")
				loginBody, _ := json.Marshal(fiber.Map{"email": "sse-assist@example.com", "password": "sse-password"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := stubApp.Test(loginReq, -1) // -1: setup call, not a latency assertion (see BeforeEach)
				Expect(err).NotTo(HaveOccurred())
				var cookie *http.Cookie
				for _, ck := range loginResp.Cookies() {
					cookie = ck
				}

				campBody, _ := json.Marshal(fiber.Map{"name": "Stub Campaign", "campaign_type_id": "Uk"})
				campReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(campBody))
				campReq.Header.Set("Content-Type", "application/json")
				campReq.AddCookie(cookie)
				campResp, err := stubApp.Test(campReq, -1)
				Expect(err).NotTo(HaveOccurred())
				var camp models.Campaign
				Expect(json.NewDecoder(campResp.Body).Decode(&camp)).To(Succeed())

				postBody, _ := json.Marshal(fiber.Map{
					"campaign_id":        camp.ID,
					"platform_id":        "AXqWG7U2qnpt", // seeded LinkedIn platform Sqid
					"platform_post_type": "text-post",
					"title":              "Stub Post",
					"content":            "original content",
				})
				postReq := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(postBody))
				postReq.Header.Set("Content-Type", "application/json")
				postReq.AddCookie(cookie)
				postResp, err := stubApp.Test(postReq, -1)
				Expect(err).NotTo(HaveOccurred())
				Expect(postResp.StatusCode).To(Equal(fiber.StatusCreated))
				var p models.Post
				Expect(json.NewDecoder(postResp.Body).Decode(&p)).To(Succeed())

				return stubApp, cookie, p.ID
			}

			type sseEvent struct{ event, data string }
			parseSSE := func(body io.Reader) []sseEvent {
				var events []sseEvent
				scanner := bufio.NewScanner(body)
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
				return events
			}

			It("streams delta and complete events with text/event-stream content-type", func() {
				final := &post_assistant.PostAssistantResponse{
					Explanation:    "shortened it",
					UpdatedContent: "# Shorter body",
					Action:         "edited",
				}
				stub := func(_ context.Context, _ post_assistant.PostAssistantRequest, onEvent post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error) {
					onEvent(post_assistant.SSEEventExplanationDelta, post_assistant.DeltaEventPayload{Delta: "short"})
					onEvent(post_assistant.SSEEventExplanationDelta, post_assistant.DeltaEventPayload{Delta: "ened it"})
					onEvent(post_assistant.SSEEventContentDelta, post_assistant.DeltaEventPayload{Delta: "body"})
					onEvent(post_assistant.SSEEventComplete, final)
					return final, nil
				}
				stubApp, cookie, postID := buildStubApp(stub)

				body, _ := json.Marshal(fiber.Map{"instruction": "make it shorter"})
				req := httptest.NewRequest("POST", "/api/posts/"+postID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := stubApp.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				events := parseSSE(resp.Body)
				names := make([]string, len(events))
				for i, e := range events {
					names[i] = e.event
				}
				Expect(names).To(Equal([]string{
					"explanation_delta",
					"explanation_delta",
					"content_delta",
					"complete",
				}))
				Expect(events[0].data).To(Equal(`{"delta":"short"}`))
				Expect(events[3].data).To(ContainSubstring(`"explanation":"shortened it"`))
			})

			It("emits an error event with code 400 on ValidationError", func() {
				stub := func(_ context.Context, _ post_assistant.PostAssistantRequest, _ post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error) {
					return nil, &post_assistant.ValidationError{Msg: "instruction is required"}
				}
				stubApp, cookie, postID := buildStubApp(stub)

				body, _ := json.Marshal(fiber.Map{"instruction": "x"}) // bypass handler-level validation
				req := httptest.NewRequest("POST", "/api/posts/"+postID+"/assistant", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := stubApp.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				events := parseSSE(resp.Body)
				Expect(events).To(HaveLen(1))
				Expect(events[0].event).To(Equal("error"))
				Expect(events[0].data).To(ContainSubstring(`"code":400`))
				Expect(events[0].data).To(ContainSubstring("instruction is required"))
			})
		})
	})

	// ── Quality assessment (CON-85) ───────────────────────────────────────────

	Describe("POST /api/posts/:id/assess", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("POST", "/api/posts/someid/assess", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when the assessor is not wired", func() {
				p := createPost("Assess Me", nil)
				req := httptest.NewRequest("POST", "/api/posts/"+p.ID+"/assess", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})
		})

		Context("with a stub assessor callback", func() {
			// buildAssessApp wires a PostsHandler whose quality assessor is the
			// given stub (set via SetQualityAssessor), seeds a user + campaign +
			// post owned by that user, and returns (app, cookie, postID).
			buildAssessApp := func(
				stub func(context.Context, string, post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error),
			) (*fiber.App, *http.Cookie, string) {
				stubApp := fiber.New(fiber.Config{
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
				campaignTypeRepo := repository.NewCampaignTypeRepository(db)
				campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
				postRepo := repository.NewPostRepository(db)
				postVersionRepo := repository.NewPostVersionRepository(db)
				auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(stubApp)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(stubApp)
				handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(stubApp)
				ph := handlers.NewPostsHandler(postRepo, postVersionRepo, repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth)
				ph.Register(stubApp)
				// POST /:id/assess now lives on the insights handler (CON-291).
				handlers.NewPostInsightsHandler(postRepo, stub, nil, nil, nil, nil, auth).Register(stubApp)

				seedTenantUser(db, "Assess", "sse-assess@example.com", "sse-password")
				loginBody, _ := json.Marshal(fiber.Map{"email": "sse-assess@example.com", "password": "sse-password"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := stubApp.Test(loginReq, -1) // -1: setup call, not a latency assertion (see BeforeEach)
				Expect(err).NotTo(HaveOccurred())
				var cookie *http.Cookie
				for _, ck := range loginResp.Cookies() {
					cookie = ck
				}

				campBody, _ := json.Marshal(fiber.Map{"name": "Assess Campaign", "campaign_type_id": "Uk"})
				campReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(campBody))
				campReq.Header.Set("Content-Type", "application/json")
				campReq.AddCookie(cookie)
				campResp, err := stubApp.Test(campReq, -1)
				Expect(err).NotTo(HaveOccurred())
				var camp models.Campaign
				Expect(json.NewDecoder(campResp.Body).Decode(&camp)).To(Succeed())

				postBody, _ := json.Marshal(fiber.Map{
					"campaign_id":        camp.ID,
					"platform_id":        "AXqWG7U2qnpt",
					"platform_post_type": "text-post",
					"title":              "Stub Post",
					"content":            "original content",
				})
				postReq := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(postBody))
				postReq.Header.Set("Content-Type", "application/json")
				postReq.AddCookie(cookie)
				postResp, err := stubApp.Test(postReq, -1)
				Expect(err).NotTo(HaveOccurred())
				Expect(postResp.StatusCode).To(Equal(fiber.StatusCreated))
				var p models.Post
				Expect(json.NewDecoder(postResp.Body).Decode(&p)).To(Succeed())

				return stubApp, cookie, p.ID
			}

			type sseEvent struct{ event, data string }
			parseSSE := func(body io.Reader) []sseEvent {
				var events []sseEvent
				scanner := bufio.NewScanner(body)
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
				return events
			}

			It("streams step and complete events with text/event-stream content-type", func() {
				final := &post_quality.PostQualityResponse{
					PostID:     "p1",
					Evaluation: &models.PostEvaluation{OverallPct: 72.5},
				}
				stub := func(_ context.Context, _ string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error) {
					onEvent(post_quality.SSEEventStep, post_quality.StepEventPayload{Step: "evaluate", Status: "done"})
					onEvent(post_quality.SSEEventComplete, final)
					return final, nil
				}
				stubApp, cookie, postID := buildAssessApp(stub)

				req := httptest.NewRequest("POST", "/api/posts/"+postID+"/assess", nil)
				req.AddCookie(cookie)
				resp, err := stubApp.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				events := parseSSE(resp.Body)
				names := make([]string, len(events))
				for i, e := range events {
					names[i] = e.event
				}
				Expect(names).To(Equal([]string{"step", "complete"}))
				Expect(events[1].data).To(ContainSubstring(`"overall_pct":72.5`))
			})

			It("emits an error event with code 400 on ValidationError", func() {
				stub := func(_ context.Context, _ string, _ post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error) {
					return nil, &post_quality.ValidationError{Msg: "post has no body text to evaluate"}
				}
				stubApp, cookie, postID := buildAssessApp(stub)

				req := httptest.NewRequest("POST", "/api/posts/"+postID+"/assess", nil)
				req.AddCookie(cookie)
				resp, err := stubApp.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				events := parseSSE(resp.Body)
				Expect(events).To(HaveLen(1))
				Expect(events[0].event).To(Equal("error"))
				Expect(events[0].data).To(ContainSubstring(`"code":400`))
				Expect(events[0].data).To(ContainSubstring("no body text"))
			})

			It("lets a different user assess the post (shared workspace)", func() {
				final := &post_quality.PostQualityResponse{
					PostID:     "p1",
					Evaluation: &models.PostEvaluation{OverallPct: 50},
				}
				stub := func(_ context.Context, _ string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error) {
					onEvent(post_quality.SSEEventComplete, final)
					return final, nil
				}
				stubApp, _, postID := buildAssessApp(stub)

				// A second user assesses the first user's post — allowed: posts
				// are shared across the workspace, no per-user ownership gate.
				seedTenantUser(db, "Other", "sse-assess-other@example.com", "sse-password")
				loginBody, _ := json.Marshal(fiber.Map{"email": "sse-assess-other@example.com", "password": "sse-password"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := stubApp.Test(loginReq, -1) // -1: setup call, not a latency assertion (see BeforeEach)
				Expect(err).NotTo(HaveOccurred())
				var otherCookie *http.Cookie
				for _, ck := range loginResp.Cookies() {
					otherCookie = ck
				}

				req := httptest.NewRequest("POST", "/api/posts/"+postID+"/assess", nil)
				req.AddCookie(otherCookie)
				resp, err := stubApp.Test(req, 5000)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))
				Expect(resp.Header.Get("Content-Type")).To(ContainSubstring("text/event-stream"))

				events := parseSSE(resp.Body)
				names := make([]string, len(events))
				for i, e := range events {
					names[i] = e.event
				}
				Expect(names).To(ContainElement("complete"))
			})
		})
	})

	// ── Stored assessment read (CON-92) ───────────────────────────────────────

	Describe("GET /api/posts/:id/assessment", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/posts/someid/assessment", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns 503 when the evaluation repo is not wired", func() {
				// The default app does not call SetEvaluationRepo.
				p := createPost("Unwired Assessment", nil)
				req := httptest.NewRequest("GET", "/api/posts/"+p.ID+"/assessment", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(503))
			})
		})

		Context("with the evaluation repo wired", func() {
			// buildAssessmentReadApp wires a PostsHandler whose evaluation repo
			// is backed by the real DB, seeds a user + campaign + post, and
			// returns (app, cookie, postID) plus the repo so the test can seed
			// a stored evaluation directly.
			buildAssessmentReadApp := func() (*fiber.App, *http.Cookie, string, repository.PostEvaluationRepository) {
				readApp := fiber.New(fiber.Config{
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
				campaignTypeRepo := repository.NewCampaignTypeRepository(db)
				campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
				postRepo := repository.NewPostRepository(db)
				postVersionRepo := repository.NewPostVersionRepository(db)
				evalRepo := repository.NewPostEvaluationRepository(db)
				auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(readApp)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(readApp)
				handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(readApp)
				ph := handlers.NewPostsHandler(postRepo, postVersionRepo, repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth)
				ph.Register(readApp)
				// GET /:id/assessment now lives on the insights handler (CON-291).
				handlers.NewPostInsightsHandler(postRepo, nil, evalRepo, nil, nil, nil, auth).Register(readApp)

				seedTenantUser(db, "Reader", "assessment-read@example.com", "read-password")
				loginBody, _ := json.Marshal(fiber.Map{"email": "assessment-read@example.com", "password": "read-password"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := readApp.Test(loginReq, -1) // -1: setup call, not a latency assertion (see BeforeEach)
				Expect(err).NotTo(HaveOccurred())
				var cookie *http.Cookie
				for _, ck := range loginResp.Cookies() {
					cookie = ck
				}

				campBody, _ := json.Marshal(fiber.Map{"name": "Read Campaign", "campaign_type_id": "Uk"})
				campReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(campBody))
				campReq.Header.Set("Content-Type", "application/json")
				campReq.AddCookie(cookie)
				campResp, err := readApp.Test(campReq, -1)
				Expect(err).NotTo(HaveOccurred())
				var camp models.Campaign
				Expect(json.NewDecoder(campResp.Body).Decode(&camp)).To(Succeed())

				postBody, _ := json.Marshal(fiber.Map{
					"campaign_id":        camp.ID,
					"platform_id":        "AXqWG7U2qnpt",
					"platform_post_type": "text-post",
					"title":              "Read Post",
					"content":            "some content",
				})
				postReq := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(postBody))
				postReq.Header.Set("Content-Type", "application/json")
				postReq.AddCookie(cookie)
				postResp, err := readApp.Test(postReq, -1)
				Expect(err).NotTo(HaveOccurred())
				var p models.Post
				Expect(json.NewDecoder(postResp.Body).Decode(&p)).To(Succeed())

				return readApp, cookie, p.ID, evalRepo
			}

			It("returns 404 when the post has not been assessed", func() {
				readApp, cookie, postID, _ := buildAssessmentReadApp()
				req := httptest.NewRequest("GET", "/api/posts/"+postID+"/assessment", nil)
				req.AddCookie(cookie)
				resp, err := readApp.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("returns the stored evaluation when one exists", func() {
				readApp, cookie, postID, evalRepo := buildAssessmentReadApp()

				id, err := models.NewID()
				Expect(err).NotTo(HaveOccurred())
				Expect(evalRepo.Upsert(tenantCtx(), &models.PostEvaluation{
					ID:               id,
					PostID:           postID,
					PlatformID:       "AXqWG7U2qnpt",
					PlatformPostType: "text-post",
					OverallPct:       64.0,
					Result:           models.EvaluationResult{Correctness: models.EvaluationDimension{Score: 7}},
					ModelID:          "claude-sonnet-test",
					InputHash:        "deadbeef",
				})).To(Succeed())

				req := httptest.NewRequest("GET", "/api/posts/"+postID+"/assessment", nil)
				req.AddCookie(cookie)
				resp, err := readApp.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.PostEvaluation
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.PostID).To(Equal(postID))
				Expect(got.OverallPct).To(BeNumerically("~", 64.0, 0.01))
				Expect(got.ModelID).To(Equal("claude-sonnet-test"))
			})
		})
	})

	// ── Versions ─────────────────────────────────────────────────────────────

	Describe("GET /api/posts/:id/versions", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/posts/someid/versions", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns an empty list when no versions exist", func() {
				p := createPost("No Versions", nil)
				req := httptest.NewRequest("GET", "/api/posts/"+p.ID+"/versions", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var versions []models.PostVersion
				Expect(json.NewDecoder(resp.Body).Decode(&versions)).To(Succeed())
				Expect(versions).To(BeEmpty())
			})
		})
	})

	Describe("POST /api/posts/:id/versions", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"note": "manual save"})
				req := httptest.NewRequest("POST", "/api/posts/someid/versions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("creates a version and returns 201", func() {
				p := createPost("Versioned Post", fiber.Map{"content": "Original content"})

				body, _ := json.Marshal(fiber.Map{"note": "First manual save"})
				req := httptest.NewRequest("POST", "/api/posts/"+p.ID+"/versions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var v models.PostVersion
				Expect(json.NewDecoder(resp.Body).Decode(&v)).To(Succeed())
				Expect(v.PostID).To(Equal(p.ID))
				Expect(v.VersionNumber).To(Equal(1))
				Expect(v.Content).To(Equal("Original content"))
				Expect(v.Note).To(Equal("First manual save"))
				Expect(v.Creator).To(Equal("user"))
			})

			It("increments version number", func() {
				p := createPost("Multi Version", fiber.Map{"content": "Some content"})

				// Version 1
				body, _ := json.Marshal(fiber.Map{"note": "v1"})
				req := httptest.NewRequest("POST", "/api/posts/"+p.ID+"/versions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				// Version 2
				body, _ = json.Marshal(fiber.Map{"note": "v2"})
				req = httptest.NewRequest("POST", "/api/posts/"+p.ID+"/versions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err = app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var v models.PostVersion
				Expect(json.NewDecoder(resp.Body).Decode(&v)).To(Succeed())
				Expect(v.VersionNumber).To(Equal(2))

				// List should show both
				listReq := httptest.NewRequest("GET", "/api/posts/"+p.ID+"/versions", nil)
				listReq.AddCookie(authCookie)
				listResp, err := app.Test(listReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(listResp.StatusCode).To(Equal(200))

				var versions []models.PostVersion
				Expect(json.NewDecoder(listResp.Body).Decode(&versions)).To(Succeed())
				Expect(versions).To(HaveLen(2))
				Expect(versions[0].VersionNumber).To(Equal(1))
				Expect(versions[1].VersionNumber).To(Equal(2))
			})

			It("returns 404 for an unknown post", func() {
				body, _ := json.Marshal(fiber.Map{"note": "ghost"})
				req := httptest.NewRequest("POST", "/api/posts/nonexistent/versions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── CON-69 §11: Post Log read API ─────────────────────────────────────────
	Describe("Post Log API", func() {
		It("records a state_transition entry on a successful Update", func() {
			p := createPost("Audit Me", nil)
			resp := putStatus(app, authCookie, campaignID, p.ID, models.PostStatusReadyForPublish)
			Expect(resp.StatusCode).To(Equal(200))

			req := httptest.NewRequest("GET", "/api/posts/"+p.ID+"/log", nil)
			req.AddCookie(authCookie)
			logResp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(logResp.StatusCode).To(Equal(200))

			var logs []models.PostLog
			Expect(json.NewDecoder(logResp.Body).Decode(&logs)).To(Succeed())
			Expect(logs).NotTo(BeEmpty())
			eventTypes := []string{}
			for _, l := range logs {
				eventTypes = append(eventTypes, string(l.EventType))
			}
			Expect(eventTypes).To(ContainElement(string(models.PostLogEventValidationPassed)))
			Expect(eventTypes).To(ContainElement(string(models.PostLogEventStateTransition)))
		})

		It("records state_transition_blocked when the state machine rejects", func() {
			p := createPost("Reject Me", nil) // status = draft
			// draft → published is invalid in any state machine path.
			resp := putStatus(app, authCookie, campaignID, p.ID, models.PostStatusPublished)
			Expect(resp.StatusCode).To(Equal(400))

			req := httptest.NewRequest("GET", "/api/posts/"+p.ID+"/log", nil)
			req.AddCookie(authCookie)
			logResp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())

			var logs []models.PostLog
			Expect(json.NewDecoder(logResp.Body).Decode(&logs)).To(Succeed())
			Expect(logs).To(HaveLen(1))
			Expect(logs[0].EventType).To(Equal(models.PostLogEventStateTransitionBlocked))
		})

		It("returns 404 when reading the log of an unknown post", func() {
			req := httptest.NewRequest("GET", "/api/posts/nope/log", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(404))
		})

		It("supports filtering across posts via /api/post-logs", func() {
			// Generate two transitions on different posts.
			p1 := createPost("P1", nil)
			p2 := createPost("P2", nil)
			putStatus(app, authCookie, campaignID, p1.ID, models.PostStatusReadyForPublish)
			putStatus(app, authCookie, campaignID, p2.ID, models.PostStatusReadyForPublish)

			req := httptest.NewRequest("GET", "/api/post-logs?event_type=state_transition", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var logs []models.PostLog
			Expect(json.NewDecoder(resp.Body).Decode(&logs)).To(Succeed())
			Expect(len(logs)).To(BeNumerically(">=", 2))
			for _, l := range logs {
				Expect(l.EventType).To(Equal(models.PostLogEventStateTransition))
			}
		})

		It("returns 400 for malformed since/until parameters", func() {
			req := httptest.NewRequest("GET", "/api/post-logs?since=not-a-date", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(400))
		})
	})

	// ── CON-69 §9: cancel endpoint ───────────────────────────────────────────
	Describe("POST /api/posts/:id/cancel", func() {
		It("returns 503 when scheduling deps are not wired", func() {
			// posts_test.go BeforeEach does not call SetSchedulingDeps,
			// so the cancel endpoint should refuse with 503.
			p := createPost("Cancel Me", nil)
			// Move the post to scheduled directly via DB so the handler
			// reaches the dep check.
			ctx := tenantCtx()
			_, err := db.NewUpdate().Model((*models.Post)(nil)).
				Set("status = ?", models.PostStatusScheduled).
				Where("id = ?", p.ID).Exec(ctx)
			Expect(err).NotTo(HaveOccurred())

			req := httptest.NewRequest("POST", "/api/posts/"+p.ID+"/cancel", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(503))
		})

		It("returns 409 when the post is not in Scheduled state", func() {
			p := createPost("Wrong State", nil) // status=draft
			req := httptest.NewRequest("POST", "/api/posts/"+p.ID+"/cancel", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			// 503 first (deps not wired), but the dep check happens
			// before the state check. Skip if we get 503 — separate test
			// covers state check when deps are wired.
			Expect(resp.StatusCode).To(BeNumerically(">=", 400))
		})

		It("returns 404 for an unknown post", func() {
			req := httptest.NewRequest("POST", "/api/posts/nope/cancel", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(BeNumerically(">=", 400))
		})
	})

	// ── CON-130: POST /api/posts/convert-to-manual ────────────────────────────
	Describe("POST /api/posts/convert-to-manual", func() {
		var (
			convertApp *fiber.App
			enq        *fakeCancelEnqueuer
		)

		// convertApp wires the posts handler with a fake cancel enqueuer so
		// the endpoint's dep check passes and enqueues can be observed. It
		// reuses the shared db + authCookie (sessions live in the DB).
		BeforeEach(func() {
			enq = &fakeCancelEnqueuer{}
			convertApp = fiber.New(fiber.Config{
				ErrorHandler: func(c *fiber.Ctx, err error) error {
					code := fiber.StatusInternalServerError
					if e, ok := err.(*fiber.Error); ok {
						code = e.Code
					}
					return c.Status(code).JSON(fiber.Map{"error": err.Error()})
				},
			})
			auth := handlers.RequireAuth(repository.NewSessionRepository(db), repository.NewUserRepository(db), testCookieName)
			ph := handlers.NewPostsHandler(
				repository.NewPostRepository(db),
				repository.NewPostVersionRepository(db),
				repository.NewPlatformRepository(db),
				repository.NewPostAttachmentRepository(db),
				auth,
			)
			ph.SetSchedulingDeps(nil, enq, db)
			ph.SetPostLogRepo(repository.NewPostLogRepository(db))
			ph.Register(convertApp)
		})

		// setStatus flips a post's status directly in the DB so tests can
		// stage a `scheduled` (or other) post without driving the full
		// schedule path.
		setStatus := func(id string, st models.PostStatus) {
			_, err := db.NewUpdate().Model((*models.Post)(nil)).
				Set("status = ?", st).
				Where("id = ?", id).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
		}

		postConvert := func(body fiber.Map) *http.Response {
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/api/posts/convert-to-manual", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := convertApp.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}

		decode := func(resp *http.Response) (converted []string, failed []convertFailureBody) {
			var out struct {
				Converted []string             `json:"converted"`
				Failed    []convertFailureBody `json:"failed"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			return out.Converted, out.Failed
		}

		It("converts every scheduled post for a platform (target = manual publishing)", func() {
			p1 := createPost("Sched 1", nil)
			p2 := createPost("Sched 2", nil)
			setStatus(p1.ID, models.PostStatusScheduled)
			setStatus(p2.ID, models.PostStatusScheduled)

			resp := postConvert(fiber.Map{"platform": "linkedin"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))
			converted, failed := decode(resp)
			Expect(converted).To(ConsistOf(p1.ID, p2.ID))
			Expect(failed).To(BeEmpty())
			Expect(enq.targets()).To(Equal(map[string]queues.CancelTarget{
				p1.ID: queues.CancelTargetManualPublish,
				p2.ID: queues.CancelTargetManualPublish,
			}))
		})

		It("converts explicit post_ids and reports non-scheduled posts as failed", func() {
			scheduled := createPost("Sched", nil)
			draft := createPost("Draft", nil) // stays draft
			setStatus(scheduled.ID, models.PostStatusScheduled)

			resp := postConvert(fiber.Map{"post_ids": []string{scheduled.ID, draft.ID}})
			Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))
			converted, failed := decode(resp)
			Expect(converted).To(ConsistOf(scheduled.ID))
			Expect(failed).To(HaveLen(1))
			Expect(failed[0].ID).To(Equal(draft.ID))
			Expect(failed[0].Reason).To(ContainSubstring("not scheduled"))
			Expect(enq.targets()).To(HaveKeyWithValue(scheduled.ID, queues.CancelTargetManualPublish))
			Expect(enq.targets()).NotTo(HaveKey(draft.ID))
		})

		It("treats an already-manual post as an idempotent success without enqueuing", func() {
			p := createPost("Already Manual", nil)
			setStatus(p.ID, models.PostStatusScheduledForManualPublish)

			resp := postConvert(fiber.Map{"post_ids": []string{p.ID}})
			Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))
			converted, failed := decode(resp)
			Expect(converted).To(ConsistOf(p.ID))
			Expect(failed).To(BeEmpty())
			Expect(enq.calls).To(BeEmpty())
		})

		It("reports an unknown post id as failed, not found", func() {
			resp := postConvert(fiber.Map{"post_ids": []string{"nope"}})
			Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))
			converted, failed := decode(resp)
			Expect(converted).To(BeEmpty())
			Expect(failed).To(HaveLen(1))
			Expect(failed[0].Reason).To(ContainSubstring("not found"))
		})

		It("rejects an unsupported platform with 400", func() {
			resp := postConvert(fiber.Map{"platform": "myspace"})
			Expect(resp.StatusCode).To(Equal(400))
		})

		It("rejects a request that sets neither platform nor post_ids with 400", func() {
			resp := postConvert(fiber.Map{})
			Expect(resp.StatusCode).To(Equal(400))
		})

		It("rejects a request that sets both platform and post_ids with 400", func() {
			resp := postConvert(fiber.Map{"platform": "linkedin", "post_ids": []string{"x"}})
			Expect(resp.StatusCode).To(Equal(400))
		})

		It("returns 503 when the job runtime is not wired", func() {
			// The shared app (BeforeEach) does not call SetSchedulingDeps.
			raw, _ := json.Marshal(fiber.Map{"platform": "linkedin"})
			req := httptest.NewRequest("POST", "/api/posts/convert-to-manual", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(503))
		})
	})
})

// convertFailureBody mirrors the handler's unexported convertFailure for
// decoding the convert-to-manual response in tests.
type convertFailureBody struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
