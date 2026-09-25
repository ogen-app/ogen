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

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// CON-312 §3: audio presign/finalize skipped the CON-295 quota gate that the
// multipart /upload applies. The caller's workspace runs on the seeded Trial
// tier (content_bank_assets = 10, media_storage_bytes = 100 MiB); the counters
// are stubbed so each spec pins usage exactly at or under the cap.
var _ = Describe("AudioAssetsHandler quota (CON-312)", Ordered, func() {
	var (
		app          *fiber.App
		db           *bun.DB
		authCookie   *http.Cookie
		store        *stubStorage
		enq          *fakeAudioEnqueuer
		assetCount   int64
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

	BeforeEach(func() {
		assetCount, mediaBytes = 0, 0
		enq = &fakeAudioEnqueuer{}
		// Mirror the production error handler so a *QuotaExceededError is a 402.
		app = fiber.New(fiber.Config{
			ErrorHandler: func(c *fiber.Ctx, err error) error {
				var qe *entitlements.QuotaExceededError
				if errors.As(err, &qe) {
					return c.Status(fiber.StatusPaymentRequired).JSON(fiber.Map{"error": "entitlement_exceeded", "feature": qe.Key})
				}
				code := fiber.StatusInternalServerError
				var fe *fiber.Error
				if errors.As(err, &fe) {
					code = fe.Code
				}
				return c.Status(code).JSON(fiber.Map{"error": err.Error()})
			},
		})
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		fileRepo := repository.NewAssetFileRepository(db)
		assetRepo := repository.NewAssetRepository(db, repository.NewTagRepository(db), fileRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}

		cat, err := entitlements.LoadCatalog()
		Expect(err).NotTo(HaveOccurred())
		resolver := entitlements.NewResolver(repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db), tenantRepo, cat)
		lim := entitlements.NewLimiter(resolver, cat, entitlements.ModeEnforce).
			Register("content_bank_assets", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return assetCount, nil })).
			Register("media_storage_bytes", entitlements.CounterFunc(func(context.Context, string) (int64, error) { return mediaBytes, nil }))

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		h := handlers.NewAudioAssetsHandler(assetRepo, fileRepo,
			repository.NewAudioExtractionRepository(db), repository.NewAudioSegmentRepository(db), repository.NewUtteranceRepository(db),
			store, db, enq, auth)
		h.SetLimiter(lim)
		h.Register(app)

		seedTenantUser(db, "Admin", "audio-quota@example.com", "pw-password")
		loginBody, _ := json.Marshal(fiber.Map{"email": "audio-quota@example.com", "password": "pw-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.Cookies()).To(HaveLen(1))
		authCookie = loginResp.Cookies()[0]
	})

	AfterEach(func() {
		for _, tbl := range []string{"audio_extractions", "asset_files", "assets", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
	})

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

	presignOK := func() string {
		GinkgoHelper()
		resp := post("/api/content-bank/assets/audio/presign", fiber.Map{"filename": "episode.mp3"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out struct {
			Asset models.Asset `json:"asset"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out.Asset.ID
	}

	It("refuses presign with 402 at the content_bank_assets cap and creates nothing", func() {
		assetCount = 10
		resp := post("/api/content-bank/assets/audio/presign", fiber.Map{"filename": "episode.mp3"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusPaymentRequired))
		var n int
		Expect(db.NewSelect().TableExpr("assets").ColumnExpr("count(*)").Scan(ctx, &n)).To(Succeed())
		Expect(n).To(BeZero())
	})

	It("refuses finalize with 402 when the upload would exceed media_storage_bytes", func() {
		id := presignOK()
		mediaBytes = 90 << 20
		store.headSizeOverride = 20 << 20 // 90 + 20 MiB > 100 MiB trial cap
		resp := post("/api/content-bank/assets/audio/finalize", fiber.Map{"asset_id": id})
		Expect(resp.StatusCode).To(Equal(fiber.StatusPaymentRequired))
		Expect(enq.calls).To(BeZero(), "a refused finalize must not start transcription")
	})

	It("finalizes an upload that fits the media budget", func() {
		id := presignOK()
		mediaBytes = 10 << 20
		store.headSizeOverride = 20 << 20
		resp := post("/api/content-bank/assets/audio/finalize", fiber.Map{"asset_id": id})
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(enq.calls).To(Equal(1))
	})
})
