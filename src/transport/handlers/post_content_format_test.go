package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
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

// content_format is a nullable, presence-aware field on the post: validated
// against the fixed vocabulary, readable on the post read and list, and part of
// the content lock once a post is submitted.
var _ = Describe("Post content_format", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		postRepo   repository.PostRepository
		campaignID string
		postID     string
		userID     string
	)

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

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
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), campaignTypeRepo)
		postRepo = repository.NewPostRepository(db)

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth, handlers.PostsOptions{}).Register(app)

		user := seedTenantUser(db, "Admin", "admin@example.com", "admin-password")
		userID = user.ID

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]

		ctx := tenantCtx()
		campaignID = "camp-1"
		Expect(campaignRepo.Create(ctx, &models.Campaign{
			ID: campaignID, Name: "C", CampaignTypeID: seededCampaignTypeID,
			AssetIDs: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{},
			TagIDs: models.StringSlice{}, PublishingDays: models.StringSlice{},
			Status: models.StatusDraft, PublishingTime: "09:00", SpreadMinutes: 15,
			GoalCadence: "month", CreatedBy: user.ID,
		})).To(Succeed())

		postID = "post-1"
		Expect(postRepo.Create(ctx, &models.Post{
			ID: postID, CampaignID: campaignID, Content: "hi",
			Status: models.PostStatusDraft, CTAType: models.CTATypeNone,
			MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
			CreatedBy: user.ID,
		})).To(Succeed())
	})

	AfterEach(func() {
		for _, tbl := range []string{"post_logs", "post_versions", "posts", "campaigns", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	do := func(method, path string, body any) *http.Response {
		GinkgoHelper()
		var r *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		} else {
			r = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	decode := func(resp *http.Response) models.Post {
		GinkgoHelper()
		var p models.Post
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		return p
	}

	format := func(f models.ContentFormat) *models.ContentFormat { return &f }

	Describe("PUT /api/posts/:id", func() {
		It("round-trips a format through GET and the list", func() {
			resp := do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "hi", "content_format": "how-to"})
			Expect(resp.StatusCode).To(Equal(200))
			Expect(decode(resp).ContentFormat).To(Equal(format(models.ContentFormatHowTo)))

			resp = do("GET", "/api/posts/"+postID, nil)
			Expect(resp.StatusCode).To(Equal(200))
			Expect(decode(resp).ContentFormat).To(Equal(format(models.ContentFormatHowTo)))

			resp = do("GET", "/api/posts", nil)
			Expect(resp.StatusCode).To(Equal(200))
			var list []models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&list)).To(Succeed())
			Expect(list).To(HaveLen(1))
			Expect(list[0].ContentFormat).To(Equal(format(models.ContentFormatHowTo)))
		})

		It("serialises a post without a format as null", func() {
			resp := do("GET", "/api/posts/"+postID, nil)
			Expect(resp.StatusCode).To(Equal(200))
			var raw map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
			Expect(raw).To(HaveKeyWithValue("content_format", BeNil()))
		})

		It("leaves the format alone when the key is omitted and clears it on null", func() {
			Expect(do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "hi", "content_format": "story"}).StatusCode).To(Equal(200))

			resp := do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "edited"})
			Expect(resp.StatusCode).To(Equal(200))
			Expect(decode(resp).ContentFormat).To(Equal(format(models.ContentFormatStory)))

			resp = do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "edited", "content_format": nil})
			Expect(resp.StatusCode).To(Equal(200))
			Expect(decode(resp).ContentFormat).To(BeNil())
		})

		It("400s an unknown slug and the empty string, leaving the stored value alone", func() {
			Expect(do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "hi", "content_format": "digest"}).StatusCode).To(Equal(200))

			Expect(do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "hi", "content_format": "guide"}).StatusCode).To(Equal(400))
			Expect(do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "hi", "content_format": ""}).StatusCode).To(Equal(400))

			resp := do("GET", "/api/posts/"+postID, nil)
			Expect(decode(resp).ContentFormat).To(Equal(format(models.ContentFormatDigest)))
		})
	})

	Describe("POST /api/posts", func() {
		It("accepts a format on create and 400s an unknown one", func() {
			resp := do("POST", "/api/posts", fiber.Map{"campaign_id": campaignID, "content": "new", "content_format": "opinion"})
			Expect(resp.StatusCode).To(Equal(201))
			Expect(decode(resp).ContentFormat).To(Equal(format(models.ContentFormatOpinion)))

			Expect(do("POST", "/api/posts", fiber.Map{"campaign_id": campaignID, "content": "new", "content_format": "nope"}).StatusCode).To(Equal(400))
			Expect(do("POST", "/api/posts", fiber.Map{"campaign_id": campaignID, "content": "new", "content_format": ""}).StatusCode).To(Equal(400))
		})
	})

	Describe("content lock on a submitted post", func() {
		for _, status := range []models.PostStatus{models.PostStatusScheduled, models.PostStatusPublished} {
			It("refuses a format change on a "+string(status)+" post with 409", func() {
				lockedID := "locked-" + string(status)
				Expect(postRepo.Create(tenantCtx(), &models.Post{
					ID: lockedID, CampaignID: campaignID,
					PlatformID: "AXqWG7U2qnpt", PlatformPostType: "text-post",
					Content: "went out", Status: status, CTAType: models.CTATypeNone,
					ContentFormat: format(models.ContentFormatExplainer),
					MediaURLs:     models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
					CreatedBy: userID,
				})).To(Succeed())

				body := func(extra fiber.Map) fiber.Map {
					m := fiber.Map{
						"campaign_id": campaignID, "platform_id": "AXqWG7U2qnpt", "platform_post_type": "text-post",
						"content": "went out", "status": string(status),
					}
					maps.Copy(m, extra)
					return m
				}

				Expect(do("PUT", "/api/posts/"+lockedID, body(fiber.Map{"content_format": "listicle"})).StatusCode).To(Equal(409))
				Expect(do("PUT", "/api/posts/"+lockedID, body(fiber.Map{"content_format": nil})).StatusCode).To(Equal(409))

				// Restating the same format, or omitting it, is not a change.
				Expect(do("PUT", "/api/posts/"+lockedID, body(fiber.Map{"content_format": "explainer"})).StatusCode).To(Equal(200))
				resp := do("PUT", "/api/posts/"+lockedID, body(nil))
				Expect(resp.StatusCode).To(Equal(200))
				Expect(decode(resp).ContentFormat).To(Equal(format(models.ContentFormatExplainer)))
			})
		}
	})
})
