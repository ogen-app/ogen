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

// A post's first comment: authored presence-aware on the post, checked by the
// publish gate against the platform's limit, locked once submitted, and its
// outcome owned by the publish workers.
var _ = Describe("Post first comment", Ordered, func() {
	const (
		linkedinID = "AXqWG7U2qnpt"
		xID        = "81mUCmc2xsKd"
	)
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

		campaignID = "camp-1"
		Expect(campaignRepo.Create(tenantCtx(), &models.Campaign{
			ID: campaignID, Name: "C", CampaignTypeID: seededCampaignTypeID,
			AssetIDs: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{},
			TagIDs: models.StringSlice{}, PublishingDays: models.StringSlice{},
			Status: models.StatusDraft, PublishingTime: "09:00", SpreadMinutes: 15,
			GoalCadence: "month", CreatedBy: user.ID,
		})).To(Succeed())

		postID = "post-1"
		Expect(postRepo.Create(tenantCtx(), &models.Post{
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
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
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

	put := func(extra fiber.Map) *http.Response {
		GinkgoHelper()
		body := fiber.Map{"campaign_id": campaignID, "content": "hi"}
		maps.Copy(body, extra)
		return do("PUT", "/api/posts/"+postID, body)
	}

	It("round-trips the comment, keeps it when omitted and 400s an off-menu delay", func() {
		resp := put(fiber.Map{"first_comment": "  Link: https://x.y  ", "first_comment_delay_minutes": 3})
		Expect(resp.StatusCode).To(Equal(200))
		got := decode(resp)
		Expect(got.FirstComment).To(Equal("Link: https://x.y"))
		Expect(got.FirstCommentDelayMinutes).To(Equal(3))
		Expect(got.FirstCommentStatus).To(BeNil())

		resp = put(fiber.Map{"content": "edited"})
		Expect(resp.StatusCode).To(Equal(200))
		got = decode(resp)
		Expect(got.FirstComment).To(Equal("Link: https://x.y"))
		Expect(got.FirstCommentDelayMinutes).To(Equal(3))

		Expect(put(fiber.Map{"first_comment_delay_minutes": 7}).StatusCode).To(Equal(400))
		Expect(put(fiber.Map{"first_comment_delay_minutes": nil}).StatusCode).To(Equal(400))

		resp = put(fiber.Map{"first_comment": nil})
		Expect(resp.StatusCode).To(Equal(200))
		Expect(decode(resp).FirstComment).To(BeEmpty())
	})

	It("accepts a first comment on create", func() {
		resp := do("POST", "/api/posts", fiber.Map{"campaign_id": campaignID, "content": "new", "first_comment": "c", "first_comment_delay_minutes": 10})
		Expect(resp.StatusCode).To(Equal(201))
		got := decode(resp)
		Expect(got.FirstComment).To(Equal("c"))
		Expect(got.FirstCommentDelayMinutes).To(Equal(10))
	})

	It("gates the comment on the platform's limit at ready-for-publish", func() {
		ready := fiber.Map{"platform_post_type": "text-post", "status": "ready_for_publish", "first_comment": "Link: https://x.y"}

		ready["platform_id"] = xID
		resp := put(ready)
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnprocessableEntity))
		var body map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body["platform_validation"]).To(HaveKey(xID))
		Expect(body["platform_validation"].(map[string]any)[xID]).To(ContainElement(HaveKeyWithValue("rule", "first_comment_unsupported")))

		// LinkedIn takes a first comment (seeded by the migration).
		ready["platform_id"] = linkedinID
		Expect(put(ready).StatusCode).To(Equal(200))
	})

	Context("on a published post", func() {
		const livePostID = "live-1"
		BeforeEach(func() {
			Expect(postRepo.Create(tenantCtx(), &models.Post{
				ID: livePostID, CampaignID: campaignID,
				PlatformID: linkedinID, PlatformPostType: "text-post",
				Content: "went out", Status: models.PostStatusPublished, CTAType: models.CTATypeNone,
				FirstComment: "link", FirstCommentDelayMinutes: 3,
				FirstCommentStatus: new(models.FirstCommentPending),
				Publisher:          models.PublisherZernio, PublisherPostID: "z-live",
				MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
				CreatedBy: userID,
			})).To(Succeed())
		})

		body := func(extra fiber.Map) fiber.Map {
			m := fiber.Map{
				"campaign_id": campaignID, "platform_id": linkedinID, "platform_post_type": "text-post",
				"content": "went out", "status": "published",
			}
			maps.Copy(m, extra)
			return m
		}

		It("409s a change to the comment but not a restatement", func() {
			Expect(do("PUT", "/api/posts/"+livePostID, body(fiber.Map{"first_comment": "other"})).StatusCode).To(Equal(409))
			Expect(do("PUT", "/api/posts/"+livePostID, body(fiber.Map{"first_comment_delay_minutes": 5})).StatusCode).To(Equal(409))
			Expect(do("PUT", "/api/posts/"+livePostID, body(fiber.Map{"first_comment": "link", "first_comment_delay_minutes": 3})).StatusCode).To(Equal(200))
		})

		It("never writes back the worker-owned outcome, and settles it only once", func() {
			live := &models.Post{ID: livePostID, PublisherPostID: "z-live", FirstCommentStatus: new(models.FirstCommentPosted), FirstCommentID: "c-1"}
			ok, err := postRepo.SettleFirstComment(tenantCtx(), live, "first_comment_status", "first_comment_id")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeTrue())

			// A PUT holding the stale pending copy leaves the outcome alone.
			resp := do("PUT", "/api/posts/"+livePostID, body(fiber.Map{"published_url": "https://li/x"}))
			Expect(resp.StatusCode).To(Equal(200))
			got := decode(resp)
			Expect(got.FirstCommentStatus).To(Equal(new(models.FirstCommentPosted)))
			Expect(got.FirstCommentID).To(Equal("c-1"))

			live.FirstCommentStatus = new(models.FirstCommentFailed)
			ok, err = postRepo.SettleFirstComment(tenantCtx(), live, "first_comment_status")
			Expect(err).NotTo(HaveOccurred())
			Expect(ok).To(BeFalse())
		})
	})
})
