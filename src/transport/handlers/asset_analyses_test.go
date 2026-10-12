package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// analysisTriggers records the campaign handler's analysis calls.
type analysisTriggers struct {
	attached map[string][]string
	detached map[string][]string
	rebriefs []string
}

func newAnalysisTriggers() *analysisTriggers {
	return &analysisTriggers{attached: map[string][]string{}, detached: map[string][]string{}}
}

func (a *analysisTriggers) ScheduleAttached(_ context.Context, _, campaignID string, ids []string) {
	a.attached[campaignID] = append(a.attached[campaignID], ids...)
}

func (a *analysisTriggers) Detach(_ context.Context, _, campaignID string, ids []string) {
	a.detached[campaignID] = append(a.detached[campaignID], ids...)
}

func (a *analysisTriggers) ScheduleRebrief(_ context.Context, _, campaignID string) {
	a.rebriefs = append(a.rebriefs, campaignID)
}

// analysisRetries answers Retry with err and records the lenses retried.
type analysisRetries struct {
	err     error
	retried []string
}

func (r *analysisRetries) Retry(_ context.Context, _, assetID string, campaignID *string) error {
	key := assetID
	if campaignID != nil {
		key += "/" + *campaignID
	}
	r.retried = append(r.retried, key)
	return r.err
}

type analysisWire struct {
	ID           string          `json:"id"`
	CampaignID   *string         `json:"campaign_id"`
	CampaignName *string         `json:"campaign_name"`
	Status       string          `json:"status"`
	Summary      string          `json:"summary"`
	KeyPoints    []string        `json:"key_points"`
	Commitments  json.RawMessage `json:"commitments"`
	Relevance    *struct {
		Score int `json:"score"`
	} `json:"relevance"`
	Ideas []struct {
		Title           string  `json:"title"`
		Origin          string  `json:"origin"`
		AssetAnalysisID *string `json:"asset_analysis_id"`
	} `json:"ideas"`
}

