package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
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

// animatedGIFBytes encodes a 2-frame GIF so imageprobe reports is_animated.
func animatedGIFBytes() string {
	pal := color.Palette{color.Black, color.White}
	f1 := image.NewPaletted(image.Rect(0, 0, 2, 2), pal)
	f2 := image.NewPaletted(image.Rect(0, 0, 2, 2), pal)
	g := &gif.GIF{Image: []*image.Paletted{f1, f2}, Delay: []int{0, 0}}
	var buf bytes.Buffer
	_ = gif.EncodeAll(&buf, g)
	return buf.String()
}

var _ = Describe("AssetsHandler image upload (CON-246)", Ordered, Serial, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		store      *stubStorage
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
		fileRepo := repository.NewAssetFileRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, fileRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		store = &stubStorage{returnURL: "https://pub.example.com/x", objects: map[string][]byte{}}
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		// No PDF/URL jobs wired: images need only storage + db.
		handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), store, db, nil, nil, nil, nil, auth, nil).Register(app)

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

	It("creates an IMG asset from a PNG: ready, with dimensions and an original URL", func() {
		results := postUpload([]struct{ Name, Body string }{{"logo.png", pngBytes(4, 3)}})
		Expect(results).To(HaveLen(1))
		Expect(results[0]["status"]).To(Equal("created"))

		asset := results[0]["asset"].(map[string]any)
		Expect(asset["type"]).To(Equal(models.AssetTypeImage))
		Expect(asset["status"]).To(Equal(models.AssetStatusReady))
		Expect(asset["title"]).To(Equal("logo"))
		Expect(asset["content"]).To(Equal("")) // empty description is valid
		Expect(asset["alt_text"]).To(Equal(""))

		file := asset["file"].(map[string]any)
		Expect(file["mime_type"]).To(Equal("image/png"))
		Expect(file["width"]).To(BeNumerically("==", 4))
		Expect(file["height"]).To(BeNumerically("==", 3))
		Expect(file["is_animated"]).To(BeFalse())
		// The original's URL — the field the image viewer renders — is present and
		// points at assets/{id}/original.png.
		Expect(file["url"]).To(ContainSubstring("assets/"))
		Expect(file["url"]).To(ContainSubstring("/original.png"))

		// The exact bytes were stored under the tenant-scoped original key.
		assetID := results[0]["asset_id"].(string)
		var storedKey string
		for k := range store.objects {
			if strings.HasSuffix(k, "assets/"+assetID+"/original.png") {
				storedKey = k
			}
		}
		Expect(storedKey).NotTo(BeEmpty(), "original.png should be stored")
	})

	It("records is_animated for a multi-frame GIF", func() {
		results := postUpload([]struct{ Name, Body string }{{"spin.gif", animatedGIFBytes()}})
		Expect(results[0]["status"]).To(Equal("created"))
		file := results[0]["asset"].(map[string]any)["file"].(map[string]any)
		Expect(file["mime_type"]).To(Equal("image/gif"))
		Expect(file["is_animated"]).To(BeTrue())
	})

	It("deduplicates identical bytes within a tenant", func() {
		png := pngBytes(8, 8)
		first := postUpload([]struct{ Name, Body string }{{"a.png", png}})
		second := postUpload([]struct{ Name, Body string }{{"b-copy.png", png}})
		Expect(first[0]["status"]).To(Equal("created"))
		Expect(second[0]["status"]).To(Equal("created"))
		// Same checksum → the second upload returns the first asset, not a new one.
		Expect(second[0]["asset_id"]).To(Equal(first[0]["asset_id"]))

		count, err := db.NewSelect().Table("assets").Count(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(1))
	})

	It("rejects images whose dimensions exceed the cap", func() {
		results := postUpload([]struct{ Name, Body string }{{"wide.png", pngBytes(8193, 1)}})
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("dimensions"))
	})

	It("rejects a file whose bytes aren't a real image despite the extension", func() {
		results := postUpload([]struct{ Name, Body string }{{"fake.png", "this is definitely not a PNG"}})
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("unsupported media type"))
	})

	It("mentions images in the unsupported-type message", func() {
		// .bin is genuinely unsupported (.txt now routes to document ingestion).
		results := postUpload([]struct{ Name, Body string }{{"notes.bin", "plain"}})
		Expect(results[0]["status"]).To(Equal("failed"))
		Expect(results[0]["error"]).To(ContainSubstring("image"))
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
