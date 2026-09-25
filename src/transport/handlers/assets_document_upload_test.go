package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

// fakeDocumentEnqueuer records process_document enqueues (CON-280) so the upload
// path can be asserted without a River worker; the insert tx is real.
type fakeDocumentEnqueuer struct{ calls []string }

func (f *fakeDocumentEnqueuer) EnqueueProcessDocumentTx(_ context.Context, _ *sql.Tx, assetID, _, originalName, mimeType, storageKey string) error {
	f.calls = append(f.calls, strings.Join([]string{assetID, originalName, mimeType, storageKey}, "|"))
	return nil
}

// CON-312 §7: the document branch of POST /upload (CON-280) had only worker
// tests. These pin routing by extension, the stored original, the atomic
// enqueue, and the early rejects.
var _ = Describe("AssetsHandler document upload (CON-280)", Ordered, Serial, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		store      *stubStorage
		docEnq     *fakeDocumentEnqueuer
	)
	ctx := context.Background()

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

	buildApp := func(withDocumentService bool) {
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
		tagRepo := repository.NewTagRepository(db)
		fileRepo := repository.NewAssetFileRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, fileRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}
		docEnq = &fakeDocumentEnqueuer{}
		var docJobs handlers.DocumentIngestEnqueuer
		if withDocumentService {
			docJobs = docEnq
		}
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), store, db, nil, nil, nil, docJobs, nil, auth, nil).Register(app)
	}

	BeforeEach(func() {
		buildApp(true)
		seedTenantUser(db, "Admin", "doc@example.com", "pw-password")
		loginBody, _ := json.Marshal(fiber.Map{"email": "doc@example.com", "password": "pw-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.Cookies()).To(HaveLen(1))
		authCookie = loginResp.Cookies()[0]
	})

	AfterEach(func() {
		for _, tbl := range []string{"asset_files", "assets", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	upload := func(name, body string) map[string]any {
		GinkgoHelper()
		buf := &bytes.Buffer{}
		w := multipart.NewWriter(buf)
		fw, err := w.CreateFormFile("files", name)
		Expect(err).NotTo(HaveOccurred())
		_, err = io.Copy(fw, strings.NewReader(body))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.Close()).To(Succeed())
		req := httptest.NewRequest("POST", "/api/content-bank/assets/upload", buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.AddCookie(authCookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out struct {
			Results []map[string]any `json:"results"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		Expect(out.Results).To(HaveLen(1))
		return out.Results[0]
	}

	assetCount := func() int {
		GinkgoHelper()
		var n int
		Expect(db.NewSelect().TableExpr("assets").ColumnExpr("count(*)").Scan(ctx, &n)).To(Succeed())
		return n
	}

	It("creates a pending DOC asset, stores the original, and enqueues ingestion", func() {
		res := upload("Q3 plan.docx", "PK\x03\x04 fake docx bytes")
		Expect(res["status"]).To(Equal("created"))
		asset := res["asset"].(map[string]any)
		Expect(asset["type"]).To(Equal(models.AssetTypeDocument))
		Expect(asset["status"]).To(Equal(models.AssetStatusPending))
		Expect(asset["title"]).To(Equal("Q3 plan"))

		id := res["asset_id"].(string)
		var stored []byte
		for k, v := range store.objects {
			if strings.HasSuffix(k, "assets/"+id+"/original.docx") {
				stored = v
			}
		}
		Expect(string(stored)).To(Equal("PK\x03\x04 fake docx bytes"))

		Expect(docEnq.calls).To(ConsistOf(strings.Join([]string{
			id, "Q3 plan.docx",
			"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			"assets/" + id + "/original.docx",
		}, "|")))
	})

	It("routes plain-text formats to document ingestion", func() {
		res := upload("pipeline.csv", "name,stage\nacme,won\n")
		Expect(res["status"]).To(Equal("created"))
		Expect(res["asset"].(map[string]any)["type"]).To(Equal(models.AssetTypeDocument))
		Expect(docEnq.calls).To(HaveLen(1))
		Expect(docEnq.calls[0]).To(ContainSubstring("|text/csv|"))
	})

	It("rejects a legacy/password-protected OLE2 file before storing anything", func() {
		res := upload("old.docx", "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1rest")
		Expect(res["status"]).To(Equal("failed"))
		Expect(res["code"]).To(Equal(models.UploadCodeUnsupportedMediaType))
		Expect(store.objects).To(BeEmpty())
		Expect(docEnq.calls).To(BeEmpty())
		Expect(assetCount()).To(BeZero())
	})

	It("rejects an empty file", func() {
		res := upload("empty.docx", "")
		Expect(res["status"]).To(Equal("failed"))
		Expect(res["code"]).To(Equal(models.UploadCodeEmptyFile))
		Expect(assetCount()).To(BeZero())
	})

	It("fails fast with service_unavailable when document-service is unwired", func() {
		buildApp(false)
		res := upload("Q3 plan.docx", "PK\x03\x04 fake docx bytes")
		Expect(res["status"]).To(Equal("failed"))
		Expect(res["code"]).To(Equal(models.UploadCodeServiceUnavailable))
		Expect(assetCount()).To(BeZero())
	})
})
