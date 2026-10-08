package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// pluginImageWire decodes POST /api/plugins/figma/images.
type pluginImageWire struct {
	Asset struct {
		ID        string                 `json:"id"`
		Title     string                 `json:"title"`
		Status    string                 `json:"status"`
		Origin    string                 `json:"origin"`
		OriginRef *models.AssetOriginRef `json:"origin_ref"`
		URL       string                 `json:"url"`
	} `json:"asset"`
	Deduplicated bool `json:"deduplicated"`
	Attachment   *struct {
		ID     string `json:"id"`
		PostID string `json:"post_id"`
	} `json:"attachment"`
	AttachError *struct {
		Code string `json:"code"`
	} `json:"attach_error"`
	OpenURL string `json:"open_url"`
	Code    string `json:"code"`
}

var _ = Describe("Figma plugin API", Ordered, func() {
	var (
		app      *fiber.App
		db       *bun.DB
		imgEnq   *fakeImageEnqueuer
		renderer *fakePreviewRenderer
		jane     *models.User
		cookie   *http.Cookie
		token    string
		postID   string
		otherPNG string
		hub      eventhub.Hub
	)
	ctx := context.Background()

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
		otherPNG = pngBytes(3, 2)
	})

	BeforeEach(func() {
		app = fiber.New(fiber.Config{
			BodyLimit: 100 << 20,
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
		store := &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}
		imgEnq = &fakeImageEnqueuer{}
		renderer = &fakePreviewRenderer{}

		assets := handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), store, db, nil, nil, nil, nil, imgEnq, auth, nil, handlers.AssetsOptions{})
		hub = eventhub.New(eventhub.Config{})
		attachments := handlers.NewPostAttachmentsHandler(postAttRepo, postRepo, store, fakePDFRenderer{}, nil, &fakeImagePreparer{store: store}, nil, 280, auth, nil).
			WithContentBank(assetRepo).WithEventHub(hub)

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), platformRepo, postAttRepo, auth, handlers.PostsOptions{}).Register(app)
		assets.Register(app)
		handlers.NewFigmaPluginHandler(handlers.FigmaPluginDeps{
			Pairing:     newTestPluginService(db),
			AppBaseURL:  testAppBaseURL,
			Tokens:      repository.NewPluginTokenRepository(db),
			Users:       userRepo,
			Posts:       postRepo,
			Platforms:   platformRepo,
			Assets:      assets,
			Attachments: attachments,

			PostAttachments: postAttRepo,
			MediaPreviews:   repository.NewMediaPreviewRepository(db),
			Storage:         store,
			Previews:        renderer,
		}).Register(app)

		jane = seedTenantUser(db, "Jane", "jane@example.com", "jane-password")
		cookie = loginAs(app, "jane@example.com", "jane-password")
		token, _ = mintPluginToken(db, jane)

		postJSON := func(path string, body fiber.Map, into any) {
			GinkgoHelper()
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest(fiber.MethodPost, path, bytes.NewReader(raw))
			req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			req.AddCookie(cookie)
			resp, err := app.Test(req, -1)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			Expect(json.NewDecoder(resp.Body).Decode(into)).To(Succeed())
		}
		var camp models.Campaign
		postJSON("/api/campaigns", fiber.Map{"name": "Launch", "campaign_type_id": "Uk"}, &camp)
		var p models.Post
		postJSON("/api/posts", fiber.Map{
			"campaign_id": camp.ID, "platform_id": "AXqWG7U2qnpt", "platform_post_type": "image-post",
			"title": "Hero announcement", "content": "secret body copy",
		}, &p)
		postID = p.ID
	})

	AfterEach(func() {
		for _, tbl := range []string{"media_previews", "post_attachments", "asset_files", "assets", "post_versions", "posts", "campaigns", "plugin_tokens", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	call := func(method, path string, body io.Reader, contentType string) *http.Response {
		GinkgoHelper()
		req := httptest.NewRequest(method, path, body)
		if contentType != "" {
			req.Header.Set(fiber.HeaderContentType, contentType)
		}
		req.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	sendImage := func(filename, content string, fields map[string]string) (*http.Response, pluginImageWire) {
		GinkgoHelper()
		buf := &bytes.Buffer{}
		w := multipart.NewWriter(buf)
		for k, v := range fields {
			Expect(w.WriteField(k, v)).To(Succeed())
		}
		fw, err := w.CreateFormFile("file", filename)
		Expect(err).NotTo(HaveOccurred())
		_, err = io.Copy(fw, strings.NewReader(content))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Close()).To(Succeed())
		resp := call(fiber.MethodPost, "/api/plugins/figma/images", buf, w.FormDataContentType())
		var out pluginImageWire
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &out)
		return resp, out
	}

	frame := func(extra map[string]string) map[string]string {
		f := map[string]string{"node_id": "12:345", "node_name": "Hero / Desktop", "file_name": "Launch"}
		maps.Copy(f, extra)
		return f
	}

	loadAsset := func(id string) *models.Asset {
		GinkgoHelper()
		a := new(models.Asset)
		Expect(db.NewSelect().Model(a).Where("a.id = ?", id).Scan(tenantCtx())).To(Succeed())
		return a
	}

	It("reports who the token acts as", func() {
		resp := call(fiber.MethodGet, "/api/plugins/figma/me", nil, "")
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var me struct {
			Workspace  struct{ ID, Name string }
			User       struct{ ID, Name string }
			Connection struct{ ID, Label string }
			Limits     struct {
				MaxImageBytes     int64    `json:"max_image_bytes"`
				MaxVideoBytes     int64    `json:"max_video_bytes"`
				VideoContentTypes []string `json:"video_content_types"`
			}
		}
		Expect(json.NewDecoder(resp.Body).Decode(&me)).To(Succeed())
		Expect(me.Workspace.ID).To(Equal(models.DefaultTenantID))
		Expect(me.Workspace.Name).NotTo(BeEmpty())
		Expect(me.User.ID).To(Equal(jane.ID))
		Expect(me.Connection.Label).To(Equal("Figma · Jane"))
		Expect(me.Limits.MaxImageBytes).To(BeNumerically(">", 0))
		// No plugin cap configured here, so the web app's video limit applies.
		Expect(me.Limits.MaxVideoBytes).To(BeNumerically("==", models.DefaultPlatformGlobalLimits().MaxVideoUploadBytes))
		Expect(me.Limits.VideoContentTypes).To(ConsistOf("video/mp4", "video/webm"))
	})

	It("stores a frame as a pending figma image asset and enqueues processing", func() {
		resp, out := sendImage("export.bin", pngBytes(4, 4), frame(map[string]string{"alt_text": "  Launch hero  "}))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(out.Deduplicated).To(BeFalse())
		Expect(out.Asset.Status).To(Equal(models.AssetStatusPending))
		Expect(out.Asset.Origin).To(Equal(models.AssetOriginFigma))
		Expect(out.Asset.Title).To(Equal("Hero - Desktop"))
		Expect(out.Asset.OriginRef).To(Equal(&models.AssetOriginRef{NodeID: "12:345", NodeName: "Hero / Desktop", FileName: "Launch"}))
		Expect(out.Asset.URL).To(HaveSuffix(".png"))
		Expect(out.OpenURL).To(Equal(testAppBaseURL + "/content-bank/assets/" + out.Asset.ID))
		Expect(out.Attachment).To(BeNil())
		Expect(out.AttachError).To(BeNil())
		Expect(imgEnq.calls).To(HaveLen(1))
		Expect(imgEnq.calls[0]).To(ContainSubstring("image/png"))

		stored := loadAsset(out.Asset.ID)
		Expect(stored.Origin).To(Equal(models.AssetOriginFigma))
		Expect(stored.CreatedBy).To(Equal(jane.ID))
		Expect(stored.AltText).To(Equal("Launch hero"))
		Expect(stored.AltTextEditedByUser).To(BeTrue())
	})

	It("returns the existing asset for identical bytes and keeps its provenance", func() {
		img := pngBytes(5, 5)
		_, first := sendImage("a.png", img, frame(nil))
		resp, second := sendImage("b.png", img, frame(map[string]string{"node_id": "9:9", "node_name": "Copy"}))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(second.Deduplicated).To(BeTrue())
		Expect(second.Asset.ID).To(Equal(first.Asset.ID))
		Expect(second.Asset.OriginRef.NodeID).To(Equal("12:345"))
		Expect(imgEnq.calls).To(HaveLen(1))
	})

	It("falls back to the node id when the frame name is unusable", func() {
		_, out := sendImage("x.png", otherPNG, frame(map[string]string{"node_name": " ../ "}))
		Expect(out.Asset.Title).To(Equal("figma-12-345"))
	})

	DescribeTable("rejects what isn't a PNG or JPEG export",
		func(filename, content string, status int, code string) {
			resp, out := sendImage(filename, content, frame(nil))
			Expect(resp.StatusCode).To(Equal(status))
			Expect(out.Code).To(Equal(code))
			Expect(imgEnq.calls).To(BeEmpty())
		},
		Entry("svg by name", "frame.svg", `<svg xmlns="http://www.w3.org/2000/svg"/>`, fiber.StatusUnsupportedMediaType, models.UploadCodeVectorRejected),
		Entry("svg by content", "frame.bin", `<?xml version="1.0"?><svg/>`, fiber.StatusUnsupportedMediaType, models.UploadCodeVectorRejected),
		Entry("other bytes", "frame.png", "GIF89a not really", fiber.StatusUnsupportedMediaType, models.UploadCodeUnsupportedMediaType),
		Entry("empty", "frame.png", "", fiber.StatusBadRequest, models.UploadCodeEmptyFile),
	)

	It("requires the frame's node id and name", func() {
		resp, _ := sendImage("f.png", otherPNG, map[string]string{"node_name": "Hero"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		resp, _ = sendImage("f.png", otherPNG, map[string]string{"node_id": "1:2"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("attaches the frame to a post when asked", func() {
		editor := subscribePostEvents(hub, models.DefaultTenantID, jane.ID)
		resp, out := sendImage("f.png", otherPNG, frame(map[string]string{"post_id": postID}))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(out.AttachError).To(BeNil())
		Expect(out.Attachment).NotTo(BeNil())
		Expect(out.Attachment.PostID).To(Equal(postID))

		// The sender's own open editor hears about it: actor events are not dropped.
		ev := nextAttachmentEvent(editor)
		Expect(ev.Topic).To(Equal("entity:post:" + postID))
		payload := attachmentEventPayload(ev)
		Expect(payload).To(HaveKeyWithValue("source", "figma_plugin"))
		Expect(payload).To(HaveKeyWithValue("attachment_id", out.Attachment.ID))

		var att models.PostAttachment
		Expect(db.NewSelect().Model(&att).Where("id = ?", out.Attachment.ID).Scan(tenantCtx())).To(Succeed())
		Expect(att.PostID).To(Equal(postID))
		Expect(att.MimeType).To(Equal("image/png"))
	})

	It("keeps the asset and reports why when the post can't take it", func() {
		editor := subscribePostEvents(hub, models.DefaultTenantID, jane.ID)
		resp, out := sendImage("f.png", otherPNG, frame(map[string]string{"post_id": "no-such-post"}))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(out.Attachment).To(BeNil())
		Expect(out.AttachError.Code).To(Equal(handlers.CodePostNotFound))
		loadAsset(out.Asset.ID)

		_, err := db.NewUpdate().TableExpr("posts").Set("status = ?", models.PostStatusScheduled).Where("id = ?", postID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		resp, out = sendImage("g.png", pngBytes(6, 6), frame(map[string]string{"post_id": postID}))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(out.AttachError.Code).To(Equal(handlers.CodePostLocked))
		loadAsset(out.Asset.ID)
		expectNoPostEvent(editor)
	})

	It("lists attachable posts without their body", func() {
		resp := call(fiber.MethodGet, "/api/plugins/figma/posts?q=hero", nil, "")
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		raw, _ := io.ReadAll(resp.Body)
		Expect(string(raw)).NotTo(ContainSubstring("secret body copy"))
		var out struct {
			Posts []struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Campaign *struct {
					Name string `json:"name"`
				} `json:"campaign"`
				Platform string `json:"platform"`
			} `json:"posts"`
		}
		Expect(json.Unmarshal(raw, &out)).To(Succeed())
		Expect(out.Posts).To(HaveLen(1))
		Expect(out.Posts[0].ID).To(Equal(postID))
		Expect(out.Posts[0].Campaign.Name).To(Equal("Launch"))
		Expect(out.Posts[0].Platform).NotTo(BeEmpty())

		resp = call(fiber.MethodGet, "/api/plugins/figma/posts?q=nothing-like-this", nil, "")
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		Expect(out.Posts).To(BeEmpty())
	})

	It("lists campaigns with their posts for the send-to picker", func() {
		resp := call(fiber.MethodGet, "/api/plugins/figma/campaigns", nil, "")
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		raw, _ := io.ReadAll(resp.Body)
		Expect(string(raw)).NotTo(ContainSubstring("secret body copy"))
		Expect(string(raw)).NotTo(ContainSubstring(`"content"`))

		type treeWire struct {
			Campaigns []struct {
				ID             string     `json:"id"`
				Name           string     `json:"name"`
				Status         string     `json:"status"`
				Timezone       *string    `json:"timezone"`
				StartDate      *string    `json:"start_date"`
				EndDate        *string    `json:"end_date"`
				PostsChangedAt *time.Time `json:"posts_changed_at"`
				Posts          []struct {
					ID       string `json:"id"`
					Title    string `json:"title"`
					Status   string `json:"status"`
					Platform *struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"platform"`
					PostType        string  `json:"post_type"`
					ScheduledAt     *string `json:"scheduled_at"`
					AttachmentCount *int    `json:"attachment_count"`
					VideoCount      *int    `json:"video_count"`
					Attachable      bool    `json:"attachable"`
				} `json:"posts"`
			} `json:"campaigns"`
			Platforms map[string]struct {
				Name  string `json:"name"`
				Video *struct {
					AllowedFormats        []string `json:"allowed_formats"`
					MaxDurationSeconds    int      `json:"max_duration_seconds"`
					AllowedAspectRatios   []string `json:"allowed_aspect_ratios"`
					MaxAttachmentsPerPost int      `json:"max_attachments_per_post"`
				} `json:"video"`
				PostTypes []struct {
					Slug string `json:"slug"`
					Rule *struct {
						AllowedKinds []string `json:"allowed_kinds"`
					} `json:"rule"`
					Canvas *models.Canvas `json:"canvas"`
				} `json:"post_types"`
			} `json:"platforms"`
		}
		var out treeWire
		Expect(json.Unmarshal(raw, &out)).To(Succeed())
		Expect(out.Campaigns).To(HaveLen(1))
		camp := out.Campaigns[0]
		Expect(camp.Name).To(Equal("Launch"))
		Expect(camp.Status).NotTo(BeEmpty())
		Expect(camp.Timezone).NotTo(BeNil())
		Expect(camp.PostsChangedAt).NotTo(BeNil())
		createdAt := *camp.PostsChangedAt
		Expect(camp.Posts).To(HaveLen(1))
		post := camp.Posts[0]
		Expect(post.ID).To(Equal(postID))
		Expect(post.Title).To(Equal("Hero announcement"))
		Expect(post.Status).To(Equal(string(models.PostStatusDraft)))
		Expect(post.Platform).NotTo(BeNil())
		Expect(post.Platform.ID).To(Equal("AXqWG7U2qnpt"))
		Expect(post.Platform.Name).To(Equal("LinkedIn"))
		Expect(post.ScheduledAt).To(BeNil())
		Expect(post.AttachmentCount).To(Equal(new(0)))
		Expect(post.VideoCount).To(Equal(new(0)))
		Expect(post.PostType).To(Equal("image-post"))
		Expect(post.Attachable).To(BeTrue())

		Expect(out.Platforms).To(HaveLen(1))
		linkedIn := out.Platforms["AXqWG7U2qnpt"]
		Expect(linkedIn.Name).To(Equal("LinkedIn"))
		Expect(linkedIn.Video).NotTo(BeNil())
		Expect(linkedIn.Video.AllowedFormats).To(ContainElement("mp4"))
		Expect(linkedIn.Video.MaxDurationSeconds).To(BeNumerically(">", 0))
		Expect(linkedIn.Video.AllowedAspectRatios).NotTo(BeEmpty())
		Expect(linkedIn.Video.MaxAttachmentsPerPost).To(Equal(1))
		Expect(linkedIn.PostTypes).To(ContainElement(SatisfyAll(
			HaveField("Slug", "image-post"),
			HaveField("Rule.AllowedKinds", ConsistOf("image")),
			HaveField("Canvas", Equal(&models.Canvas{Width: 1200, Height: 627})),
		)))
		Expect(linkedIn.PostTypes).To(ContainElement(SatisfyAll(
			HaveField("Slug", "text-post"),
			HaveField("Canvas", BeNil()),
		)))

		_, err := db.NewUpdate().TableExpr("posts").Set("status = ?", models.PostStatusScheduled).Where("id = ?", postID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		resp = call(fiber.MethodGet, "/api/plugins/figma/campaigns", nil, "")
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		out = treeWire{}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		Expect(out.Campaigns[0].Posts).To(HaveLen(1))
		Expect(out.Campaigns[0].Posts[0].Status).To(Equal(string(models.PostStatusScheduled)))
		Expect(out.Campaigns[0].Posts[0].Attachable).To(BeFalse())
		Expect(*out.Campaigns[0].PostsChangedAt).To(BeTemporally(">", createdAt))
	})

	It("serializes empty campaigns and post lists as arrays", func() {
		_, err := db.NewDelete().TableExpr("posts").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		resp := call(fiber.MethodGet, "/api/plugins/figma/campaigns", nil, "")
		raw, _ := io.ReadAll(resp.Body)
		Expect(string(raw)).To(ContainSubstring(`"posts":[]`))

		_, err = db.NewDelete().TableExpr("campaigns").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		resp = call(fiber.MethodGet, "/api/plugins/figma/campaigns", nil, "")
		raw, _ = io.ReadAll(resp.Body)
		Expect(strings.TrimSpace(string(raw))).To(Equal(`{"campaigns":[],"platforms":{}}`))
	})

	It("returns one campaign with all its posts for a board re-sync", func() {
		var campID string
		Expect(db.NewSelect().TableExpr("posts").Column("campaign_id").Where("id = ?", postID).Scan(ctx, &campID)).To(Succeed())

		type campaignWire struct {
			Campaign struct {
				ID             string     `json:"id"`
				Status         string     `json:"status"`
				PostsChangedAt *time.Time `json:"posts_changed_at"`
				Posts          []struct {
					ID       string `json:"id"`
					PostType string `json:"post_type"`
				} `json:"posts"`
			} `json:"campaign"`
			Platforms map[string]struct {
				PostTypes []struct {
					Slug   string         `json:"slug"`
					Canvas *models.Canvas `json:"canvas"`
				} `json:"post_types"`
			} `json:"platforms"`
		}
		get := func() campaignWire {
			GinkgoHelper()
			resp := call(fiber.MethodGet, "/api/plugins/figma/campaigns/"+campID, nil, "")
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			raw, _ := io.ReadAll(resp.Body)
			Expect(string(raw)).NotTo(ContainSubstring("secret body copy"))
			var out campaignWire
			Expect(json.Unmarshal(raw, &out)).To(Succeed())
			return out
		}

		out := get()
		Expect(out.Campaign.ID).To(Equal(campID))
		Expect(out.Campaign.Posts).To(HaveLen(1))
		Expect(out.Campaign.Posts[0].ID).To(Equal(postID))
		Expect(out.Campaign.Posts[0].PostType).To(Equal("image-post"))
		Expect(out.Campaign.PostsChangedAt).NotTo(BeNil())
		Expect(out.Platforms["AXqWG7U2qnpt"].PostTypes).To(ContainElement(SatisfyAll(
			HaveField("Slug", "image-post"),
			HaveField("Canvas", Equal(&models.Canvas{Width: 1200, Height: 627})),
		)))
		createdAt := *out.Campaign.PostsChangedAt

		// Archived campaigns are still served, with their status.
		_, err := db.NewUpdate().TableExpr("campaigns").Set("status = ?", models.StatusArchived).Set("archived_at = now()").
			Where("id = ?", campID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(get().Campaign.Status).To(Equal(string(models.StatusArchived)))

		// A deleted post drops out, and the campaign records the change.
		_, err = db.NewDelete().TableExpr("posts").Where("id = ?", postID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		out = get()
		Expect(out.Campaign.Posts).To(BeEmpty())
		Expect(*out.Campaign.PostsChangedAt).To(BeTemporally(">", createdAt))
		Expect(out.Platforms).To(BeEmpty())

		// A deleted campaign, or one that doesn't exist here, is a coded 404.
		_, err = db.NewUpdate().TableExpr("campaigns").Set("deleted_at = now()").Where("id = ?", campID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		for _, id := range []string{campID, "no-such-campaign"} {
			resp := call(fiber.MethodGet, "/api/plugins/figma/campaigns/"+id, nil, "")
			Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
			var body struct {
				Code string `json:"code"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body.Code).To(Equal(handlers.CodeCampaignNotFound))
		}
	})

	It("lists each post's media with Figma-ready previews", func() {
		var campID string
		Expect(db.NewSelect().TableExpr("posts").Column("campaign_id").Where("id = ?", postID).Scan(ctx, &campID)).To(Succeed())
		seg0 := 0
		seed := func(pos int, seg *int, mime string, w, h int, sum, key, thumb string) string {
			GinkgoHelper()
			id, err := models.NewID()
			Expect(err).NotTo(HaveOccurred())
			_, err = db.NewInsert().Model(&models.PostAttachment{
				ID: id, PostID: postID, Position: pos, SegmentIndex: seg, MimeType: mime, SizeBytes: 10,
				Width: w, Height: h, ChecksumSHA256: sum, S3Key: key, ThumbnailS3Key: thumb, CreatedBy: jane.ID,
			}).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
			return id
		}
		// The thread segment's media comes after the whole post's, though its
		// position is lower.
		segment := seed(0, &seg0, "image/jpeg", 800, 600, "sum-seg", "k/seg.jpg", "")
		jpeg := seed(1, nil, "image/jpeg", 2160, 2700, "sum-jpeg", "k/a.jpg", "")
		webp := seed(2, nil, "image/webp", 1000, 800, "sum-webp", "k/b.webp", "")
		huge := seed(3, nil, "image/png", 6000, 3000, "sum-huge", "k/c.png", "")
		again := seed(4, nil, "image/webp", 1000, 800, "sum-webp", "k/d.webp", "")
		video := seed(5, nil, "video/mp4", 1920, 1080, "", "k/e.mp4", "k/e-poster.png")
		bare := seed(6, nil, "video/mp4", 1920, 1080, "", "k/f.mp4", "")
		pdf := seed(7, nil, "application/pdf", 0, 0, "sum-pdf", "k/g.pdf", "k/g.png")
		broken := seed(8, nil, "image/webp", 500, 500, "sum-broken", "k/broken.webp", "")
		renderer.sizes = map[string][2]int{
			"k/b.webp": {1000, 800}, "k/c.png": {4096, 2048}, "k/e-poster.png": {1080, 1920},
		}

		type mediaWire struct {
			ID            string  `json:"id"`
			Kind          string  `json:"kind"`
			SegmentIndex  *int    `json:"segment_index"`
			PreviewURL    *string `json:"preview_url"`
			PreviewWidth  *int    `json:"preview_width"`
			PreviewHeight *int    `json:"preview_height"`
		}
		get := func() []mediaWire {
			GinkgoHelper()
			resp := call(fiber.MethodGet, "/api/plugins/figma/campaigns/"+campID, nil, "")
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			var out struct {
				Campaign struct {
					Posts []struct {
						Media []mediaWire `json:"media"`
					} `json:"posts"`
				} `json:"campaign"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			Expect(out.Campaign.Posts).To(HaveLen(1))
			return out.Campaign.Posts[0].Media
		}
		preview := func(m mediaWire) []any {
			if m.PreviewURL == nil {
				return nil
			}
			return []any{*m.PreviewURL, *m.PreviewWidth, *m.PreviewHeight}
		}
		signed := "https://pub.example.com/signed/"

		media := get()
		ids := make([]string, len(media))
		for i, m := range media {
			ids[i] = m.ID
		}
		Expect(ids).To(Equal([]string{jpeg, webp, huge, again, video, bare, pdf, broken, segment}))
		Expect(media[8].SegmentIndex).To(Equal(&seg0))
		Expect(media[0].Kind).To(Equal("image"))
		Expect(media[4].Kind).To(Equal("video"))
		Expect(media[6].Kind).To(Equal("pdf"))

		// A JPEG that fits is served as stored.
		Expect(preview(media[0])).To(Equal([]any{signed + "k/a.jpg", 2160, 2700}))
		// WebP, an oversized PNG and a poster are copied, the WebP once for
		// both attachments with its bytes.
		Expect(*media[1].PreviewURL).To(HavePrefix(signed + "t/" + models.DefaultTenantID + "/media-previews/"))
		Expect(preview(media[1])[1:]).To(Equal([]any{1000, 800}))
		Expect(preview(media[3])).To(Equal(preview(media[1])))
		Expect(preview(media[2])[1:]).To(Equal([]any{4096, 2048}))
		Expect(preview(media[4])[1:]).To(Equal([]any{1080, 1920}))
		// No poster, a PDF, or a failed render: no preview.
		Expect(media[5].PreviewURL).To(BeNil())
		Expect(media[6].PreviewURL).To(BeNil())
		Expect(media[7].PreviewURL).To(BeNil())
		Expect(renderer.sources()).To(ConsistOf("k/b.webp", "k/c.png", "k/e-poster.png", "k/broken.webp"))

		// The next read reuses the copies and retries only the failed one.
		Expect(get()[2]).To(Equal(media[2]))
		Expect(renderer.sources()).To(ConsistOf("k/b.webp", "k/c.png", "k/e-poster.png", "k/broken.webp", "k/broken.webp"))

		// The campaign list carries no media.
		resp := call(fiber.MethodGet, "/api/plugins/figma/campaigns", nil, "")
		raw, _ := io.ReadAll(resp.Body)
		Expect(string(raw)).NotTo(ContainSubstring(`"media"`))
	})

	It("refuses the campaign tree without a valid plugin token", func() {
		req := httptest.NewRequest(fiber.MethodGet, "/api/plugins/figma/campaigns", nil)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))

		req = httptest.NewRequest(fiber.MethodGet, "/api/plugins/figma/campaigns", nil)
		req.Header.Set(fiber.HeaderAuthorization, "Bearer ogp_not-a-real-token")
		resp, err = app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
	})

	It("disconnects itself", func() {
		Expect(call(fiber.MethodDelete, "/api/plugins/figma/token", nil, "").StatusCode).To(Equal(fiber.StatusNoContent))
		Expect(call(fiber.MethodGet, "/api/plugins/figma/me", nil, "").StatusCode).To(Equal(fiber.StatusUnauthorized))
	})

	It("rate-limits each token", func() {
		// The bucket holds 120 requests and refills at 2/s, so a slow run (race
		// detector, parallel suites) earns back a few before the burst ends.
		// Assert the full burst passes and the limit then bites, not the exact
		// request it bites on.
		for range 120 {
			Expect(call(fiber.MethodGet, "/api/plugins/figma/me", nil, "").StatusCode).To(Equal(fiber.StatusOK))
		}
		var resp *http.Response
		for range 200 {
			if resp = call(fiber.MethodGet, "/api/plugins/figma/me", nil, ""); resp.StatusCode != fiber.StatusOK {
				break
			}
		}
		Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
		Expect(resp.Header.Get(fiber.HeaderRetryAfter)).NotTo(BeEmpty())
	})
})
