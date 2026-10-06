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
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// pluginVideoWire decodes the plugin's video finalize response.
type pluginVideoWire struct {
	Attachment struct {
		ID           string `json:"id"`
		PostID       string `json:"post_id"`
		MimeType     string `json:"mime_type"`
		DurationMs   int64  `json:"duration_ms"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		SizeBytes    int64  `json:"size_bytes"`
		ThumbnailURL string `json:"thumbnail_url"`
	} `json:"attachment"`
	PlatformValidation []struct {
		Rule         string `json:"rule"`
		AttachmentID string `json:"attachment_id"`
	} `json:"platform_validation"`
	OpenURL string `json:"open_url"`
	Code    string `json:"code"`
}

// The workspace runs on the seeded Trial tier (media_storage_bytes = 100 MiB);
// usage is stubbed per spec.
var _ = Describe("Figma plugin video API", Ordered, func() {
	const (
		trialMediaCap       = 100 << 20
		pluginCap           = 10 << 20
		instagramPlatformID = "rzgpTkARLH0L"
		otherTenantID       = "plugin-video-other"
	)

	var (
		app          *fiber.App
		db           *bun.DB
		store        *stubStorage
		prober       *fakeVideoProber
		hub          eventhub.Hub
		jane         *models.User
		cookie       *http.Cookie
		token        string
		campaignID   string
		postID       string
		mediaBytes   int64
		originalTier string
	)
	ctx := context.Background()

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

	cookieJSON := func(path string, body fiber.Map) map[string]any {
		GinkgoHelper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(fiber.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		req.AddCookie(cookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out
	}
	createPost := func(postType string) string {
		GinkgoHelper()
		return cookieJSON("/api/posts", fiber.Map{
			"campaign_id": campaignID, "platform_id": instagramPlatformID, "platform_post_type": postType, "title": "Launch reel",
		})["id"].(string)
	}

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
		fileRepo := repository.NewAssetFileRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, fileRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}
		prober = &fakeVideoProber{result: probedMP4()}
		hub = eventhub.New(eventhub.Config{})

		cat, err := entitlements.LoadCatalog()
		Expect(err).NotTo(HaveOccurred())
		resolver := entitlements.NewResolver(repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db), repository.NewTenantRepository(db), cat)
		lim := entitlements.NewLimiter(resolver, cat, entitlements.ModeEnforce).
			Register("media_storage_bytes", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return mediaBytes, nil }))

		assets := handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), store, db, nil, nil, nil, nil, &fakeImageEnqueuer{}, auth, nil, handlers.AssetsOptions{})
		attachments := handlers.NewPostAttachmentsHandler(postAttRepo, postRepo, store, fakePDFRenderer{}, prober, &fakeImagePreparer{store: store}, nil, 280, auth, lim).
			WithEventHub(hub)

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), platformRepo, postAttRepo, auth, handlers.PostsOptions{}).Register(app)
		attachments.Register(app)
		handlers.NewFigmaPluginHandler(handlers.FigmaPluginDeps{
			Pairing:       newTestPluginService(db),
			AppBaseURL:    testAppBaseURL,
			Tokens:        repository.NewPluginTokenRepository(db),
			Users:         userRepo,
			Posts:         postRepo,
			Assets:        assets,
			Attachments:   attachments,
			MaxVideoBytes: pluginCap,
		}).Register(app)

		jane = seedTenantUser(db, "Jane", "jane-video@example.com", "jane-password")
		cookie = loginAs(app, "jane-video@example.com", "jane-password")
		token, _ = mintPluginToken(db, jane)

		campaignID = cookieJSON("/api/campaigns", fiber.Map{"name": "Launch", "campaign_type_id": "Uk"})["id"].(string)
		postID = createPost("reel")
	})

	AfterEach(func() {
		for _, tbl := range []string{"post_attachments", "post_versions", "posts", "campaigns", "plugin_tokens", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := db.NewDelete().Model((*models.Tenant)(nil)).Where("id = ?", otherTenantID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	callAs := func(bearer, path string, body fiber.Map) *http.Response {
		GinkgoHelper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(fiber.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		if bearer != "" {
			req.Header.Set(fiber.HeaderAuthorization, "Bearer "+bearer)
		}
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}
	presign := func(postID string, body fiber.Map) *http.Response {
		return callAs(token, "/api/plugins/figma/posts/"+postID+"/videos/presign", body)
	}
	finalize := func(postID string, body fiber.Map) (*http.Response, pluginVideoWire) {
		GinkgoHelper()
		resp := callAs(token, "/api/plugins/figma/posts/"+postID+"/videos/finalize", body)
		var out pluginVideoWire
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return resp, out
	}
	errorCode := func(resp *http.Response) string {
		GinkgoHelper()
		var out struct {
			Code string `json:"code"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out.Code
	}
	// upload presigns an MP4 of n bytes for post and "PUTs" it.
	upload := func(post string, n int) string {
		GinkgoHelper()
		resp := presign(post, fiber.Map{"content_type": "video/mp4", "size_bytes": n})
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var out struct {
			S3Key string `json:"s3_key"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		store.objects[out.S3Key] = bytes.Repeat([]byte{0}, n)
		return out.S3Key
	}
	frame := func(key string) fiber.Map {
		return fiber.Map{"s3_key": key, "node_id": "12:345", "node_name": "Hero / Reel", "file_name": "Launch"}
	}
	attachmentCount := func(post string) int {
		GinkgoHelper()
		n, err := db.NewSelect().Model((*models.PostAttachment)(nil)).Where("post_id = ?", post).Count(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		return n
	}
	lockPost := func(post string) {
		GinkgoHelper()
		_, err := db.NewUpdate().Model((*models.Post)(nil)).Set("status = ?", models.PostStatusScheduled).
			Where("id = ?", post).Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
	}

	It("attaches a rendered video to the post and announces it as a plugin send", func() {
		editor := subscribePostEvents(hub, models.DefaultTenantID, jane.ID)
		resp, out := finalize(postID, frame(upload(postID, 4096)))

		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(out.Attachment.PostID).To(Equal(postID))
		Expect(out.Attachment.MimeType).To(Equal("video/mp4"))
		Expect(out.Attachment.DurationMs).To(BeEquivalentTo(6200))
		Expect(out.Attachment.Width).To(Equal(1080))
		Expect(out.Attachment.Height).To(Equal(1920))
		Expect(out.Attachment.SizeBytes).To(BeEquivalentTo(4096))
		Expect(out.Attachment.ThumbnailURL).NotTo(BeEmpty())
		Expect(out.PlatformValidation).NotTo(BeNil())
		Expect(out.PlatformValidation).To(BeEmpty())
		Expect(out.OpenURL).To(Equal(testAppBaseURL + "/posts/" + postID))
		Expect(attachmentEventPayload(nextAttachmentEvent(editor))).To(HaveKeyWithValue("source", "figma_plugin"))
	})

	It("accepts WebM and refuses other containers", func() {
		Expect(presign(postID, fiber.Map{"content_type": "video/webm", "size_bytes": 1024}).StatusCode).To(Equal(fiber.StatusOK))

		resp := presign(postID, fiber.Map{"content_type": "video/quicktime", "size_bytes": 1024})
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnsupportedMediaType))
		Expect(errorCode(resp)).To(Equal(models.UploadCodeUnsupportedMediaType))
	})

	It("caps a video at the plugin limit", func() {
		resp := presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": pluginCap + 1})
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		Expect(errorCode(resp)).To(Equal(models.UploadCodeTooLarge))

		// The stored object is what counts, whatever was declared.
		key := upload(postID, 1024)
		store.objects[key] = bytes.Repeat([]byte{0}, pluginCap+1)
		fin, out := finalize(postID, frame(key))
		Expect(fin.StatusCode).To(Equal(fiber.StatusBadRequest))
		Expect(out.Code).To(Equal(models.UploadCodeTooLarge))
		Expect(store.objects).NotTo(HaveKey(key))
	})

	It("refuses with 402 over the media storage budget, at presign and at finalize", func() {
		key := upload(postID, 4096)

		mediaBytes = trialMediaCap - 1024
		Expect(presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": 4096}).StatusCode).To(Equal(fiber.StatusPaymentRequired))
		resp, _ := finalize(postID, frame(key))
		Expect(resp.StatusCode).To(Equal(fiber.StatusPaymentRequired))
		Expect(store.objects).NotTo(HaveKey(key))
		Expect(attachmentCount(postID)).To(BeZero())
	})

	It("reports an unknown post and a locked post with the plugin's codes", func() {
		resp := presign("no-such-post", fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
		Expect(errorCode(resp)).To(Equal(handlers.CodePostNotFound))

		key := upload(postID, 1024)
		lockPost(postID)
		resp = presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		Expect(errorCode(resp)).To(Equal(handlers.CodePostLocked))
		fin, out := finalize(postID, frame(key))
		Expect(fin.StatusCode).To(Equal(fiber.StatusConflict))
		Expect(out.Code).To(Equal(handlers.CodePostLocked))
	})

	It("requires the frame's node id and name", func() {
		key := upload(postID, 1024)
		resp, _ := finalize(postID, fiber.Map{"s3_key": key, "node_name": "Hero"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		resp, _ = finalize(postID, fiber.Map{"s3_key": key, "node_id": "1:2"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("refuses a key presigned for another post, and one never uploaded", func() {
		other := createPost("reel")
		key := upload(other, 1024)
		resp, _ := finalize(postID, frame(key))
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		Expect(store.objects).To(HaveKey(key))

		resp = presign(postID, fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
		var p struct {
			S3Key string `json:"s3_key"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		fin, _ := finalize(postID, frame(p.S3Key))
		Expect(fin.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("refuses an unreadable video and deletes it", func() {
		prober.err = status.Error(codes.InvalidArgument, "moov atom not found")
		key := upload(postID, 1024)
		resp, out := finalize(postID, frame(key))
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		Expect(out.Code).To(Equal(models.UploadCodeInvalidFile))
		Expect(store.objects).NotTo(HaveKey(key))
	})

	It("attaches the video unprobed when video-service is down", func() {
		prober.err = status.Error(codes.Unavailable, "connection refused")
		resp, out := finalize(postID, frame(upload(postID, 1024)))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(out.Attachment.MimeType).To(Equal("video/mp4"))
		Expect(out.Attachment.DurationMs).To(BeZero())
	})

	It("answers a repeated finalize with the attachment it already made", func() {
		key := upload(postID, 1024)
		first, a := finalize(postID, frame(key))
		Expect(first.StatusCode).To(Equal(fiber.StatusCreated))

		again, b := finalize(postID, frame(key))
		Expect(again.StatusCode).To(Equal(fiber.StatusOK))
		Expect(b.Attachment.ID).To(Equal(a.Attachment.ID))
		Expect(attachmentCount(postID)).To(Equal(1))
	})

	It("reports the video's own rule failures", func() {
		prober.result.DurationMs = 1000 // Instagram has a 3s floor
		_, out := finalize(postID, frame(upload(postID, 1024)))
		Expect(out.PlatformValidation).To(ContainElement(SatisfyAll(
			HaveField("Rule", "min_duration_seconds"),
			HaveField("AttachmentID", out.Attachment.ID),
		)))
	})

	It("reports post-level rules the video breaks, and not the post's other gaps", func() {
		_, _ = finalize(postID, frame(upload(postID, 1024)))
		_, second := finalize(postID, frame(upload(postID, 1024)))
		rules := make([]string, 0, len(second.PlatformValidation))
		for _, e := range second.PlatformValidation {
			rules = append(rules, e.Rule)
		}
		Expect(rules).To(ContainElements("max_attachments_per_post", "max_attachments"))

		imagePost := createPost("image-post")
		_, onImagePost := finalize(imagePost, frame(upload(imagePost, 1024)))
		rules = rules[:0]
		for _, e := range onImagePost.PlatformValidation {
			rules = append(rules, e.Rule)
		}
		Expect(rules).To(ContainElement("attachment_kind"))
		Expect(rules).NotTo(ContainElement("requires_content"))
	})

	It("doesn't let another workspace's token reach the post", func() {
		_, err := db.NewInsert().Model(&models.Tenant{
			ID: otherTenantID, Name: "Other", Slug: otherTenantID, TierID: models.DefaultTierID,
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		outsider := seedTenantUser(db, "Olga", "olga-video@example.com", "olga-password")
		_, err = db.NewUpdate().Model((*models.User)(nil)).Set("tenant_id = ?", otherTenantID).
			Where("id = ?", outsider.ID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		outsider.TenantID = otherTenantID
		theirToken, _ := mintPluginToken(db, outsider)

		resp := callAs(theirToken, "/api/plugins/figma/posts/"+postID+"/videos/presign", fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
		key := upload(postID, 1024)
		resp = callAs(theirToken, "/api/plugins/figma/posts/"+postID+"/videos/finalize", frame(key))
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
		Expect(attachmentCount(postID)).To(BeZero())
	})

	It("publishes the plugin cap and counts the post's videos for pre-flight checks", func() {
		get := func(path string, into any) {
			GinkgoHelper()
			req := httptest.NewRequest(fiber.MethodGet, path, nil)
			req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
			resp, err := app.Test(req, -1)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			Expect(json.NewDecoder(resp.Body).Decode(into)).To(Succeed())
		}
		var me struct {
			Limits struct {
				MaxVideoBytes int64 `json:"max_video_bytes"`
			} `json:"limits"`
		}
		get("/api/plugins/figma/me", &me)
		Expect(me.Limits.MaxVideoBytes).To(BeEquivalentTo(pluginCap))

		_, _ = finalize(postID, frame(upload(postID, 1024)))
		var tree struct {
			Campaigns []struct {
				Posts []struct {
					ID         string `json:"id"`
					PostType   string `json:"post_type"`
					VideoCount int    `json:"video_count"`
				} `json:"posts"`
			} `json:"campaigns"`
		}
		get("/api/plugins/figma/campaigns", &tree)
		Expect(tree.Campaigns).To(HaveLen(1))
		Expect(tree.Campaigns[0].Posts).To(ContainElement(SatisfyAll(
			HaveField("ID", postID), HaveField("PostType", "reel"), HaveField("VideoCount", 1),
		)))
	})

	It("refuses a request without a plugin token", func() {
		resp := callAs("", "/api/plugins/figma/posts/"+postID+"/videos/presign", fiber.Map{"content_type": "video/mp4", "size_bytes": 1024})
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
	})
})
