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

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
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
		jane     *models.User
		cookie   *http.Cookie
		token    string
		postID   string
		otherPNG string
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

		assets := handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), store, db, nil, nil, nil, nil, imgEnq, auth, nil, handlers.AssetsOptions{})
		attachments := handlers.NewPostAttachmentsHandler(postAttRepo, postRepo, store, fakePDFRenderer{}, nil, &fakeImagePreparer{store: store}, nil, 280, auth, nil).
			WithContentBank(assetRepo)

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
			Assets:      assets,
			Attachments: attachments,
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
		for _, tbl := range []string{"post_attachments", "asset_files", "assets", "post_versions", "posts", "campaigns", "plugin_tokens", "sessions", "users", "accounts"} {
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
				MaxImageBytes int64 `json:"max_image_bytes"`
			}
		}
		Expect(json.NewDecoder(resp.Body).Decode(&me)).To(Succeed())
		Expect(me.Workspace.ID).To(Equal(models.DefaultTenantID))
		Expect(me.Workspace.Name).NotTo(BeEmpty())
		Expect(me.User.ID).To(Equal(jane.ID))
		Expect(me.Connection.Label).To(Equal("Figma · Jane"))
		Expect(me.Limits.MaxImageBytes).To(BeNumerically(">", 0))
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
		resp, out := sendImage("f.png", otherPNG, frame(map[string]string{"post_id": postID}))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(out.AttachError).To(BeNil())
		Expect(out.Attachment).NotTo(BeNil())
		Expect(out.Attachment.PostID).To(Equal(postID))

		var att models.PostAttachment
		Expect(db.NewSelect().Model(&att).Where("id = ?", out.Attachment.ID).Scan(tenantCtx())).To(Succeed())
		Expect(att.PostID).To(Equal(postID))
		Expect(att.MimeType).To(Equal("image/png"))
	})

	It("keeps the asset and reports why when the post can't take it", func() {
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
				ID        string  `json:"id"`
				Name      string  `json:"name"`
				Status    string  `json:"status"`
				Timezone  *string `json:"timezone"`
				StartDate *string `json:"start_date"`
				EndDate   *string `json:"end_date"`
				Posts     []struct {
					ID       string `json:"id"`
					Title    string `json:"title"`
					Status   string `json:"status"`
					Platform *struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"platform"`
					ScheduledAt     *string `json:"scheduled_at"`
					AttachmentCount *int    `json:"attachment_count"`
					Attachable      bool    `json:"attachable"`
				} `json:"posts"`
			} `json:"campaigns"`
		}
		var out treeWire
		Expect(json.Unmarshal(raw, &out)).To(Succeed())
		Expect(out.Campaigns).To(HaveLen(1))
		camp := out.Campaigns[0]
		Expect(camp.Name).To(Equal("Launch"))
		Expect(camp.Status).NotTo(BeEmpty())
		Expect(camp.Timezone).NotTo(BeNil())
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
		Expect(post.Attachable).To(BeTrue())

		_, err := db.NewUpdate().TableExpr("posts").Set("status = ?", models.PostStatusScheduled).Where("id = ?", postID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		resp = call(fiber.MethodGet, "/api/plugins/figma/campaigns", nil, "")
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		out = treeWire{}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		Expect(out.Campaigns[0].Posts).To(HaveLen(1))
		Expect(out.Campaigns[0].Posts[0].Status).To(Equal(string(models.PostStatusScheduled)))
		Expect(out.Campaigns[0].Posts[0].Attachable).To(BeFalse())
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
		Expect(strings.TrimSpace(string(raw))).To(Equal(`{"campaigns":[]}`))
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
		for range 120 {
			Expect(call(fiber.MethodGet, "/api/plugins/figma/me", nil, "").StatusCode).To(Equal(fiber.StatusOK))
		}
		resp := call(fiber.MethodGet, "/api/plugins/figma/me", nil, "")
		Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
		Expect(resp.Header.Get(fiber.HeaderRetryAfter)).NotTo(BeEmpty())
	})
})
