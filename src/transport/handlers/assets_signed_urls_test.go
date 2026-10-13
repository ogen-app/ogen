package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// A scraped page stores its mirrored images as public URLs inside the Markdown;
// reads must hand out signed ones (a private bucket refuses the public form),
// and a save that echoes the signed Markdown back must not persist links that
// expire.
var _ = Describe("AssetsHandler signed image links in scraped Markdown", Ordered, func() {
	const (
		pub    = "https://pub.example.com/"
		signed = "https://pub.example.com/signed/"
	)
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		imageRepo  repository.AssetImageRepository
		tctx       context.Context
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
		tctx = tenantctx.With(context.Background(), models.DefaultTenantID)
	})

	BeforeEach(func() {
		app = fiber.New()
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tagRepo := repository.NewTagRepository(db)
		fileRepo := repository.NewAssetFileRepository(db)
		imageRepo = repository.NewAssetImageRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewAssetsHandler(
			repository.NewAssetRepository(db, tagRepo, fileRepo), fileRepo, imageRepo,
			&stubStorage{}, db, nil, &fakeURLEnqueuer{}, &fakeScrapeGate{has: true}, nil, nil, auth, nil, handlers.AssetsOptions{}).
			Register(app)

		seedTenantUser(db, "Admin", "signed@example.com", "admin-password")
		loginBody, _ := json.Marshal(fiber.Map{"email": "signed@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]
	})

	AfterEach(func() {
		for _, tbl := range []string{"asset_images", "assets_chunks", "asset_files", "assets", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	do := func(method, path string, body any) *http.Response {
		var r *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		} else {
			r = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	// seedScrapedAsset creates a URL asset whose stored Markdown links two
	// mirrored images by their public URL, as process_url leaves it.
	seedScrapedAsset := func() (id, stored string, keys []string) {
		resp := do("POST", "/api/content-bank/assets/url", fiber.Map{"url": "https://8.8.8.8/page"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var a models.Asset
		Expect(json.NewDecoder(resp.Body).Decode(&a)).To(Succeed())

		keys = []string{
			"t/" + models.DefaultTenantID + "/assets/" + a.ID + "/images/0.png",
			"t/" + models.DefaultTenantID + "/assets/" + a.ID + "/images/1.jpg",
		}
		stored = "# Page\n\n![hero](" + pub + keys[0] + ")\n\nBody ![](" + pub + keys[1] + ")\n"
		_, err := db.NewUpdate().Model((*models.Asset)(nil)).
			Set("content = ?", stored).
			Set("status = ?", models.AssetStatusReady).
			Where("id = ?", a.ID).Exec(tctx)
		Expect(err).NotTo(HaveOccurred())

		images := make([]models.AssetImage, len(keys))
		for i, k := range keys {
			imgID, err := models.NewID()
			Expect(err).NotTo(HaveOccurred())
			images[i] = models.AssetImage{ID: imgID, AssetID: a.ID, Idx: i, SourceURL: "https://8.8.8.8/img" + k, S3Key: k}
		}
		Expect(imageRepo.ReplaceForAsset(tctx, a.ID, images)).To(Succeed())
		return a.ID, stored, keys
	}

	expectSigned := func(a models.Asset, keys []string) {
		Expect(a.Images).To(HaveLen(len(keys)))
		for i, k := range keys {
			Expect(a.Images[i].URL).To(HaveValue(Equal(signed + k)))
			Expect(a.Content).To(ContainSubstring("(" + signed + k + ")"))
			Expect(a.Content).NotTo(ContainSubstring("(" + pub + k + ")"))
		}
	}

	It("signs the image links on GET", func() {
		id, _, keys := seedScrapedAsset()

		resp := do("GET", "/api/content-bank/assets/"+id, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var one models.Asset
		Expect(json.NewDecoder(resp.Body).Decode(&one)).To(Succeed())
		expectSigned(one, keys)
	})

	It("signs the file and thumbnail links on the list", func() {
		id, _, _ := seedScrapedAsset()
		key, thumb := "t/"+models.DefaultTenantID+"/assets/"+id+"/original.pdf", "t/"+models.DefaultTenantID+"/assets/"+id+"/thumb.png"
		_, err := db.NewInsert().Model(&models.AssetFile{
			ID: "file-" + id, AssetID: id, OriginalName: "a.pdf", MimeType: "application/pdf",
			S3Key: key, ThumbnailS3Key: &thumb,
		}).Exec(tctx)
		Expect(err).NotTo(HaveOccurred())

		resp := do("GET", "/api/content-bank/assets", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var page struct {
			Assets []models.AssetListItem `json:"assets"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&page)).To(Succeed())
		Expect(page.Assets).To(HaveLen(1))
		Expect(page.Assets[0].File).NotTo(BeNil())
		Expect(page.Assets[0].File.URL).To(HaveValue(Equal(signed + key)))
		Expect(page.Assets[0].File.ThumbnailURL).To(HaveValue(Equal(signed + thumb)))
	})

	It("stores the public links when a save echoes the signed Markdown back", func() {
		id, stored, keys := seedScrapedAsset()

		resp := do("GET", "/api/content-bank/assets/"+id, nil)
		var got models.Asset
		Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())

		edited := strings.Replace(got.Content, "Body", "Edited body", 1)
		resp = do("PUT", "/api/content-bank/assets/"+id, fiber.Map{"title": got.Title, "content": edited})
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var updated models.Asset
		Expect(json.NewDecoder(resp.Body).Decode(&updated)).To(Succeed())
		expectSigned(updated, keys)

		var persisted models.Asset
		Expect(db.NewSelect().Model(&persisted).Where("id = ?", id).Scan(tctx)).To(Succeed())
		Expect(persisted.Content).To(Equal(strings.Replace(stored, "Body", "Edited body", 1)))
	})
})
