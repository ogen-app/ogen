package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("AssetsHandler", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		app = fiber.New(fiber.Config{
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
		pieceRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth, nil, nil).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewAssetsHandler(pieceRepo, repository.NewAssetFileRepository(db), repository.NewAssetImageRepository(db), nil, nil, nil, nil, nil, nil, nil, auth, nil,
			handlers.AssetsOptions{Analyses: repository.NewAssetAnalysisRepository(db)}).Register(app)
		handlers.NewTagsHandler(tagRepo, auth).Register(app)

		// Seed an auth user and log in
		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("assets").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("tags").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// helper: create a asset via the API and return it
	createPiece := func(title, content string) models.Asset {
		body, _ := json.Marshal(fiber.Map{"title": title, "content": content})
		req := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var p models.Asset
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		return p
	}

	// listAssets reads one page of the asset list with query appended.
	listAssets := func(query string) assetListPage {
		req := httptest.NewRequest("GET", "/api/content-bank/assets"+query, nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		raw, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		var page assetListPage
		Expect(json.Unmarshal(raw, &page)).To(Succeed())
		page.Raw = string(raw)
		return page
	}

	// ── List ─────────────────────────────────────────────────────────────────

	Describe("GET /api/content-bank/assets", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/content-bank/assets", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns an empty list when no assets exist", func() {
				req := httptest.NewRequest("GET", "/api/content-bank/assets", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var page assetListPage
				Expect(json.NewDecoder(resp.Body).Decode(&page)).To(Succeed())
				Expect(page.Assets).To(BeEmpty())
				Expect(page.Total).To(Equal(0))
				Expect(page.NextCursor).To(BeNil())
			})

			It("returns all assets, newest first, without content", func() {
				createPiece("First", `[{"type":"paragraph"}]`)
				createPiece("Second", `[{"type":"paragraph"}]`)

				page := listAssets("")
				Expect(page.Assets).To(HaveLen(2))
				Expect(page.Assets[0].Title).To(Equal("Second"))
				Expect(page.Assets[0].Kind).To(Equal(models.AssetKindNote))
				Expect(page.Total).To(Equal(2))
				Expect(page.TotalUnfiltered).To(Equal(2))
				Expect(page.Raw).NotTo(ContainSubstring(`"content"`))
			})

			It("filters, pages and counts", func() {
				for _, title := range []string{"Alpha", "Beta", "Gamma"} {
					createPiece(title, `[{"type":"paragraph"}]`)
				}
				page := listAssets("?title=a&title_not=gamma&limit=1&facets=kind,status")
				Expect(page.Assets).To(HaveLen(1))
				Expect(page.Total).To(Equal(2))
				Expect(page.TotalUnfiltered).To(Equal(3))
				Expect(page.NextCursor).NotTo(BeNil())
				Expect(page.Facets["kind"]).To(ContainElement(models.AssetFacetCount{Value: "note", Count: 2}))
				Expect(page.Facets["status"]).To(ContainElement(models.AssetFacetCount{Value: "failed", Count: 0}))

				next := listAssets("?title=a&title_not=gamma&limit=1&cursor=" + *page.NextCursor)
				Expect(next.Assets).To(HaveLen(1))
				Expect(next.Assets[0].ID).NotTo(Equal(page.Assets[0].ID))
				Expect(next.NextCursor).To(BeNil())

				req := httptest.NewRequest("GET", "/api/content-bank/assets?ids_only=true&sort=title", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				var idsOnly struct {
					IDs []string `json:"ids"`
				}
				Expect(json.NewDecoder(resp.Body).Decode(&idsOnly)).To(Succeed())
				Expect(idsOnly.IDs).To(HaveLen(3))
			})

			DescribeTable("rejects invalid parameters with a stable code",
				func(query, code string) {
					req := httptest.NewRequest("GET", "/api/content-bank/assets"+query, nil)
					req.AddCookie(authCookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
					var body map[string]string
					Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
					Expect(body["code"]).To(Equal(code))
				},
				Entry("unknown kind", "?kind=pdf,movie", models.AssetCodeInvalidFilter),
				Entry("unknown status", "?status_not=processing", models.AssetCodeInvalidFilter),
				Entry("unknown ideas value", "?ideas=maybe", models.AssetCodeInvalidFilter),
				Entry("bad timestamp", "?updated_after=yesterday", models.AssetCodeInvalidFilter),
				Entry("bad checksum", "?checksum=abc", models.AssetCodeInvalidFilter),
				Entry("unknown sort", "?sort=size", models.AssetCodeInvalidFilter),
				Entry("relevance without text", "?sort=relevance", models.AssetCodeInvalidFilter),
				Entry("limit too high", "?limit=201", models.AssetCodeInvalidFilter),
				Entry("limit zero", "?limit=0", models.AssetCodeInvalidFilter),
				Entry("unknown facet", "?facets=kind,size", models.AssetCodeInvalidFilter),
				Entry("unknown time zone", "?tz=Mars/Base", models.AssetCodeInvalidFilter),
				Entry("bad ids_only", "?ids_only=maybe", models.AssetCodeInvalidFilter),
				Entry("unknown group", "?group=size", models.AssetCodeInvalidFilter),
				Entry("unknown created preset", "?created=2d", models.AssetCodeInvalidFilter),
				Entry("created preset with created_after", "?created=1d&created_after=2026-01-01T00:00:00Z", models.AssetCodeInvalidFilter),
				Entry("bad created timestamp", "?created_before=soon", models.AssetCodeInvalidFilter),
				Entry("garbage cursor", "?cursor=***", models.AssetCodeInvalidCursor),
				Entry("text too short", "?text=a", models.AssetCodeQueryTooShort),
				Entry("text too long", "?text="+strings.Repeat("a", 201), models.AssetCodeQueryTooLong),
			)

			It("groups, filters by when an asset was added and sums the undecided ideas", func() {
				createPiece("First", `[{"type":"paragraph"}]`)
				createPiece("Second", `[{"type":"paragraph"}]`)

				page := listAssets("?group=ideas&created=1d&tz=Europe/Kyiv&sort=created_at&limit=1&facets=created")
				Expect(page.Total).To(Equal(2))
				Expect(page.IdeasUndecided).To(Equal(0))
				Expect(page.Groups).To(Equal([]models.AssetGroupCount{{Key: "none", Count: 2}}))
				Expect(page.Assets).To(HaveLen(1))
				Expect(page.Assets[0].Title).To(Equal("First"))
				Expect(page.Assets[0].GroupKey).To(Equal("none"))
				Expect(page.Raw).To(ContainSubstring(`"next_idea":null`))
				Expect(page.Raw).To(ContainSubstring(`"published_at":null`))
				Expect(page.Facets["created"]).To(ContainElement(models.AssetFacetCount{Value: "1d", Count: 2}))

				next := listAssets("?group=ideas&created=1d&tz=Europe/Kyiv&sort=created_at&limit=1&cursor=" + *page.NextCursor)
				Expect(next.Assets).To(HaveLen(1))
				Expect(next.Assets[0].Title).To(Equal("Second"))

				ungrouped := listAssets("?created_before=2000-01-01T00:00:00Z")
				Expect(ungrouped.Total).To(Equal(0))
				Expect(ungrouped.Raw).To(ContainSubstring(`"groups":[]`))
				Expect(ungrouped.Raw).To(ContainSubstring(`"ideas_undecided_total":0`))
			})

			It("takes days for the date bounds, meaning their start in tz, as it takes instants", func() {
				createPiece("First", `[{"type":"paragraph"}]`)
				createPiece("Second", `[{"type":"paragraph"}]`)
				kyiv, err := time.LoadLocation("Europe/Kyiv")
				Expect(err).NotTo(HaveOccurred())
				local := time.Now().In(kyiv)
				today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, kyiv)
				tomorrow := today.AddDate(0, 0, 1)

				for _, field := range []string{"created", "updated"} {
					days := listAssets("?" + field + "_after=" + today.Format(time.DateOnly) + "&" + field + "_before=" +
						tomorrow.Format(time.DateOnly) + "&tz=Europe/Kyiv&ids_only=false")
					instants := listAssets("?" + field + "_after=" + url.QueryEscape(today.Format(time.RFC3339)) + "&" +
						field + "_before=" + url.QueryEscape(tomorrow.Format(time.RFC3339)))
					Expect(days.Total).To(Equal(2))
					Expect(days.Raw).To(Equal(instants.Raw))
					Expect(listAssets("?" + field + "_before=" + today.Format(time.DateOnly) + "&tz=Europe/Kyiv").Total).To(Equal(0))
				}
			})

			It("404s an unknown scope campaign", func() {
				req := httptest.NewRequest("GET", "/api/content-bank/assets?scope_campaign=missing", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
			})

			It("returns the gist and the recording duration on the list and the single asset", func() {
				p := createPiece("Launch sync", `[{"type":"paragraph"}]`)
				seedGistAndDuration(db, p.ID, "Pricing moves to seats in Q3.", 1_080_000)

				list := listAssets("").Assets
				Expect(list).To(HaveLen(1))
				Expect(list[0].Gist).To(HaveValue(Equal("Pricing moves to seats in Q3.")))
				Expect(list[0].File).NotTo(BeNil())
				Expect(list[0].File.DurationMS).To(Equal(int64(1_080_000)))

				getReq := httptest.NewRequest("GET", "/api/content-bank/assets/"+p.ID, nil)
				getReq.AddCookie(authCookie)
				getResp, err := app.Test(getReq)
				Expect(err).NotTo(HaveOccurred())
				var one models.Asset
				Expect(json.NewDecoder(getResp.Body).Decode(&one)).To(Succeed())
				Expect(one.Gist).To(HaveValue(Equal("Pricing moves to seats in Q3.")))
				Expect(one.File).NotTo(BeNil())
				Expect(one.File.DurationMs).To(Equal(int64(1_080_000)))
			})

			It("returns a null gist while the workspace analysis is not ready", func() {
				p := createPiece("Pending", `[{"type":"paragraph"}]`)
				tctx := tenantctx.With(context.Background(), models.DefaultTenantID)
				_, err := repository.NewAssetAnalysisRepository(db).Ensure(tctx, db, p.ID, nil)
				Expect(err).NotTo(HaveOccurred())

				var body struct {
					Assets []map[string]any `json:"assets"`
				}
				Expect(json.Unmarshal([]byte(listAssets("").Raw), &body)).To(Succeed())
				Expect(body.Assets).To(HaveLen(1))
				Expect(body.Assets[0]).To(HaveKeyWithValue("analysis_status", "pending"))
				Expect(body.Assets[0]).To(HaveKeyWithValue("gist", BeNil()))
			})
		})
	})

	// ── Create ───────────────────────────────────────────────────────────────

	Describe("POST /api/content-bank/assets", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"title": "Test", "content": `[]`})
				req := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("creates a asset and returns 201 with the created_by from the session", func() {
				body, _ := json.Marshal(fiber.Map{"title": "My Asset", "content": `[{"type":"paragraph"}]`})
				req := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var p models.Asset
				Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
				Expect(p.ID).NotTo(BeEmpty())
				Expect(p.Title).To(Equal("My Asset"))
				Expect(p.Content).To(Equal(`[{"type":"paragraph"}]`))
				Expect(p.CreatedBy).NotTo(BeEmpty())
			})

			It("returns 400 when title is missing", func() {
				body, _ := json.Marshal(fiber.Map{"content": `[]`})
				req := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when content is missing", func() {
				body, _ := json.Marshal(fiber.Map{"title": "No Content"})
				req := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Get ──────────────────────────────────────────────────────────────────

	Describe("GET /api/content-bank/assets/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/content-bank/assets/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the asset", func() {
				p := createPiece("Hello", `[{"type":"paragraph"}]`)

				req := httptest.NewRequest("GET", "/api/content-bank/assets/"+p.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Asset
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.ID).To(Equal(p.ID))
				Expect(got.Title).To(Equal("Hello"))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("GET", "/api/content-bank/assets/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Update ───────────────────────────────────────────────────────────────

	Describe("PUT /api/content-bank/assets/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"title": "Updated", "content": `[]`})
				req := httptest.NewRequest("PUT", "/api/content-bank/assets/someid", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("updates the asset and returns the updated resource", func() {
				p := createPiece("Original", `[{"type":"paragraph"}]`)

				body, _ := json.Marshal(fiber.Map{"title": "Updated", "content": `[{"type":"heading"}]`})
				req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.Asset
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Title).To(Equal("Updated"))
				Expect(got.Content).To(Equal(`[{"type":"heading"}]`))
			})

			It("returns 404 for an unknown id", func() {
				body, _ := json.Marshal(fiber.Map{"title": "Updated", "content": `[]`})
				req := httptest.NewRequest("PUT", "/api/content-bank/assets/nonexistent", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("returns 400 when body is invalid", func() {
				p := createPiece("Original", `[]`)

				body, _ := json.Marshal(fiber.Map{"title": "No Content Field"})
				req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+p.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Tag hydration ────────────────────────────────────────────────────────

	Describe("tag hydration on GET /api/content-bank/assets and /:id", func() {
		var tagID string

		BeforeEach(func() {
			tagBody, _ := json.Marshal(fiber.Map{"name": "Featured", "color": "#FF5733"})
			tagReq := httptest.NewRequest("POST", "/api/tags", bytes.NewReader(tagBody))
			tagReq.Header.Set("Content-Type", "application/json")
			tagReq.AddCookie(authCookie)
			tagResp, err := app.Test(tagReq)
			Expect(err).NotTo(HaveOccurred())
			Expect(tagResp.StatusCode).To(Equal(fiber.StatusCreated))
			var t map[string]any
			Expect(json.NewDecoder(tagResp.Body).Decode(&t)).To(Succeed())
			tagID = t["id"].(string)
		})

		It("returns tags populated on List", func() {
			body, _ := json.Marshal(fiber.Map{
				"title":   "Tagged Asset",
				"content": `[{"type":"paragraph"}]`,
				"tag_ids": []string{tagID},
			})
			req := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

			assets := listAssets("?tag=" + tagID).Assets
			Expect(assets).To(HaveLen(1))
			Expect(assets[0].Tags).To(HaveLen(1))
			Expect(assets[0].Tags[0].ID).To(Equal(tagID))
			Expect(assets[0].Tags[0].Name).To(Equal("Featured"))
			Expect(listAssets("?tag=none").Assets).To(BeEmpty())
		})

		It("returns tags populated on GetByID", func() {
			p := createPiece("Tagged", `[{"type":"paragraph"}]`)

			body, _ := json.Marshal(fiber.Map{
				"title":   "Tagged",
				"content": `[{"type":"paragraph"}]`,
				"tag_ids": []string{tagID},
			})
			req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+p.ID, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			_, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())

			getReq := httptest.NewRequest("GET", "/api/content-bank/assets/"+p.ID, nil)
			getReq.AddCookie(authCookie)
			getResp, err := app.Test(getReq)
			Expect(err).NotTo(HaveOccurred())
			Expect(getResp.StatusCode).To(Equal(200))

			var got models.Asset
			Expect(json.NewDecoder(getResp.Body).Decode(&got)).To(Succeed())
			Expect(got.Tags).To(HaveLen(1))
			Expect(got.Tags[0].Name).To(Equal("Featured"))
		})

		It("returns empty tags array when no tag_ids set", func() {
			p := createPiece("No Tags", `[{"type":"paragraph"}]`)

			getReq := httptest.NewRequest("GET", "/api/content-bank/assets/"+p.ID, nil)
			getReq.AddCookie(authCookie)
			getResp, err := app.Test(getReq)
			Expect(err).NotTo(HaveOccurred())

			var got models.Asset
			Expect(json.NewDecoder(getResp.Body).Decode(&got)).To(Succeed())
			Expect(got.Tags).NotTo(BeNil())
			Expect(got.Tags).To(BeEmpty())
		})
	})

	// ── Tagging ─────────────────────────────────────────────────────

	Describe("asset tagging (CON-279)", func() {
		createTag := func(name string) string {
			body, _ := json.Marshal(fiber.Map{"name": name})
			req := httptest.NewRequest("POST", "/api/tags", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var t map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&t)).To(Succeed())
			return t["id"].(string)
		}

		putJSON := func(id string, body fiber.Map) *http.Response {
			buf, _ := json.Marshal(body)
			req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+id, bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}

		postJSON := func(path string, body fiber.Map) *http.Response {
			buf, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", path, bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}

		getAsset := func(id string) models.Asset {
			req := httptest.NewRequest("GET", "/api/content-bank/assets/"+id, nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))
			var a models.Asset
			Expect(json.NewDecoder(resp.Body).Decode(&a)).To(Succeed())
			return a
		}

		body := `[{"type":"paragraph"}]`

		// The bug this fixes: the PUT is a whole-resource write, and a
		// title/content-only save (all the document editor sends) used to reset
		// tags and alt text to empty.
		Context("PUT preserves fields the payload omits", func() {
			It("keeps existing tags when tag_ids is omitted", func() {
				tagID := createTag("Legal")
				p := createPiece("Doc", body)
				Expect(putJSON(p.ID, fiber.Map{"title": "Doc", "content": body, "tag_ids": []string{tagID}}).StatusCode).To(Equal(200))

				// A rename with no tag_ids field must not touch the tags.
				Expect(putJSON(p.ID, fiber.Map{"title": "Doc renamed", "content": body}).StatusCode).To(Equal(200))

				got := getAsset(p.ID)
				Expect(got.Title).To(Equal("Doc renamed"))
				Expect(got.TagIDs).To(ConsistOf(tagID))
			})

			It("clears tags when tag_ids is an explicit empty array", func() {
				tagID := createTag("Legal")
				p := createPiece("Doc", body)
				Expect(putJSON(p.ID, fiber.Map{"title": "Doc", "content": body, "tag_ids": []string{tagID}}).StatusCode).To(Equal(200))

				Expect(putJSON(p.ID, fiber.Map{"title": "Doc", "content": body, "tag_ids": []string{}}).StatusCode).To(Equal(200))

				Expect(getAsset(p.ID).TagIDs).To(BeEmpty())
			})

			It("keeps existing alt_text when alt_text is omitted", func() {
				p := createPiece("Doc", body)
				Expect(putJSON(p.ID, fiber.Map{"title": "Doc", "content": body, "alt_text": "a bar chart"}).StatusCode).To(Equal(200))

				Expect(putJSON(p.ID, fiber.Map{"title": "Doc renamed", "content": body}).StatusCode).To(Equal(200))

				Expect(getAsset(p.ID).AltText).To(Equal("a bar chart"))
			})
		})

		// alt_text_edited_by_user locks the text against re-extraction,
		// so it may only flip on a real edit.
		Context("alt_text_edited_by_user", func() {
			It("stays false when the PUT echoes the stored alt text back", func() {
				p := createPiece("Doc", body)
				Expect(putJSON(p.ID, fiber.Map{"title": "Doc", "content": body, "alt_text": ""}).StatusCode).To(Equal(200))
				Expect(getAsset(p.ID).AltTextEditedByUser).To(BeFalse())
			})

			It("flips to true when the alt text changes", func() {
				p := createPiece("Doc", body)
				Expect(putJSON(p.ID, fiber.Map{"title": "Doc", "content": body, "alt_text": "a bar chart"}).StatusCode).To(Equal(200))
				Expect(getAsset(p.ID).AltTextEditedByUser).To(BeTrue())
			})
		})

		Context("POST /api/content-bank/assets/tags (bulk)", func() {
			It("adds a tag across multiple assets", func() {
				tagID := createTag("Legal")
				a1 := createPiece("A", body)
				a2 := createPiece("B", body)

				resp := postJSON("/api/content-bank/assets/tags", fiber.Map{
					"asset_ids": []string{a1.ID, a2.ID},
					"add":       []string{tagID},
				})
				Expect(resp.StatusCode).To(Equal(200))
				var updated []models.Asset
				Expect(json.NewDecoder(resp.Body).Decode(&updated)).To(Succeed())
				Expect(updated).To(HaveLen(2))

				Expect(getAsset(a1.ID).TagIDs).To(ConsistOf(tagID))
				Expect(getAsset(a2.ID).TagIDs).To(ConsistOf(tagID))
			})

			It("removes a tag and leaves the others", func() {
				t1 := createTag("Legal")
				t2 := createTag("Q4")
				a := createPiece("A", body)
				Expect(putJSON(a.ID, fiber.Map{"title": "A", "content": body, "tag_ids": []string{t1, t2}}).StatusCode).To(Equal(200))

				resp := postJSON("/api/content-bank/assets/tags", fiber.Map{
					"asset_ids": []string{a.ID},
					"remove":    []string{t1},
				})
				Expect(resp.StatusCode).To(Equal(200))
				Expect(getAsset(a.ID).TagIDs).To(ConsistOf(t2))
			})

			It("does not duplicate a tag already present", func() {
				t1 := createTag("Legal")
				a := createPiece("A", body)
				Expect(putJSON(a.ID, fiber.Map{"title": "A", "content": body, "tag_ids": []string{t1}}).StatusCode).To(Equal(200))

				resp := postJSON("/api/content-bank/assets/tags", fiber.Map{
					"asset_ids": []string{a.ID},
					"add":       []string{t1},
				})
				Expect(resp.StatusCode).To(Equal(200))
				Expect(getAsset(a.ID).TagIDs).To(ConsistOf(t1))
			})

			It("adds and removes in one call", func() {
				t1 := createTag("Legal")
				t2 := createTag("Q4")
				a := createPiece("A", body)
				Expect(putJSON(a.ID, fiber.Map{"title": "A", "content": body, "tag_ids": []string{t1}}).StatusCode).To(Equal(200))

				resp := postJSON("/api/content-bank/assets/tags", fiber.Map{
					"asset_ids": []string{a.ID},
					"add":       []string{t2},
					"remove":    []string{t1},
				})
				Expect(resp.StatusCode).To(Equal(200))
				Expect(getAsset(a.ID).TagIDs).To(ConsistOf(t2))
			})

			It("rejects a request with neither add nor remove", func() {
				a := createPiece("A", body)
				resp := postJSON("/api/content-bank/assets/tags", fiber.Map{"asset_ids": []string{a.ID}})
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("rejects an empty asset_ids", func() {
				t1 := createTag("Legal")
				resp := postJSON("/api/content-bank/assets/tags", fiber.Map{"asset_ids": []string{}, "add": []string{t1}})
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("rejects a tag named in both add and remove", func() {
				t1 := createTag("Legal")
				a := createPiece("A", body)
				resp := postJSON("/api/content-bank/assets/tags", fiber.Map{
					"asset_ids": []string{a.ID},
					"add":       []string{t1},
					"remove":    []string{t1},
				})
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 401 when not authenticated", func() {
				buf, _ := json.Marshal(fiber.Map{"asset_ids": []string{"x"}, "add": []string{"y"}})
				req := httptest.NewRequest("POST", "/api/content-bank/assets/tags", bytes.NewReader(buf))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})
	})

	// ── Delete ───────────────────────────────────────────────────────────────

	Describe("DELETE /api/content-bank/assets/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/content-bank/assets/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("deletes the asset and returns 204", func() {
				p := createPiece("To Delete", `[]`)

				req := httptest.NewRequest("DELETE", "/api/content-bank/assets/"+p.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(204))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("DELETE", "/api/content-bank/assets/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})
})

// ── onSave (embed trigger) ───────────────────────────────────────────────────

var _ = Describe("AssetsHandler onSave embed trigger", Ordered, func() {
	var (
		app          *fiber.App
		db           *bun.DB
		authCookie   *http.Cookie
		onSaveCh     chan string // receives asset IDs whenever onSave fires
		onSaveTenant chan string // receives the tenant id passed to onSave
		reembed      *fakeImageReembedder
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		onSaveCh = make(chan string, 16)
		onSaveTenant = make(chan string, 16)
		// Capture the channels in locals for the onSave closure below. onSave
		// fires in a fire-and-forget goroutine (go h.onSave) that can outlive
		// the spec; reading the package-level vars from there would race with
		// the next spec's BeforeEach reassigning them (a data race under -race).
		saveCh, tenantCh := onSaveCh, onSaveTenant

		app = fiber.New(fiber.Config{
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
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth, nil, nil).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewTagsHandler(tagRepo, auth).Register(app)
		reembed = &fakeImageReembedder{ch: make(chan string, 16)}
		h := handlers.NewAssetsHandler(assetRepo, repository.NewAssetFileRepository(db), repository.NewAssetImageRepository(db), nil, nil, nil, nil, nil, nil, nil, auth, func(assetID, _, _, tenantID string) {
			saveCh <- assetID
			tenantCh <- tenantID
		}, handlers.AssetsOptions{ImageReembed: reembed})
		h.Register(app)

		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
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
		_, err = db.NewDelete().TableExpr("tags").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// waitForSave returns true if an onSave event arrived within the timeout.
	waitForSave := func() bool {
		select {
		case <-onSaveCh:
			return true
		case <-time.After(200 * time.Millisecond):
			return false
		}
	}

	createAsset := func(title, content string) models.Asset {
		body, _ := json.Marshal(fiber.Map{"title": title, "content": content})
		req := httptest.NewRequest("POST", "/api/content-bank/assets", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var a models.Asset
		Expect(json.NewDecoder(resp.Body).Decode(&a)).To(Succeed())
		return a
	}

	updateAsset := func(id, title, content string, extra map[string]any) {
		body := fiber.Map{"title": title, "content": content}
		maps.Copy(body, extra)
		buf, _ := json.Marshal(body)
		req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+id, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(200))
	}

	It("fires onSave on create", func() {
		createAsset("Title", "Content body")
		Expect(waitForSave()).To(BeTrue(), "expected onSave to fire on create")
	})

	// CON-97 file-upload bug: the embed runs in a background goroutine with no
	// request context, so the handler must thread the caller's tenant into
	// onSave — otherwise the scheduler's tenant-scoped writes fail closed.
	It("threads the caller's tenant into onSave", func() {
		createAsset("Title", "Content body")
		Expect(waitForSave()).To(BeTrue())
		select {
		case tid := <-onSaveTenant:
			Expect(tid).To(Equal(models.DefaultTenantID))
		case <-time.After(200 * time.Millisecond):
			Fail("expected the caller's tenant to be passed to onSave")
		}
	})

	It("fires onSave when content changes", func() {
		asset := createAsset("Title", "Original content")
		Expect(waitForSave()).To(BeTrue()) // drain create event

		updateAsset(asset.ID, "Title", "Updated content", nil)
		Expect(waitForSave()).To(BeTrue(), "expected onSave to fire on content change")
	})

	It("fires onSave when title changes", func() {
		asset := createAsset("Original Title", "Content")
		Expect(waitForSave()).To(BeTrue()) // drain create event

		updateAsset(asset.ID, "New Title", "Content", nil)
		Expect(waitForSave()).To(BeTrue(), "expected onSave to fire on title change")
	})

	It("does NOT fire onSave when title and content are unchanged", func() {
		asset := createAsset("Title", "Unchanged content")
		Expect(waitForSave()).To(BeTrue()) // drain create event

		updateAsset(asset.ID, "Title", "Unchanged content", nil)
		Expect(waitForSave()).To(BeFalse(), "onSave must not fire when embed inputs are unchanged")
	})

	It("does NOT fire onSave on a tag-only update", func() {
		// Seed a tag.
		tagBody, _ := json.Marshal(fiber.Map{"name": "Q4", "color": "#3498DB"})
		tagReq := httptest.NewRequest("POST", "/api/tags", bytes.NewReader(tagBody))
		tagReq.Header.Set("Content-Type", "application/json")
		tagReq.AddCookie(authCookie)
		tagResp, err := app.Test(tagReq)
		Expect(err).NotTo(HaveOccurred())
		var t map[string]any
		Expect(json.NewDecoder(tagResp.Body).Decode(&t)).To(Succeed())
		tagID := t["id"].(string)

		asset := createAsset("Title", "Body")
		Expect(waitForSave()).To(BeTrue()) // drain create event

		updateAsset(asset.ID, "Title", "Body", map[string]any{"tag_ids": []string{tagID}})
		Expect(waitForSave()).To(BeFalse(), "onSave must not fire for a tag-only change")
	})

	// Service-ingested assets keep their anchored chunks. The markdown
	// re-embed (onSave) must never run for them, and their content is read-only
	// except for an image's description.
	Context("service-ingested assets (CON-312)", func() {
		// seedIngested creates an asset through the API, then retypes it the way
		// an ingestion upload would have stored it.
		seedIngested := func(assetType, content string) models.Asset {
			a := createAsset("Ingested", "placeholder")
			Expect(waitForSave()).To(BeTrue()) // drain create event
			_, err := db.NewUpdate().TableExpr("assets").
				Set("type = ?", assetType).Set("content = ?", content).
				Where("id = ?", a.ID).Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
			a.Type, a.Content = &assetType, content
			return a
		}

		putAsset := func(id string, body fiber.Map) *http.Response {
			buf, _ := json.Marshal(body)
			req := httptest.NewRequest("PUT", "/api/content-bank/assets/"+id, bytes.NewReader(buf))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}

		waitForReembed := func() bool {
			select {
			case <-reembed.ch:
				return true
			case <-time.After(200 * time.Millisecond):
				return false
			}
		}

		for _, t := range []string{models.AssetTypePDF, models.AssetTypeDocument, models.AssetTypeAudio} {
			It("renames a "+t+" asset without re-embedding it", func() {
				a := seedIngested(t, "extracted text")
				resp := putAsset(a.ID, fiber.Map{"title": "Renamed", "content": "extracted text"})
				Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
				Expect(waitForSave()).To(BeFalse(), "onSave must not replace service-produced chunks")
			})
		}

		It("saves an AUDIO asset with no content in the payload and keeps its transcript", func() {
			a := seedIngested(models.AssetTypeAudio, "the transcript")
			resp := putAsset(a.ID, fiber.Map{"title": "Episode 1"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			var got models.Asset
			Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
			Expect(got.Title).To(Equal("Episode 1"))
			Expect(got.Content).To(Equal("the transcript"))
			Expect(waitForSave()).To(BeFalse())
		})

		It("saves a still-pending AUDIO asset whose content is empty", func() {
			a := seedIngested(models.AssetTypeAudio, "")
			resp := putAsset(a.ID, fiber.Map{"title": "Episode 1", "content": ""})
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		})

		It("rejects a content edit on a PDF asset with 409 content_locked", func() {
			a := seedIngested(models.AssetTypePDF, "extracted text")
			resp := putAsset(a.ID, fiber.Map{"title": "Ingested", "content": "hand-edited"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
			var body map[string]string
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["code"]).To(Equal(models.AssetCodeContentLocked))
			Expect(waitForSave()).To(BeFalse())
		})

		It("re-embeds an edited image description through the image pipeline", func() {
			a := seedIngested(models.AssetTypeImage, "a chart")
			resp := putAsset(a.ID, fiber.Map{"title": "Ingested", "content": "a bar chart of Q3 revenue"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			Expect(waitForReembed()).To(BeTrue(), "expected an image re-embed for a description edit")
			Expect(waitForSave()).To(BeFalse(), "the markdown re-embed would drop the region chunks")
		})

		It("does not re-embed an image on a title-only change", func() {
			a := seedIngested(models.AssetTypeImage, "a chart")
			resp := putAsset(a.ID, fiber.Map{"title": "Renamed", "content": "a chart"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			Expect(waitForReembed()).To(BeFalse())
			Expect(waitForSave()).To(BeFalse())
		})
	})
})

// ── chunk view ────────────────────────────────────────────────────

var _ = Describe("AssetsHandler GET /:id/chunks (CON-312)", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		chunksRepo repository.AssetChunksRepository
		userID     string
	)
	ctx := context.Background()

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

	BeforeEach(func() {
		app = fiber.New(fiber.Config{
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
		chunksRepo = repository.NewAssetChunksRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		h := handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), nil, nil, nil, nil, nil, nil, nil, auth, nil, handlers.AssetsOptions{Chunks: chunksRepo})
		h.Register(app)

		userID = seedTenantUser(db, "Admin", "chunks@example.com", "admin-password").ID
		loginBody, _ := json.Marshal(fiber.Map{"email": "chunks@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		authCookie = loginResp.Cookies()[0]
	})

	AfterEach(func() {
		for _, tbl := range []string{"assets_chunks", "assets", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	// seedAudio stores an AUDIO asset with n time-anchored transcript chunks.
	seedAudio := func(n int) string {
		GinkgoHelper()
		tctx := tenantctx.With(ctx, models.DefaultTenantID)
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		audioType := models.AssetTypeAudio
		_, err = db.NewInsert().Model(&models.Asset{
			ID: id, Title: "Episode", Content: "t", Status: models.AssetStatusReady, Type: &audioType,
			TagIDs: models.StringSlice{}, CreatedBy: userID,
		}).Exec(tctx)
		Expect(err).NotTo(HaveOccurred())
		chunks := make([]models.AssetChunk, n)
		for i := range chunks {
			label := fmt.Sprintf("0:%02d–0:%02d", i*10, i*10+10)
			chunks[i] = models.AssetChunk{
				ID: fmt.Sprintf("%s:%d", id, i), AssetID: id, ChunkIndex: i, Content: fmt.Sprintf("part %d", i),
				TokenCount: 2, Embedding: pgvector.NewHalfVector(make([]float32, 3072)), Model: "m",
				SourceLabel:  &label,
				SourceAnchor: &models.SourceAnchor{Kind: "time", StartMs: int64(i) * 10_000, EndMs: int64(i+1) * 10_000, Provenance: "transcript"},
			}
		}
		Expect(chunksRepo.UpsertChunks(tctx, id, chunks)).To(Succeed())
		return id
	}

	get := func(path string) *http.Response {
		GinkgoHelper()
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	It("returns chunks with their source label and anchor, in order", func() {
		id := seedAudio(3)
		resp := get("/api/content-bank/assets/" + id + "/chunks")
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var body map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body["total"]).To(BeNumerically("==", 3))
		chunks := body["chunks"].([]any)
		Expect(chunks).To(HaveLen(3))
		first := chunks[0].(map[string]any)
		Expect(first["source_label"]).To(Equal("0:00–0:10"))
		anchor := first["source_anchor"].(map[string]any)
		Expect(anchor["kind"]).To(Equal("time"))
		Expect(anchor).To(HaveKeyWithValue("start_ms", BeNumerically("==", 0)))
		Expect(first).NotTo(HaveKey("embedding"))
	})

	It("pages with offset and limit", func() {
		id := seedAudio(5)
		resp := get("/api/content-bank/assets/" + id + "/chunks?offset=2&limit=2")
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var body struct {
			Chunks []models.AssetChunk `json:"chunks"`
			Total  int                 `json:"total"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body.Total).To(Equal(5))
		Expect(body.Chunks).To(HaveLen(2))
		Expect(body.Chunks[0].ChunkIndex).To(Equal(2))
	})

	It("returns 404 for an unknown asset", func() {
		Expect(get("/api/content-bank/assets/nope/chunks").StatusCode).To(Equal(fiber.StatusNotFound))
	})
})

// fakeImageReembedder records image re-embed enqueues.
type fakeImageReembedder struct{ ch chan string }

func (f *fakeImageReembedder) EnqueueReembedImage(_ context.Context, assetID, _ string) error {
	f.ch <- assetID
	return nil
}

// seedGistAndDuration gives an asset a recording file with durationMs and a
// ready workspace analysis with gist.
func seedGistAndDuration(db *bun.DB, assetID, gist string, durationMs int64) {
	GinkgoHelper()
	ctx := tenantctx.With(context.Background(), models.DefaultTenantID)
	files := repository.NewAssetFileRepository(db)
	Expect(files.Upsert(ctx, &models.AssetFile{
		ID: "file-" + assetID, AssetID: assetID, OriginalName: "call.mp3", MimeType: "audio/mpeg",
		SizeBytes: 1024, S3Key: "assets/" + assetID + "/original.mp3",
	})).To(Succeed())
	Expect(files.SetDuration(ctx, assetID, durationMs)).To(Succeed())

	lens, err := repository.NewAssetAnalysisRepository(db).Ensure(ctx, db, assetID, nil)
	Expect(err).NotTo(HaveOccurred())
	_, err = db.NewUpdate().Model((*models.AssetAnalysis)(nil)).
		Set("status = ?", models.AssetAnalysisStatusReady).
		Set("gist = ?", gist).
		Where("id = ?", lens.ID).
		Exec(ctx)
	Expect(err).NotTo(HaveOccurred())
}

// assetListPage is the body of GET /api/content-bank/assets, plus the raw JSON.
type assetListPage struct {
	Assets          []models.AssetListItem              `json:"assets"`
	NextCursor      *string                             `json:"next_cursor"`
	Total           int                                 `json:"total"`
	TotalUnfiltered int                                 `json:"total_unfiltered"`
	IdeasUndecided  int                                 `json:"ideas_undecided_total"`
	Facets          map[string][]models.AssetFacetCount `json:"facets"`
	Groups          []models.AssetGroupCount            `json:"groups"`
	Raw             string                              `json:"-"`
}
