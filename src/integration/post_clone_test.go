//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
)

// threadsPlatformID is the Threads Sqid assigned by
// 20240115000001_fix_platform_ids_to_sqids. Threads accepts text-post,
// image-post, carousel, video, poll, gif-post (but NOT article), which
// lets the cross-platform + fallback cases below exercise real rules.
const threadsPlatformID = "pQ4yxT3SuE57"

var _ = Describe("Post clone — CON-59 (real S3/MinIO)", Ordered, func() {
	var (
		app         *fiber.App
		db          *bun.DB
		store       storage.Storage
		raw         *s3.Client
		bucket      string
		authCookie  *http.Cookie
		campaignID  string
		postRepo    repository.PostRepository
		postAttRepo repository.PostAttachmentRepository
		versionRepo repository.PostVersionRepository
		logRepo     repository.PostLogRepository
	)

	BeforeAll(func() {
		db = mustOpenIntegrationDB()
		store, raw, bucket = mustOpenIntegrationStorage()
	})

	BeforeEach(func() {
		app = fiber.New(fiber.Config{
			BodyLimit: 60 << 20,
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
		postRepo = repository.NewPostRepository(db)
		postAttRepo = repository.NewPostAttachmentRepository(db)
		versionRepo = repository.NewPostVersionRepository(db)
		logRepo = repository.NewPostLogRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, "test_session")

		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, "test_session", false).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(app)

		postsHandler := handlers.NewPostsHandler(postRepo, versionRepo, platformRepo, postAttRepo, auth)
		postsHandler.SetPostLogRepo(logRepo)
		// POST /:id/clone now lives on the actions handler (CON-291).
		handlers.NewPostActionsHandler(postRepo, clone.New(db, postRepo, versionRepo, postAttRepo, platformRepo, logRepo, store, nil), nil, nil, auth).Register(app)
		// Same S3-cleanup-on-delete hook the production server wires —
		// the independence test relies on it.
		postsHandler.SetOnBeforeDelete(func(ctx context.Context, postID string) error {
			keys, err := postAttRepo.ListS3KeysByPostID(ctx, postID)
			if err != nil {
				return err
			}
			for _, k := range keys {
				if k == "" {
					continue
				}
				if err := store.Delete(ctx, k); err != nil {
					return err
				}
			}
			return nil
		})
		postsHandler.Register(app)
		handlers.NewPostAttachmentsHandler(postAttRepo, postRepo, store, fakePDFRenderer{}, nil, httpImagePreparer{}, nil, "gemini-2.5-flash", 280, auth).Register(app)

		// Seed user + session + campaign.
		seedTenantUser(db, "Admin", "clone@example.com", "clone-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "clone@example.com", "password": "clone-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(len(loginResp.Cookies())).To(BeNumerically(">", 0))
		authCookie = loginResp.Cookies()[0]

		cBody, _ := json.Marshal(fiber.Map{"name": "Clone Campaign", "campaign_type_id": "Uk"})
		cReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(cBody))
		cReq.Header.Set("Content-Type", "application/json")
		cReq.AddCookie(authCookie)
		cResp, err := app.Test(cReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(cResp.StatusCode).To(Equal(fiber.StatusCreated))
		var camp models.Campaign
		Expect(json.NewDecoder(cResp.Body).Decode(&camp)).To(Succeed())
		campaignID = camp.ID
	})

	AfterEach(func() {
		ctx := tenantCtx()
		for _, t := range []string{"post_attachments", "post_versions", "post_logs", "post_assistant_messages", "posts", "campaigns", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(t).Where("1 = 1").Exec(ctx)
		}
	})

	// createPost makes a source post with the given platform/type/content.
	createPost := func(platformID, postType, content string) string {
		body, _ := json.Marshal(fiber.Map{
			"campaign_id":        campaignID,
			"platform_id":        platformID,
			"platform_post_type": postType,
			"title":              "Source Post",
			"content":            content,
			"cta_type":           "none",
		})
		req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var p models.Post
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		return p.ID
	}

	clone := func(sourceID string, body any) (*http.Response, models.Post) {
		var r *http.Request
		if body == nil {
			r = httptest.NewRequest("POST", "/api/posts/"+sourceID+"/clone", nil)
		} else {
			b, _ := json.Marshal(body)
			r = httptest.NewRequest("POST", "/api/posts/"+sourceID+"/clone", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
		}
		r.AddCookie(authCookie)
		resp, err := app.Test(r)
		Expect(err).NotTo(HaveOccurred())
		var p models.Post
		if resp.StatusCode == fiber.StatusCreated {
			Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		}
		return resp, p
	}

	It("#1 same-platform clone (empty body) duplicates verbatim into a new draft", func() {
		srcID := createPost(linkedinPlatformID, "text-post", "# Hello\n\nOriginal body.")
		resp, c := clone(srcID, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

		Expect(c.ID).NotTo(Equal(srcID))
		Expect(c.Status).To(Equal(models.PostStatusDraft))
		Expect(c.CampaignID).To(Equal(campaignID))
		Expect(c.PlatformID).To(Equal(linkedinPlatformID))
		Expect(c.PlatformPostType).To(Equal("text-post"))
		Expect(c.Content).To(Equal("# Hello\n\nOriginal body."))
		Expect(c.ClonedFromPostID).NotTo(BeNil())
		Expect(*c.ClonedFromPostID).To(Equal(srcID))

		// Source is untouched.
		src, err := postRepo.GetByID(tenantCtx(), srcID)
		Expect(err).NotTo(HaveOccurred())
		Expect(src.Status).To(Equal(models.PostStatusDraft))
		Expect(src.ClonedFromPostID).To(BeNil())

		// A v1 snapshot exists with the cloned content.
		versions, err := versionRepo.ListByPostID(tenantCtx(), c.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(versions).To(HaveLen(1))
		Expect(versions[0].VersionNumber).To(Equal(1))
		Expect(versions[0].Content).To(Equal("# Hello\n\nOriginal body."))

		// An audit entry was recorded on the clone.
		logs, err := logRepo.ListByPostID(tenantCtx(), c.ID, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainElement(WithTransform(
			func(l models.PostLog) models.PostLogEventType { return l.EventType },
			Equal(models.PostLogEventPostCloned),
		)))
	})

	It("#2 deep-copies attachments to independent S3 keys", func() {
		srcID := createPost(linkedinPlatformID, "image-post", "with image")

		// Upload an attachment to the source.
		mbody, ct := multipartBody("file", "image.png", "image/png", makePNG(8))
		ureq := httptest.NewRequest("POST", "/api/posts/"+srcID+"/attachments", mbody)
		ureq.Header.Set("Content-Type", ct)
		ureq.AddCookie(authCookie)
		uresp, err := app.Test(ureq, 60000)
		Expect(err).NotTo(HaveOccurred())
		Expect(uresp.StatusCode).To(Equal(fiber.StatusCreated))

		srcAtts, err := postAttRepo.ListByPostID(tenantCtx(), srcID)
		Expect(err).NotTo(HaveOccurred())
		Expect(srcAtts).To(HaveLen(1))
		srcKey := srcAtts[0].S3Key

		resp, c := clone(srcID, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

		cloneAtts, err := postAttRepo.ListByPostID(tenantCtx(), c.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cloneAtts).To(HaveLen(1))
		Expect(cloneAtts[0].S3Key).NotTo(Equal(srcKey))
		Expect(cloneAtts[0].PostID).To(Equal(c.ID))

		ctx := tenantCtx()
		Expect(objectExists(ctx, raw, bucket, srcKey)).To(BeTrue())
		Expect(objectExists(ctx, raw, bucket, cloneAtts[0].S3Key)).To(BeTrue())

		// Deleting the source must NOT remove the clone's blob.
		dreq := httptest.NewRequest("DELETE", "/api/posts/"+srcID, nil)
		dreq.AddCookie(authCookie)
		dresp, err := app.Test(dreq)
		Expect(err).NotTo(HaveOccurred())
		Expect(dresp.StatusCode).To(Equal(fiber.StatusNoContent))

		Expect(objectExists(ctx, raw, bucket, srcKey)).To(BeFalse())
		Expect(objectExists(ctx, raw, bucket, cloneAtts[0].S3Key)).To(BeTrue())
	})

	It("#3 cross-platform clone retargets the platform and suffixes the title", func() {
		srcID := createPost(linkedinPlatformID, "text-post", "cross body")
		resp, c := clone(srcID, fiber.Map{"target_platform_id": threadsPlatformID})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

		Expect(c.PlatformID).To(Equal(threadsPlatformID))
		Expect(c.PlatformPostType).To(Equal("text-post")) // valid on Threads
		Expect(c.Content).To(Equal("cross body"))         // REST path is verbatim
		Expect(c.Title).To(Equal("Source Post (Threads)"))
	})

	It("#4 falls back to the platform default when the source post-type is invalid on the target", func() {
		// 'article' exists on LinkedIn but not on Threads → falls back to text-post.
		srcID := createPost(linkedinPlatformID, "article", "long form")
		resp, c := clone(srcID, fiber.Map{"target_platform_id": threadsPlatformID})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(c.PlatformID).To(Equal(threadsPlatformID))
		Expect(c.PlatformPostType).To(Equal("text-post"))
	})

	It("#5 returns 404 for an unknown source and 400 for an unknown platform", func() {
		resp, _ := clone("doesnotexist", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))

		srcID := createPost(linkedinPlatformID, "text-post", "x")
		resp2, _ := clone(srcID, fiber.Map{"target_platform_id": "nope-not-real"})
		Expect(resp2.StatusCode).To(Equal(fiber.StatusBadRequest))
	})
})
