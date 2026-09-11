package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// pngBytes encodes a w×h PNG and returns it as a string (multipart bodies here
// are strings; a Go string holds the binary fine).
func pngBytes(w, h int) string {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.String()
}

// fakeImageEnqueuer records process_image enqueues so the async upload path can
// be asserted without a running River worker (CON-281). It ignores the tx — the
// upload's RunInTx is a real transaction against the test DB; only the enqueue is
// faked.
type fakeImageEnqueuer struct{ calls []string }

func (f *fakeImageEnqueuer) EnqueueProcessImageTx(_ context.Context, _ *sql.Tx, assetID, _, _, mimeType, storageKey, runKey, _ string) error {
	f.calls = append(f.calls, strings.Join([]string{assetID, runKey, mimeType, storageKey}, "|"))
	return nil
}

var _ = Describe("AssetsHandler image upload (CON-281 async)", Ordered, Serial, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		store      *stubStorage
		imgEnq     *fakeImageEnqueuer
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	// buildApp (re)constructs the app + handlers against the shared DB. It does NOT
	// seed/login — that happens once per spec in BeforeEach — so a test can rebuild
	// with the image service unwired (withImageService=false) without re-seeding the
	// tenant (which would trip the unique-email constraint).
	buildApp := func(withImageService bool) {
		app = fiber.New(fiber.Config{
			BodyLimit: 100 << 20,
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
		fileRepo := repository.NewAssetFileRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, fileRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}
		imgEnq = &fakeImageEnqueuer{}
		// image-service is the single image authority (D6): a nil enqueuer models an
		// unwired service, which must reject image uploads.
		var imgJobs handlers.ImageIngestEnqueuer
		if withImageService {
			imgJobs = imgEnq
		}
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), store, db, nil, nil, nil, nil, imgJobs, auth, nil).Register(app)
	}

	BeforeEach(func() {
		buildApp(true)
		seedTenantUser(db, "Admin", "img@example.com", "pw-password")
		loginBody, _ := json.Marshal(fiber.Map{"email": "img@example.com", "password": "pw-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("assets").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, _ = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
	})

	buildMultipart := func(files []struct{ Name, Body string }) (*bytes.Buffer, string) {
		buf := &bytes.Buffer{}
		w := multipart.NewWriter(buf)
		for _, f := range files {
			fw, err := w.CreateFormFile("files", f.Name)
			Expect(err).NotTo(HaveOccurred())
			_, err = io.Copy(fw, strings.NewReader(f.Body))
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(w.Close()).To(Succeed())
		return buf, w.FormDataContentType()
	}

	postUpload := func(files []struct{ Name, Body string }) []map[string]any {
		body, ct := buildMultipart(files)
		req := httptest.NewRequest("POST", "/api/content-bank/assets/upload", body)
		req.Header.Set("Content-Type", ct)
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out struct {
			Results []map[string]any `json:"results"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out.Results
	}

	It("creates a PENDING IMG asset and enqueues a process_image job", func() {
		results := postUpload([]struct{ Name, Body string }{{"logo.png", pngBytes(4, 3)}})
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("created"))

		asset := results[0]["asset"].(map[string]any)
		Expect(asset["type"]).To(Equal(models.AssetTypeImage))
		// Ingestion is async now (CON-281): the asset lands `pending`, and the job
		// fills description/dimensions/alt text later.
		Expect(asset["status"]).To(Equal(models.AssetStatusPending))
		Expect(asset["title"]).To(Equal("logo"))
		Expect(asset["content"]).To(Equal(""))

		file := asset["file"].(map[string]any)
		Expect(file["mime_type"]).To(Equal("image/png"))
		// The original's URL is present; dimensions are 0 until the job stamps them.
		Expect(file["url"]).To(ContainSubstring("assets/"))
		Expect(file["url"]).To(ContainSubstring("/original.png"))

		// The exact bytes were stored under the tenant-scoped original key.
		assetID := results[0]["asset_id"].(string)
		var stored bool
		for k := range store.objects {
			if strings.HasSuffix(k, "assets/"+assetID+"/original.png") {
				stored = true
			}
		}
		Expect(stored).To(BeTrue(), "original.png should be stored")

		// Exactly one job was enqueued, for run-1, atomically with the insert.
		Expect(imgEnq.calls).To(HaveLen(1))
		Expect(imgEnq.calls[0]).To(ContainSubstring(assetID + "|run-1|image/png|assets/" + assetID + "/original.png"))
	})

	It("accepts HEIC by extension (routed to the service, sniffed there)", func() {
		// The body isn't a real HEIC — the handler no longer validates pixels (the
		// service does); it only routes by extension and enqueues.
		results := postUpload([]struct{ Name, Body string }{{"photo.heic", "not-really-heic-but-routed"}})
		Expect(results[0]["status"]).To(Equal("created"))
		asset := results[0]["asset"].(map[string]any)
		Expect(asset["file"].(map[string]any)["mime_type"]).To(Equal("image/heic"))
		Expect(imgEnq.calls).To(HaveLen(1))
	})

	It("deduplicates identical bytes within a tenant (no second enqueue)", func() {
		pngA := pngBytes(8, 8)
		first := postUpload([]struct{ Name, Body string }{{"a.png", pngA}})
		second := postUpload([]struct{ Name, Body string }{{"b-copy.png", pngA}})
		Expect(first[0]["status"]).To(Equal("created"))
		Expect(second[0]["status"]).To(Equal("created"))
		Expect(second[0]["asset_id"]).To(Equal(first[0]["asset_id"]))

		count, err := db.NewSelect().Table("assets").Count(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(1))
		// The dedupe short-circuits before enqueue, so only the first upload queued.
		Expect(imgEnq.calls).To(HaveLen(1))
	})

	It("rejects SVG / vector images with a specific message", func() {
		results := postUpload([]struct{ Name, Body string }{{"icon.svg", "<svg/>"}})
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("vector"))
		Expect(imgEnq.calls).To(BeEmpty())
	})

	It("mentions images in the unsupported-type message", func() {
		results := postUpload([]struct{ Name, Body string }{{"notes.bin", "plain"}})
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("image"))
	})

	It("rejects image uploads when image-service is not configured (D6)", func() {
		buildApp(false) // rebuild with no image enqueuer wired (reuses the seeded session)
		results := postUpload([]struct{ Name, Body string }{{"logo.png", pngBytes(2, 2)}})
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("not configured"))
	})

	It("processes a mixed batch of markdown and image independently", func() {
		results := postUpload([]struct{ Name, Body string }{
			{"note.md", "# hi"},
			{"pic.png", pngBytes(2, 2)},
		})
		Expect(results).To(HaveLen(2))
		Expect(results[0]["asset"].(map[string]any)["type"]).To(Equal(models.AssetTypeMarkdown))
		Expect(results[1]["asset"].(map[string]any)["type"]).To(Equal(models.AssetTypeImage))
	})

	It("allows an empty content on an image update and stores alt_text", func() {
		created := postUpload([]struct{ Name, Body string }{{"logo.png", pngBytes(4, 4)}})
		id := created[0]["asset_id"].(string)

		body, _ := json.Marshal(fiber.Map{"title": "Brand logo", "content": "", "alt_text": "Our logo"})
		req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+id, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))

		var asset map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&asset)).To(Succeed())
		Expect(asset["alt_text"]).To(Equal("Our logo"))
		Expect(asset["content"]).To(Equal(""))
	})

	It("still requires content when updating a document asset", func() {
		created := postUpload([]struct{ Name, Body string }{{"note.md", "# hello"}})
		id := created[0]["asset_id"].(string)

		body, _ := json.Marshal(fiber.Map{"title": "Note", "content": ""})
		req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+id, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})
})
