package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// pdfEnqueueCall captures one EnqueueProcessPDFTx invocation.
type pdfEnqueueCall struct {
	assetID, tenantID, originalName, mimeType string
}

// fakePDFEnqueuer records EnqueueProcessPDFTx calls so the PDF upload test can
// assert ingestion was enqueued — in the same tx as the insert — without a real
// River job table. It runs inside db.RunInTx and just no-ops on the tx.
type fakePDFEnqueuer struct {
	calls []pdfEnqueueCall
	err   error // when set, fails the enqueue (and so the surrounding tx)
}

func (f *fakePDFEnqueuer) EnqueueProcessPDFTx(_ context.Context, _ *sql.Tx, assetID, tenantID, originalName, mimeType string) error {
	f.calls = append(f.calls, pdfEnqueueCall{assetID, tenantID, originalName, mimeType})
	return f.err
}

// Serial: this suite pushes 10 MB and 50 MB bodies through the handler; under
// `-race -procs=2` those take ~10-15s and starve 1s-budget requests on the other
// proc, flaking unrelated specs' setup calls. Running the whole (Ordered) suite
// serially keeps the CPU hogs out of the parallel phase. (An individual Serial
// spec isn't allowed inside a non-Serial Ordered container, so the decorator
// goes on the container.)
var _ = Describe("AssetsHandler upload", Ordered, Serial, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		store      *stubStorage
		enq        *fakePDFEnqueuer
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
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
		assetRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		// Wire the real PDF ingestion path: object storage for original.pdf and a
		// recording enqueuer, plus the test DB for the insert+enqueue transaction.
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}
		enq = &fakePDFEnqueuer{}
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewAssetsHandler(assetRepo, repository.NewAssetFileRepository(db), repository.NewAssetImageRepository(db), store, db, enq, nil, nil, nil, auth, nil).Register(app)

		seedTenantUser(db, "Admin", "up@example.com", "pw-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "up@example.com", "password": "pw-password"})
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
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
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

	post := func(body *bytes.Buffer, ct string) *http.Response {
		req := httptest.NewRequest("POST", "/api/content-bank/assets/upload", body)
		req.Header.Set("Content-Type", ct)
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	decode := func(resp *http.Response) []map[string]any {
		var out struct {
			Results []map[string]any `json:"results"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out.Results
	}

	It("creates assets from multiple .md files", func() {
		body, ct := buildMultipart([]struct{ Name, Body string }{
			{"first.md", "# Hello\n\nThis is the first asset.\n"},
			{"second.md", "- item 1\n- item 2\n"},
		})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(2))
		for _, r := range results {
			Expect(r["status"]).To(Equal("created"))
			Expect(r["asset_id"]).NotTo(BeEmpty())
			asset := r["asset"].(map[string]any)
			Expect(asset["type"]).To(Equal("MD"))
			Expect(asset["status"]).To(Equal(models.AssetStatusPending))
		}
		// Titles default to filename without extension.
		Expect(results[0]["filename"]).To(Equal("first.md"))
		Expect(results[0]["asset"].(map[string]any)["title"]).To(Equal("first"))
		Expect(results[1]["asset"].(map[string]any)["title"]).To(Equal("second"))
	})

	It("rejects non-.md files per file but still processes good ones", func() {
		body, ct := buildMultipart([]struct{ Name, Body string }{
			{"bad.txt", "not markdown"},
			{"good.md", "hello"},
		})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(2))
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring(".md"))
		Expect(results[1]["status"]).To(Equal("created"))
	})

	It("rejects files over the size limit", func() {
		big := strings.Repeat("x", (10<<20)+1)
		body, ct := buildMultipart([]struct{ Name, Body string }{{"big.md", big}})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("exceeds"))
	})

	It("creates an empty asset for an empty .md file", func() {
		body, ct := buildMultipart([]struct{ Name, Body string }{{"empty.md", ""}})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("created"))
		asset := results[0]["asset"].(map[string]any)
		Expect(asset["content"]).To(Equal(""))
		Expect(asset["type"]).To(Equal("MD"))
	})

	It("returns 400 with no files", func() {
		body, ct := buildMultipart(nil)
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("creates a PDF asset: stores original.pdf and enqueues ingestion (status=pending)", func() {
		// Minimal valid PDF magic header followed by a plausible body. The handler
		// checks the `%PDF` prefix and size, stores original.pdf to object storage,
		// then inserts the asset and enqueues the ingestion job in one transaction
		// (CON-103). Real processing runs async in the worker.
		const pdfBody = "%PDF-1.4\n%...dummy body..."
		body, ct := buildMultipart([]struct{ Name, Body string }{
			{"report.pdf", pdfBody},
		})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("created"))
		asset := results[0]["asset"].(map[string]any)
		Expect(asset["type"]).To(Equal(models.AssetTypePDF))
		Expect(asset["status"]).To(Equal(models.AssetStatusPending))
		Expect(asset["title"]).To(Equal("report"))

		assetID := results[0]["asset_id"].(string)
		Expect(assetID).NotTo(BeEmpty())

		// The original PDF was stored under the tenant-scoped key before enqueue,
		// with the exact uploaded bytes (the worker re-reads it per attempt).
		var storedKey string
		for k := range store.objects {
			if strings.HasSuffix(k, "assets/"+assetID+"/original.pdf") {
				storedKey = k
			}
		}
		Expect(storedKey).NotTo(BeEmpty(), "original.pdf should be stored")
		Expect(store.objects[storedKey]).To(Equal([]byte(pdfBody)))

		// The ingestion job was enqueued for that asset, in the same tx.
		Expect(enq.calls).To(HaveLen(1))
		Expect(enq.calls[0].assetID).To(Equal(assetID))
		Expect(enq.calls[0].originalName).To(Equal("report.pdf"))
		Expect(enq.calls[0].mimeType).To(Equal("application/pdf"))
	})

	It("deletes the orphaned original.pdf when the insert/enqueue transaction fails", func() {
		// Force the tx to roll back after the blob upload by failing the enqueue.
		enq.err = errors.New("enqueue boom")
		body, ct := buildMultipart([]struct{ Name, Body string }{
			{"oops.pdf", "%PDF-1.4\n%orphan cleanup"},
		})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("create asset"))

		// The upload ran (enqueue was reached inside the tx), but the rollback
		// must not leave original.pdf behind — the key is cleaned up.
		Expect(enq.calls).To(HaveLen(1))
		Expect(store.objects).To(BeEmpty(), "original.pdf must be cleaned up after tx failure")
	})

	It("rejects .pdf files failing the magic-byte sniff", func() {
		body, ct := buildMultipart([]struct{ Name, Body string }{
			{"fake.pdf", "this is not a PDF"},
		})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("PDF"))
	})

	It("rejects .pdf files over 50 MB", func() {
		big := "%PDF-1.4\n" + strings.Repeat("x", (50<<20)+1)
		body, ct := buildMultipart([]struct{ Name, Body string }{{"huge.pdf", big}})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("exceeds"))
	})

	It("rejects unknown extensions with a clear message", func() {
		body, ct := buildMultipart([]struct{ Name, Body string }{{"notes.txt", "plain text"}})
		resp := post(body, ct)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		results := decode(resp)
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring(".pdf"))
	})

	It("requires authentication", func() {
		body, ct := buildMultipart([]struct{ Name, Body string }{{"x.md", "x"}})
		req := httptest.NewRequest("POST", "/api/content-bank/assets/upload", body)
		req.Header.Set("Content-Type", ct)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(401))
	})
})
