package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

// fakeAudioEnqueuer records EnqueueProcessAudioTx calls so the presign specs can
// assert the (upload-only) reject/accept paths never enqueue a job — that
// happens at finalize, not presign.
type fakeAudioEnqueuer struct{ calls int }

func (f *fakeAudioEnqueuer) EnqueueProcessAudioTx(_ context.Context, _ *sql.Tx, _, _, _, _, _, _, _ string) error {
	f.calls++
	return nil
}

var _ = Describe("AudioAssetsHandler presign", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		store      *stubStorage
		enq        *fakeAudioEnqueuer
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	// newApp builds and registers a fresh app around the shared DB (no seeding);
	// swapping the enqueuer lets the 409 spec reuse the same session against a
	// handler where audio ingestion is unconfigured.
	newApp := func(audioJobs handlers.AudioIngestEnqueuer) *fiber.App {
		a := fiber.New(fiber.Config{
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
		tagRepo := repository.NewTagRepository(db)
		fileRepo := repository.NewAssetFileRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, fileRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(a)
		handlers.NewAudioAssetsHandler(
			assetRepo, fileRepo,
			repository.NewAudioExtractionRepository(db),
			repository.NewAudioSegmentRepository(db),
			repository.NewUtteranceRepository(db),
			store, db, audioJobs, auth,
		).Register(a)
		return a
	}

	BeforeEach(func() {
		enq = &fakeAudioEnqueuer{}
		app = newApp(enq)

		seedTenantUser(db, "Admin", "audio@example.com", "pw-password")
		loginBody, _ := json.Marshal(fiber.Map{"email": "audio@example.com", "password": "pw-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"audio_extractions", "asset_files", "assets", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	presign := func(filename string) *http.Response {
		body, _ := json.Marshal(fiber.Map{"filename": filename})
		req := httptest.NewRequest("POST", "/api/content-bank/assets/audio/presign", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	It("rejects an unsupported audio type with 400", func() {
		resp := presign("notes.pdf")
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		var out map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		Expect(out["error"]).To(ContainSubstring("unsupported audio type"))
		// A rejected presign creates no asset and enqueues nothing.
		Expect(enq.calls).To(Equal(0))
		var n int
		Expect(db.NewSelect().TableExpr("assets").ColumnExpr("count(*)").Scan(context.Background(), &n)).To(Succeed())
		Expect(n).To(Equal(0))
	})

	It("presigns a supported audio upload as a pending AUDIO asset", func() {
		resp := presign("interview.mp3")
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out struct {
			Asset struct {
				ID     string `json:"id"`
				Type   string `json:"type"`
				Status string `json:"status"`
				Title  string `json:"title"`
			} `json:"asset"`
			UploadURL  string `json:"upload_url"`
			StorageKey string `json:"storage_key"`
			Method     string `json:"method"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		Expect(out.Asset.Type).To(Equal(models.AssetTypeAudio))
		Expect(out.Asset.Status).To(Equal(models.AssetStatusPending))
		Expect(out.Asset.Title).To(Equal("interview"))
		Expect(out.UploadURL).NotTo(BeEmpty())
		Expect(out.Method).To(Equal(fiber.MethodPut))
		Expect(out.StorageKey).To(Equal("assets/" + out.Asset.ID + "/original.mp3"))
		// Presign records a size-0 file row (finalize stamps the real size); it does
		// NOT enqueue — that is finalize's job.
		Expect(enq.calls).To(Equal(0))
		var size int64 = -1
		Expect(db.NewSelect().TableExpr("asset_files").ColumnExpr("size_bytes").
			Where("asset_id = ?", out.Asset.ID).Scan(context.Background(), &size)).To(Succeed())
		Expect(size).To(Equal(int64(0)))
	})

	It("returns 409 when audio ingestion is not configured", func() {
		app = newApp(nil) // nil enqueuer → configured() is false; reuse the session
		resp := presign("interview.mp3")
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
	})

	It("rejects an oversized object on extract with 413 and no enqueue", func() {
		// Create the pending asset, then simulate an upload larger than the cap.
		// A caller who skips finalize and calls extract must still be size-gated
		// before the worker can probe/process the object (CWE-400).
		resp := presign("huge.mp3")
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var pr struct {
			Asset struct {
				ID string `json:"id"`
			} `json:"asset"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&pr)).To(Succeed())

		store.headSizeOverride = 6 << 30 // 6 GiB, over the 5 GiB cap

		req := httptest.NewRequest("POST", "/api/content-bank/assets/"+pr.Asset.ID+"/audio/extract", nil)
		req.AddCookie(authCookie)
		r, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(r.StatusCode).To(Equal(fiber.StatusRequestEntityTooLarge))
		Expect(enq.calls).To(Equal(0))
	})
})
