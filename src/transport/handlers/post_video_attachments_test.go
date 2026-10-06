package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/grpc/client/video"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// fakeVideoProber stands in for video-service. err, when set, is returned
// from every Probe; otherwise result is.
type fakeVideoProber struct {
	result *video.ProbeResult
	err    error
}

func (f *fakeVideoProber) Probe(context.Context, video.ProbeOptions) (*video.ProbeResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// probedMP4 is a 1080x1920, 6.2s H.264 reel with a poster frame.
func probedMP4() *video.ProbeResult {
	return &video.ProbeResult{
		DurationMs: 6200, Codec: "h264", Container: "mov,mp4,m4a,3gp,3g2,mj2",
		Width: 1080, Height: 1920, PosterPNG: minimalPNG(),
	}
}

// The workspace runs on the seeded Trial tier (media_storage_bytes = 100 MiB);
// usage is stubbed per spec.
var _ = Describe("PostAttachmentsHandler video presign/finalize", Ordered, func() {
	const (
		trialMediaCap       = 100 << 20
		instagramPlatformID = "rzgpTkARLH0L"
	)

	var (
		app          *fiber.App
		db           *bun.DB
		authCookie   *http.Cookie
		store        *stubStorage
		prober       *fakeVideoProber
		hub          eventhub.Hub
		mediaBytes   int64
		campaignID   string
		postID       string
		originalTier string
	)
	ctx := context.Background()

	postJSON := func(path string, body any) *http.Response {
		GinkgoHelper()
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}
	decode := func(resp *http.Response) map[string]any {
		GinkgoHelper()
		var out map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out
	}
	createPost := func(platformID, postType string) string {
		GinkgoHelper()
		resp := postJSON("/api/posts", fiber.Map{
			"campaign_id": campaignID, "platform_id": platformID, "platform_post_type": postType, "title": "Video Post",
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		return decode(resp)["id"].(string)
	}
	presign := func(postID string, body fiber.Map) *http.Response {
		return postJSON("/api/posts/"+postID+"/attachments/presign", body)
	}
	finalize := func(postID string, body fiber.Map) *http.Response {
		return postJSON("/api/posts/"+postID+"/attachments/finalize", body)
	}
	// uploadVideo presigns and "PUTs" n bytes to the returned key.
	uploadVideo := func(postID string, n int) string {
		GinkgoHelper()
		resp := presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": n})
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		key := decode(resp)["s3_key"].(string)
		store.objects[key] = bytes.Repeat([]byte{0}, n)
		return key
	}
	attachmentCount := func() int {
		GinkgoHelper()
		n, err := db.NewSelect().Model((*models.PostAttachment)(nil)).Where("post_id = ?", postID).Count(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
		Expect(db.NewSelect().Model((*models.Tenant)(nil)).Column("tier_id").
			Where("id = ?", models.DefaultTenantID).Scan(ctx, &originalTier)).To(Succeed())
		_, err := db.NewUpdate().Model((*models.Tenant)(nil)).Set("tier_id = ?", "trial").
			Where("id = ?", models.DefaultTenantID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		_, err := db.NewUpdate().Model((*models.Tenant)(nil)).Set("tier_id = ?", originalTier).
			Where("id = ?", models.DefaultTenantID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	BeforeEach(func() {
		mediaBytes = 0
		app = fiber.New(fiber.Config{
			ErrorHandler: func(c *fiber.Ctx, err error) error {
				if qe, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
					return c.Status(fiber.StatusPaymentRequired).JSON(fiber.Map{"error": "entitlement_exceeded", "feature": qe.Key})
				}
				code := fiber.StatusInternalServerError
				if fe, ok := errors.AsType[*fiber.Error](err); ok {
					code = fe.Code
				}
				return c.Status(code).JSON(fiber.Map{"error": err.Error()})
			},
		})
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tagRepo := repository.NewTagRepository(db)
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)
		postRepo := repository.NewPostRepository(db)
		postAttRepo := repository.NewPostAttachmentRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}
		prober = &fakeVideoProber{result: probedMP4()}
		hub = eventhub.New(eventhub.Config{})

		cat, err := entitlements.LoadCatalog()
		Expect(err).NotTo(HaveOccurred())
		resolver := entitlements.NewResolver(repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db), repository.NewTenantRepository(db), cat)
		lim := entitlements.NewLimiter(resolver, cat, entitlements.ModeEnforce).
			Register("media_storage_bytes", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return mediaBytes, nil }))

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), platformRepo, postAttRepo, auth, handlers.PostsOptions{}).Register(app)
		handlers.NewPostAttachmentsHandler(postAttRepo, postRepo, store, fakePDFRenderer{}, prober, &fakeImagePreparer{store: store}, nil, 280, auth, lim).
			WithEventHub(hub).Register(app)

		seedTenantUser(db, "Admin", "video@example.com", "pw-password")
		loginBody, _ := json.Marshal(fiber.Map{"email": "video@example.com", "password": "pw-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.Cookies()).To(HaveLen(1))
		authCookie = loginResp.Cookies()[0]

		resp := postJSON("/api/campaigns", fiber.Map{"name": "Video", "campaign_type_id": "Uk"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		campaignID = decode(resp)["id"].(string)
		postID = createPost(instagramPlatformID, "reel")
	})

	AfterEach(func() {
		for _, tbl := range []string{"post_attachments", "post_versions", "posts", "campaigns", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	Describe("presign", func() {
		It("mints a PUT URL for a key under the post's prefix", func() {
			resp := presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			body := decode(resp)
			key := body["s3_key"].(string)
			Expect(key).To(ContainSubstring("post-attachments/" + postID + "/"))
			Expect(key).To(HaveSuffix(".mp4"))
			Expect(body["upload_url"]).To(ContainSubstring("/put/"))
			Expect(body["expires_in"]).To(BeNumerically("==", 1800))
		})

		It("refuses a non-video content type with 415 unsupported_media_type", func() {
			resp := presign(postID, fiber.Map{"content_type": "image/png", "size_bytes": 1024})
			Expect(resp.StatusCode).To(Equal(fiber.StatusUnsupportedMediaType))
			Expect(decode(resp)).To(HaveKeyWithValue("code", models.UploadCodeUnsupportedMediaType))
		})

		It("refuses a missing size with 400", func() {
			resp := presign(postID, fiber.Map{"content_type": "video/mp4"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		})

		It("refuses with 402 when the declared size is over the media storage budget", func() {
			mediaBytes = trialMediaCap - 10
			resp := presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
			Expect(resp.StatusCode).To(Equal(fiber.StatusPaymentRequired))
		})

		It("returns 409 for a post already sent for publishing", func() {
			_, err := db.NewUpdate().Model((*models.Post)(nil)).Set("status = ?", models.PostStatusScheduled).
				Where("id = ?", postID).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
			resp := presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
			Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		})

		It("returns 404 for an unknown post", func() {
			resp := presign("nope", fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
			Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
		})
	})

	Describe("finalize", func() {
		It("creates a probed video attachment with a poster and announces it from the editor", func() {
			events := subscribePostEvents(hub, models.DefaultTenantID, "teammate")
			key := uploadVideo(postID, 2048)

			resp := finalize(postID, fiber.Map{"s3_key": key, "alt_text": "A spinning logo"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			body := decode(resp)
			Expect(body).To(HaveKeyWithValue("mime_type", "video/mp4"))
			Expect(body).To(HaveKeyWithValue("duration_ms", BeNumerically("==", 6200)))
			Expect(body).To(HaveKeyWithValue("size_bytes", BeNumerically("==", 2048)))
			Expect(body).To(HaveKeyWithValue("alt_text", "A spinning logo"))
			Expect(body["thumbnail_url"]).NotTo(BeEmpty())
			Expect(body["platform_validation"]).To(BeNil())
			Expect(attachmentEventPayload(nextAttachmentEvent(events))).To(HaveKeyWithValue("source", "editor"))
		})

		It("reports platform rules as soft warnings", func() {
			prober.result.DurationMs = 1000 // Instagram has a 3s floor
			key := uploadVideo(postID, 2048)
			resp := finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			Expect(decode(resp)["platform_validation"]).To(ContainElement(HaveKeyWithValue("rule", "min_duration_seconds")))
		})

		It("refuses with 402 when the real size is over the budget, deleting the object", func() {
			key := uploadVideo(postID, 2048)
			mediaBytes = trialMediaCap - 1024 // the presign fit; the stored object no longer does
			resp := finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusPaymentRequired))
			Expect(store.objects).NotTo(HaveKey(key))
			Expect(attachmentCount()).To(BeZero())
		})

		It("refuses a key minted for another post", func() {
			other := createPost(instagramPlatformID, "reel")
			key := uploadVideo(other, 2048)
			resp := finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
			Expect(store.objects).To(HaveKey(key))
		})

		It("refuses before the bytes were PUT", func() {
			resp := presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
			key := decode(resp)["s3_key"].(string)
			resp = finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
			Expect(decode(resp)["error"]).To(ContainSubstring("not found"))
		})

		It("refuses an empty object with empty_file and deletes it", func() {
			key := uploadVideo(postID, 1)
			store.objects[key] = []byte{}
			resp := finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
			Expect(decode(resp)).To(HaveKeyWithValue("code", models.UploadCodeEmptyFile))
			Expect(store.objects).NotTo(HaveKey(key))
		})

		It("refuses an unreadable video with invalid_file and deletes it", func() {
			prober.err = status.Error(codes.InvalidArgument, "moov atom not found")
			key := uploadVideo(postID, 2048)
			resp := finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
			Expect(decode(resp)).To(HaveKeyWithValue("code", models.UploadCodeInvalidFile))
			Expect(store.objects).NotTo(HaveKey(key))
			Expect(attachmentCount()).To(BeZero())
		})

		It("still creates the attachment, unprobed, when video-service is down", func() {
			prober.err = status.Error(codes.Unavailable, "connection refused")
			key := uploadVideo(postID, 2048)
			resp := finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			body := decode(resp)
			Expect(body).To(HaveKeyWithValue("mime_type", "video/mp4"))
			Expect(body).To(HaveKeyWithValue("duration_ms", BeNumerically("==", 0)))
			Expect(body).NotTo(HaveKey("thumbnail_url"))
		})

		It("returns 409 for a post already sent for publishing", func() {
			key := uploadVideo(postID, 2048)
			_, err := db.NewUpdate().Model((*models.Post)(nil)).Set("status = ?", models.PostStatusScheduled).
				Where("id = ?", postID).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
			resp := finalize(postID, fiber.Map{"s3_key": key})
			Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		})
	})
})
