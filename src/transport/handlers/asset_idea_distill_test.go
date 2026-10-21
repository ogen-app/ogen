package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

// fakeDistiller answers RequestDistill with run and err and records inputs.
type fakeDistiller struct {
	run    *models.IdeaDistillRun
	err    error
	inputs []assetanalysis.DistillInput
}

func (f *fakeDistiller) RequestDistill(_ context.Context, in assetanalysis.DistillInput) (*models.IdeaDistillRun, error) {
	f.inputs = append(f.inputs, in)
	return f.run, f.err
}

func (f *fakeDistiller) ListDistills(context.Context, string, string, *string) ([]models.IdeaDistillRun, error) {
	if f.run == nil {
		return []models.IdeaDistillRun{}, nil
	}
	return []models.IdeaDistillRun{*f.run}, nil
}

// distillJobs records queued distill runs; the analysis jobs are no-ops.
type distillJobs struct{ runs []string }

func (j *distillJobs) EnqueueAnalyzeAsset(context.Context, string, string, *string, string, time.Time) error {
	return nil
}
func (j *distillJobs) EnqueueReanalyzeCampaign(context.Context, string, string, time.Time) error {
	return nil
}
func (j *distillJobs) EnqueueDistillIdeas(_ context.Context, _, runID string) error {
	j.runs = append(j.runs, runID)
	return nil
}

