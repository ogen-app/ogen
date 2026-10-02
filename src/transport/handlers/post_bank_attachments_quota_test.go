package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Attach-from-asset gates media_storage_bytes on the stripped copy it stores,
// not on the bank original's recorded size. The workspace runs on the seeded
// Trial tier (media_storage_bytes = 100 MiB); usage is stubbed per spec.
var _ = Describe("PostAttachmentsHandler from-asset quota", Ordered, func() {
	const trialMediaCap = 100 << 20

	var (
		app          *fiber.App
		db           *bun.DB
		authCookie   *http.Cookie
		store        *stubStorage
		mediaBytes   int64
		userID       string
		postID       string
		originalTier string
	)
	ctx := context.Background()

	post := func(path string, body fiber.Map) *http.Response {
		GinkgoHelper()
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
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
		// Mirror the production error handler so a *QuotaExceededError is a 402.
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
		assetRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}

		cat, err := entitlements.LoadCatalog()
		Expect(err).NotTo(HaveOccurred())
		resolver := entitlements.NewResolver(repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db), repository.NewTenantRepository(db), cat)
		lim := entitlements.NewLimiter(resolver, cat, entitlements.ModeEnforce).
			Register("media_storage_bytes", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return mediaBytes, nil }))

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), platformRepo, postAttRepo, auth, handlers.PostsOptions{}).Register(app)
		handlers.NewPostAttachmentsHandler(postAttRepo, postRepo, store, fakePDFRenderer{}, nil, &fakeImagePreparer{store: store}, nil, 280, auth, lim).
			WithContentBank(assetRepo).Register(app)

		userID = seedTenantUser(db, "Admin", "bank-quota@example.com", "pw-password").ID
		loginBody, _ := json.Marshal(fiber.Map{"email": "bank-quota@example.com", "password": "pw-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.Cookies()).To(HaveLen(1))
		authCookie = loginResp.Cookies()[0]

		var camp models.Campaign
		Expect(json.NewDecoder(post("/api/campaigns", fiber.Map{"name": "Quota", "campaign_type_id": "Uk"}).Body).Decode(&camp)).To(Succeed())
		var p models.Post
		Expect(json.NewDecoder(post("/api/posts", fiber.Map{
			"campaign_id": camp.ID, "platform_id": "AXqWG7U2qnpt", "platform_post_type": "image-post", "title": "Quota Post",
		}).Body).Decode(&p)).To(Succeed())
		postID = p.ID
	})

	AfterEach(func() {
		for _, tbl := range []string{"post_attachments", "asset_files", "assets", "post_versions", "posts", "campaigns", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	// seedBankImage stores an IMG asset whose recorded size is 1 byte, so only a
	// check against the stripped copy's real size can tell the two apart.
	seedBankImage := func() string {
		GinkgoHelper()
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		fileID, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		imgType := models.AssetTypeImage
		_, err = db.NewInsert().Model(&models.Asset{
			ID: id, Title: "photo.png", Status: models.AssetStatusReady, Type: &imgType,
			TagIDs: models.StringSlice{}, CreatedBy: userID,
		}).Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		key := "t/" + models.DefaultTenantID + "/assets/" + id + "/original.png"
		_, err = db.NewInsert().Model(&models.AssetFile{
			ID: fileID, AssetID: id, OriginalName: "photo.png", MimeType: "image/png",
			SizeBytes: 1, S3Key: key, ChecksumSHA256: "original-" + id,
		}).Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		store.objects[key] = minimalPNG()
		return id
	}

	It("refuses with 402 when the stripped copy exceeds the budget, leaving no object or row", func() {
		assetID := seedBankImage()
		mediaBytes = trialMediaCap - 2 // the recorded 1 byte would fit; the real PNG does not

		resp := post("/api/posts/"+postID+"/attachments/from-asset", fiber.Map{"asset_id": assetID})
		Expect(resp.StatusCode).To(Equal(fiber.StatusPaymentRequired))

		for key := range store.objects {
			Expect(strings.Contains(key, "post-attachments/")).To(BeFalse(), "denied copy %s was left in storage", key)
		}
		n, err := db.NewSelect().Model((*models.PostAttachment)(nil)).Where("post_id = ?", postID).Count(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
	})

	It("attaches when the stripped copy fits the budget", func() {
		assetID := seedBankImage()
		resp := post("/api/posts/"+postID+"/attachments/from-asset", fiber.Map{"asset_id": assetID})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
	})
})