var _ = Describe("AssetAnalysesHandler", Ordered, func() {
	var (
		app       *fiber.App
		db        *bun.DB
		admin     *http.Cookie
		adminUser *models.User
		triggers  *analysisTriggers
		retries   *analysisRetries
		analyses  repository.AssetAnalysisRepository
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	request := func(cookie *http.Cookie, method, path string, body any) *http.Response {
		GinkgoHelper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req, 5000)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	login := func(email, password string) *http.Cookie {
		GinkgoHelper()
		resp := request(nil, "POST", "/api/sessions", fiber.Map{"email": email, "password": password})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		return resp.Cookies()[0]
	}

	BeforeEach(func() {
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		accountRepo := repository.NewAccountRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		tagRepo := repository.NewTagRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		fileRepo := repository.NewAssetFileRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, fileRepo)
		ideaRepo := repository.NewIdeaRepository(db)
		analyses = repository.NewAssetAnalysisRepository(db)
		triggers = newAnalysisTriggers()
		retries = &analysisRetries{}

		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, accountRepo, sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewTenantsHandler(signup.New(db, accountRepo, tenantRepo, nil), tenantRepo, testCookieName, false, auth, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{Analysis: triggers}).Register(app)
		handlers.NewIdeasHandler(ideas.New(ideaRepo, userRepo), auth, nil).Register(app)
		handlers.NewAssetAnalysesHandler(analyses, ideaRepo, assetRepo, campaignRepo, retries, auth).Register(app)
		handlers.NewAssetsHandler(assetRepo, fileRepo, repository.NewAssetImageRepository(db), nil, db, nil, nil, nil, nil, nil, auth, nil,
			handlers.AssetsOptions{Analyses: analyses}).Register(app)

		adminUser = seedTenantUser(db, "Admin", "admin@example.com", "admin-password")
		admin = login("admin@example.com", "admin-password")
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"ideas", "asset_analysis_runs", "asset_analyses", "assets", "campaigns", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(tenantCtx())
	})

	seedAsset := func(id, assetType string) {
		GinkgoHelper()
		now := time.Now().UTC()
		_, err := db.NewInsert().Model(&models.Asset{
			ID: id, Title: "Asset " + id, Status: models.AssetStatusReady, Type: new(assetType),
			TagIDs: models.StringSlice{}, CreatedBy: adminUser.ID, CreatedAt: now, UpdatedAt: now,
		}).Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
	}

	createCampaign := func(name string, assetIDs ...string) string {
		GinkgoHelper()
		resp := request(admin, "POST", "/api/campaigns", fiber.Map{
			"name": name, "campaign_type_id": "Uk", "key_messages": "first", "asset_ids": assetIDs,
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var c models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
		return c.ID
	}

	// saveLens stores a finished lens with ideas titled titles.
	saveLens := func(assetID string, campaignID *string, status models.AssetAnalysisStatus, score int, titles ...string) {
		GinkgoHelper()
		ctx := tenantCtx()
		lens, err := analyses.Ensure(ctx, db, assetID, campaignID)
		Expect(err).NotTo(HaveOccurred())
		lens.Status, lens.Summary = status, "summary "+assetID
		lens.Result = models.AssetAnalysisResult{KeyPoints: []string{"k"}}
		if campaignID != nil {
			lens.Result.Relevance = &models.AnalysisRelevance{Score: score}
		}
		var rows []models.Idea
		for _, t := range titles {
			id, err := models.NewID()
			Expect(err).NotTo(HaveOccurred())
			rows = append(rows, models.Idea{ID: id, Title: t, CampaignID: campaignID, Origin: models.IdeaOriginAssetAnalysis,
				AssetAnalysisID: new(lens.ID), CreatedByName: "Ogen"})
		}
		Expect(db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			if _, err := analyses.LockLens(ctx, tx, assetID, campaignID); err != nil {
				return err
			}
			return analyses.SaveResult(ctx, tx, lens, rows)
		})).To(Succeed())
	}

	decodeAnalyses := func(resp *http.Response) []analysisWire {
		GinkgoHelper()
		Expect(resp.StatusCode).To(Equal(200))
		var body struct {
			Analyses []analysisWire `json:"analyses"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		return body.Analyses
	}

	It("requires auth", func() {
		Expect(request(nil, "GET", "/api/content-bank/assets/x/analyses", nil).StatusCode).To(Equal(401))
	})

	It("lists the workspace lens first, then campaign lenses, with their ideas", func() {
		seedAsset("doc-1", models.AssetTypeDocument)
		camp := createCampaign("Launch", "doc-1")
		saveLens("doc-1", nil, models.AssetAnalysisStatusReady, 0, "ws idea")
		saveLens("doc-1", &camp, models.AssetAnalysisStatusReady, 80, "launch idea", "second launch idea")

		got := decodeAnalyses(request(admin, "GET", "/api/content-bank/assets/doc-1/analyses", nil))
		Expect(got).To(HaveLen(2))
		Expect(got[0].CampaignID).To(BeNil())
		Expect(got[0].Relevance).To(BeNil())
		Expect(got[0].Commitments).To(BeNil(), "meeting fields are omitted for documents")
		Expect(got[0].Ideas).To(HaveLen(1))
		Expect(got[1].CampaignName).To(HaveValue(Equal("Launch")))
		Expect(got[1].Relevance.Score).To(Equal(80))
		Expect(got[1].Ideas).To(HaveLen(2))
		Expect(got[1].Ideas[0].Origin).To(Equal("asset_analysis"))

		resp := request(admin, "GET", "/api/content-bank/assets/doc-1/analysis?campaign_id="+camp, nil)
		Expect(resp.StatusCode).To(Equal(200))
		var one analysisWire
		Expect(json.NewDecoder(resp.Body).Decode(&one)).To(Succeed())
		Expect(*one.CampaignID).To(Equal(camp))

		Expect(request(admin, "GET", "/api/content-bank/assets/doc-1/analysis?campaign_id=nope", nil).StatusCode).To(Equal(404))
		Expect(request(admin, "GET", "/api/content-bank/assets/missing/analyses", nil).StatusCode).To(Equal(404))
	})

	It("ranks a campaign's lenses by relevance and hides a deleted campaign", func() {
		seedAsset("low", models.AssetTypePDF)
		seedAsset("high", models.AssetTypePDF)
		camp := createCampaign("Launch", "low", "high")
		saveLens("low", &camp, models.AssetAnalysisStatusReady, 20)
		saveLens("high", &camp, models.AssetAnalysisStatusReady, 90)

		got := decodeAnalyses(request(admin, "GET", "/api/campaigns/"+camp+"/asset-analyses", nil))
		Expect(got).To(HaveLen(2))
		Expect(got[0].Summary).To(Equal("summary high"))
		Expect(got[0].Ideas).To(BeNil(), "ideas are not embedded in the campaign list")

		_, err := db.NewUpdate().TableExpr("campaigns").Set("deleted_at = now()").Where("id = ?", camp).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(request(admin, "GET", "/api/campaigns/"+camp+"/asset-analyses", nil).StatusCode).To(Equal(404))
	})

	It("retries only failed or skipped lenses of analysable assets", func() {
		seedAsset("doc-1", models.AssetTypeDocument)
		seedAsset("img-1", models.AssetTypeImage)

		Expect(request(admin, "POST", "/api/content-bank/assets/doc-1/analysis/retry", nil).StatusCode).To(Equal(fiber.StatusAccepted))
		Expect(retries.retried).To(Equal([]string{"doc-1"}))

		retries.err = assetanalysis.ErrNotRetryable
		resp := request(admin, "POST", "/api/content-bank/assets/doc-1/analysis/retry", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		var body map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body["code"]).To(Equal("analysis_not_retryable"))

		Expect(request(admin, "POST", "/api/content-bank/assets/img-1/analysis/retry", nil).StatusCode).To(Equal(fiber.StatusUnprocessableEntity))
		Expect(request(admin, "POST", "/api/content-bank/assets/missing/analysis/retry", nil).StatusCode).To(Equal(404))
	})

	It("filters ideas by origin and asset and names their source asset", func() {
		seedAsset("doc-1", models.AssetTypeDocument)
		saveLens("doc-1", nil, models.AssetAnalysisStatusReady, 0, "from the doc")
		Expect(request(admin, "POST", "/api/ideas", fiber.Map{"title": "typed by hand"}).StatusCode).To(Equal(fiber.StatusCreated))

		list := func(query string) []map[string]any {
			GinkgoHelper()
			resp := request(admin, "GET", "/api/ideas"+query, nil)
			Expect(resp.StatusCode).To(Equal(200))
			var body struct {
				Ideas []map[string]any `json:"ideas"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			return body.Ideas
		}
		fromDoc := list("?origin=asset_analysis")
		Expect(fromDoc).To(HaveLen(1))
		Expect(fromDoc[0]["source_asset"]).To(HaveKeyWithValue("title", "Asset doc-1"))
		Expect(list("?origin=manual")).To(HaveLen(1))
		Expect(list("?asset_id=doc-1")).To(HaveLen(1))
		Expect(request(admin, "GET", "/api/ideas?origin=robot", nil).StatusCode).To(Equal(400))
	})

	It("shows the workspace analysis status on asset reads", func() {
		seedAsset("doc-1", models.AssetTypeDocument)
		saveLens("doc-1", nil, models.AssetAnalysisStatusFailed, 0)

		resp := request(admin, "GET", "/api/content-bank/assets/doc-1", nil)
		Expect(resp.StatusCode).To(Equal(200))
		var asset map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&asset)).To(Succeed())
		Expect(asset["analysis_status"]).To(Equal("failed"))

		resp = request(admin, "GET", "/api/content-bank/assets/doc-1/analysis", nil)
		Expect(resp.StatusCode).To(Equal(200))
		var lens map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&lens)).To(Succeed())
		Expect(lens).To(HaveKeyWithValue("ideas", BeEmpty()), "a lens without ideas reads back an empty array, not a missing field")
	})

	It("keeps campaign lenses in step with attach, detach and brief edits", func() {
		seedAsset("a", models.AssetTypeDocument)
		seedAsset("b", models.AssetTypeDocument)
		camp := createCampaign("Launch", "a")
		Expect(triggers.attached[camp]).To(Equal([]string{"a"}))

		Expect(request(admin, "POST", "/api/campaigns/"+camp+"/assets", fiber.Map{"asset_ids": []string{"b"}}).StatusCode).To(Equal(200))
		Expect(triggers.attached[camp]).To(Equal([]string{"a", "b"}))

		Expect(request(admin, "DELETE", "/api/campaigns/"+camp+"/assets/a", nil).StatusCode).To(Equal(200))
		Expect(triggers.detached[camp]).To(Equal([]string{"a"}))
		Expect(triggers.rebriefs).To(BeEmpty())

		update := func(body fiber.Map) {
			GinkgoHelper()
			body["name"], body["campaign_type_id"] = "Launch", "Uk"
			Expect(request(admin, "PUT", "/api/campaigns/"+camp, body).StatusCode).To(Equal(200))
		}
		update(fiber.Map{"key_messages": "first", "asset_ids": []string{"a"}})
		Expect(triggers.attached[camp]).To(Equal([]string{"a", "b", "a"}))
		Expect(slices.Contains(triggers.detached[camp], "b")).To(BeTrue())
		Expect(triggers.rebriefs).To(BeEmpty(), "an unchanged brief does not re-run anything")

		update(fiber.Map{"key_messages": "sharper"})
		Expect(triggers.rebriefs).To(Equal([]string{camp}))
	})

	It("does not reveal another workspace's analyses", func() {
		seedAsset("doc-1", models.AssetTypeDocument)
		saveLens("doc-1", nil, models.AssetAnalysisStatusReady, 0)
		resp := request(nil, "POST", "/api/tenants", fiber.Map{
			"tenant": fiber.Map{"name": "Other"},
			"user":   fiber.Map{"name": "Owner", "email": "other@example.com", "password": "password-owner"},
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var other *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				other = ck
			}
		}
		Expect(other).NotTo(BeNil())

		Expect(request(other, "GET", "/api/content-bank/assets/doc-1/analyses", nil).StatusCode).To(Equal(404))
		Expect(request(other, "GET", "/api/content-bank/assets/doc-1/analysis", nil).StatusCode).To(Equal(404))
		Expect(request(other, "POST", "/api/content-bank/assets/doc-1/analysis/retry", nil).StatusCode).To(Equal(404))
	})
})