var _ = Describe("AssetIdeaDistillHandler", Ordered, func() {
	var (
		app       *fiber.App
		db        *bun.DB
		admin     *http.Cookie
		adminUser *models.User
		fake      *fakeDistiller
		svc       *assetanalysis.Service
		jobs      *distillJobs
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

	// mount serves the distill routes over distiller.
	mount := func(distiller handlers.IdeaDistiller) {
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tagRepo := repository.NewTagRepository(db)
		assetRepo := repository.NewAssetRepository(db, tagRepo, repository.NewAssetFileRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewAssetIdeaDistillHandler(assetRepo, distiller, auth, nil).Register(app)
		handlers.NewAssetAnalysesHandler(analyses, repository.NewIdeaRepository(db), assetRepo,
			repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db)),
			&analysisRetries{}, auth).Register(app)
		resp := request(nil, "POST", "/api/sessions", fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		admin = resp.Cookies()[0]
	}

	BeforeEach(func() {
		analyses = repository.NewAssetAnalysisRepository(db)
		adminUser = seedTenantUser(db, "Admin", "admin@example.com", "admin-password")
		fake = &fakeDistiller{}
		mount(fake)
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"ideas", "idea_distill_ledger", "idea_distill_runs", "asset_analysis_runs", "asset_analyses", "assets", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(tenantCtx())
	})

	seedAsset := func(id, assetType, content string) {
		GinkgoHelper()
		now := time.Now().UTC()
		_, err := db.NewInsert().Model(&models.Asset{
			ID: id, Title: "Asset " + id, Content: content, Status: models.AssetStatusReady, Type: new(assetType),
			TagIDs: models.StringSlice{}, CreatedBy: adminUser.ID, CreatedAt: now, UpdatedAt: now,
		}).Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())
	}

	decodeCode := func(resp *http.Response) map[string]any {
		GinkgoHelper()
		var body map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		return body
	}

	It("requires auth", func() {
		Expect(request(nil, "POST", "/api/content-bank/assets/x/ideas/distill", nil).StatusCode).To(Equal(401))
		Expect(request(nil, "GET", "/api/content-bank/assets/x/ideas/distills", nil).StatusCode).To(Equal(401))
	})

	It("queues a run with the focus, count, lens and requester", func() {
		seedAsset("doc-1", models.AssetTypeDocument, "")
		fake.run = &models.IdeaDistillRun{ID: "run-1", AssetID: "doc-1", Status: models.IdeaDistillStatusPending, Focus: "CFOs", Count: 4}

		resp := request(admin, "POST", "/api/content-bank/assets/doc-1/ideas/distill?campaign_id=camp-1", fiber.Map{"focus": "CFOs", "count": 4})
		Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))
		var body struct {
			Run struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Count  int    `json:"count"`
			} `json:"run"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body.Run.ID).To(Equal("run-1"))
		Expect(body.Run.Status).To(Equal("pending"))
		Expect(body.Run.Count).To(Equal(4))

		in := fake.inputs[0]
		Expect(in.Focus).To(Equal("CFOs"))
		Expect(in.Count).To(Equal(4))
		Expect(in.CampaignID).To(HaveValue(Equal("camp-1")))
		Expect(in.UserID).To(HaveValue(Equal(adminUser.ID)))

		Expect(request(admin, "POST", "/api/content-bank/assets/doc-1/ideas/distill", nil).StatusCode).To(Equal(fiber.StatusAccepted), "an empty body is valid")
		Expect(fake.inputs[1].Count).To(Equal(0))
		Expect(fake.inputs[1].CampaignID).To(BeNil())
	})

	It("maps refusals to their statuses", func() {
		seedAsset("doc-1", models.AssetTypeDocument, "")
		seedAsset("img-1", models.AssetTypeImage, "")
		path := "/api/content-bank/assets/doc-1/ideas/distill"

		Expect(request(admin, "POST", "/api/content-bank/assets/missing/ideas/distill", nil).StatusCode).To(Equal(404))
		Expect(request(admin, "POST", "/api/content-bank/assets/img-1/ideas/distill", nil).StatusCode).To(Equal(422))

		fake.err = &assetanalysis.DistillInputError{Msg: "count must be between 3 and 10"}
		Expect(request(admin, "POST", path, fiber.Map{"count": 2}).StatusCode).To(Equal(400))

		fake.err = assetanalysis.ErrAnalysisNotReady
		resp := request(admin, "POST", path, nil)
		Expect(resp.StatusCode).To(Equal(409))
		Expect(decodeCode(resp)["code"]).To(Equal("analysis_not_ready"))

		fake.err = assetanalysis.ErrDistillInProgress
		fake.run = &models.IdeaDistillRun{ID: "run-1", Status: models.IdeaDistillStatusRunning}
		resp = request(admin, "POST", path, nil)
		Expect(resp.StatusCode).To(Equal(409))
		body := decodeCode(resp)
		Expect(body["code"]).To(Equal("distill_in_progress"))
		Expect(body["run"]).To(HaveKeyWithValue("id", "run-1"))

		fake.err = assetanalysis.ErrUnavailable
		Expect(request(admin, "POST", path, nil).StatusCode).To(Equal(503))

		Expect(request(admin, "POST", path, "not an object").StatusCode).To(Equal(400))
	})

	It("distills end to end and lists the run", func() {
		seedAsset("md-1", models.AssetTypeMarkdown, "Some text about pricing.")
		lens, err := analyses.Ensure(tenantCtx(), db, "md-1", nil)
		Expect(err).NotTo(HaveOccurred())
		lens.Status, lens.Summary = models.AssetAnalysisStatusReady, "summary"
		Expect(db.RunInTx(tenantCtx(), nil, func(ctx context.Context, tx bun.Tx) error {
			return analyses.SaveResult(ctx, tx, lens, nil)
		})).To(Succeed())

		svc = assetanalysis.New(db, assetanalysis.Repos{
			Analyses:  analyses,
			Assets:    repository.NewAssetRepository(db, repository.NewTagRepository(db), repository.NewAssetFileRepository(db)),
			Chunks:    repository.NewAssetChunksRepository(db),
			Campaigns: repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db)),
			Posts:     repository.NewPostRepository(db),
			Ideas:     repository.NewIdeaRepository(db),
			Brands:    repository.NewBrandRepository(db),
			Tenants:   repository.NewTenantRepository(db),
			Distills:  repository.NewIdeaDistillRepository(db),
		}, nil, nil)
		jobs = &distillJobs{}
		svc.SetJobs(jobs, time.Minute, time.Minute)
		svc.SetDistiller(func(context.Context, asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error) {
			return &asset_analysis.DistillResponse{ModelID: "m", Ideas: []asset_analysis.Idea{{Title: "Pricing, from the CFO's side", Angle: "objection"}}}, nil
		})
		mount(svc)

		resp := request(admin, "POST", "/api/content-bank/assets/md-1/ideas/distill", fiber.Map{"focus": "CFOs"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))
		Expect(jobs.runs).To(HaveLen(1))
		Expect(svc.Distill(tenantCtx(), models.DefaultTenantID, jobs.runs[0])).To(Succeed())

		resp = request(admin, "GET", "/api/content-bank/assets/md-1/ideas/distills", nil)
		Expect(resp.StatusCode).To(Equal(200))
		var list struct {
			Runs []struct {
				ID         string `json:"id"`
				Status     string `json:"status"`
				IdeasCount int    `json:"ideas_count"`
				Focus      string `json:"focus"`
			} `json:"runs"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&list)).To(Succeed())
		Expect(list.Runs).To(HaveLen(1))
		Expect(list.Runs[0].Status).To(Equal("ready"))
		Expect(list.Runs[0].IdeasCount).To(Equal(1))
		Expect(list.Runs[0].Focus).To(Equal("CFOs"))

		resp = request(admin, "GET", "/api/content-bank/assets/md-1/analysis", nil)
		Expect(resp.StatusCode).To(Equal(200))
		var one struct {
			Ideas []struct {
				Title        string  `json:"title"`
				DistillRunID *string `json:"distill_run_id"`
			} `json:"ideas"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&one)).To(Succeed())
		Expect(one.Ideas).To(HaveLen(1))
		Expect(one.Ideas[0].DistillRunID).To(HaveValue(Equal(list.Runs[0].ID)))

		resp = request(admin, "POST", "/api/content-bank/assets/md-1/ideas/distill", fiber.Map{"count": 11})
		Expect(resp.StatusCode).To(Equal(400))
	})
})
