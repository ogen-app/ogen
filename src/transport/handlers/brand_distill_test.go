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
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

// fakeBrandDistiller answers with what each test sets and records what it got.
type fakeBrandDistiller struct {
	run     *models.BrandDistillRun
	err     error
	request branddistill.RequestInput
	commit  branddistill.CommitInput
	result  *branddistill.CommitResult
}

func (f *fakeBrandDistiller) Request(_ context.Context, in branddistill.RequestInput) (*models.BrandDistillRun, error) {
	f.request = in
	return f.run, f.err
}

func (f *fakeBrandDistiller) Get(context.Context, string, string) (*models.BrandDistillRun, error) {
	return f.run, f.err
}

func (f *fakeBrandDistiller) List(context.Context, string) ([]models.BrandDistillRun, error) {
	if f.run == nil {
		return nil, f.err
	}
	return []models.BrandDistillRun{*f.run}, f.err
}

func (f *fakeBrandDistiller) Refine(context.Context, string, string, *string, []branddistill.Answer) (*models.BrandDistillRun, error) {
	return f.run, f.err
}

func (f *fakeBrandDistiller) Commit(_ context.Context, in branddistill.CommitInput) (*branddistill.CommitResult, error) {
	f.commit = in
	return f.result, f.err
}

func (f *fakeBrandDistiller) Discard(context.Context, string, string) (*models.BrandDistillRun, error) {
	return f.run, f.err
}

var _ = Describe("Brand distillation", Ordered, Serial, func() {
	var (
		app    *fiber.App
		db     *bun.DB
		cookie *http.Cookie
		fake   *fakeBrandDistiller
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
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		fake = &fakeBrandDistiller{}
		handlers.NewBrandDistillHandler(fake, repository.NewBrandRepository(db), auth, nil).Register(app)
		seedTenantUser(db, "Ada", "distill@example.com", "admin-password")
		cookie = loginCookie(app, "distill@example.com", "admin-password")
	})

	AfterEach(func() {
		for _, tbl := range []string{"sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	do := func(method, path string, body any) (*http.Response, map[string]any) {
		GinkgoHelper()
		var buf bytes.Buffer
		if body != nil {
			Expect(json.NewEncoder(&buf).Encode(body)).To(Succeed())
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp, out
	}

	It("queues a run and answers 202 with it", func() {
		fake.run = &models.BrandDistillRun{ID: "run-1", Status: models.BrandDistillStatusPending, Sources: []models.DistillSource{}, Answers: []models.DistillAnswer{}}
		resp, body := do("POST", "/api/brand/distills", fiber.Map{
			"websiteUrl": "https://acme.com", "assetIds": []string{"a1"},
			"answers": []fiber.Map{{"questionId": "q_offer", "question": "What do you sell?", "answer": "Coffee"}},
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))
		Expect(body["id"]).To(Equal("run-1"))
		Expect(body["status"]).To(Equal("pending"))
		Expect(body).To(HaveKeyWithValue("failure", BeNil()))
		Expect(fake.request.WebsiteURL).To(Equal("https://acme.com"))
		Expect(fake.request.AssetIDs).To(Equal([]string{"a1"}))
		Expect(fake.request.Answers).To(HaveLen(1))
		Expect(fake.request.UserID).NotTo(BeNil())
	})

	It("answers an open run with 409 and that run", func() {
		fake.run, fake.err = &models.BrandDistillRun{ID: "open-1", Status: models.BrandDistillStatusReading}, branddistill.ErrInProgress
		resp, body := do("POST", "/api/brand/distills", fiber.Map{"websiteUrl": "https://acme.com"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		Expect(body["code"]).To(Equal("distill_in_progress"))
		Expect(body["run"]).To(HaveKeyWithValue("id", "open-1"))
	})

	It("answers a bad request with 422 and its code", func() {
		fake.err = &branddistill.InputError{Code: branddistill.CodeInvalidURL, Msg: "only http(s) urls are supported"}
		resp, body := do("POST", "/api/brand/distills", fiber.Map{"websiteUrl": "ftp://x"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnprocessableEntity))
		Expect(body["code"]).To(Equal("invalid_url"))
	})

	It("shows a failed run's failure", func() {
		fake.run = &models.BrandDistillRun{ID: "run-2", Status: models.BrandDistillStatusFailed, FailureCode: "no_usable_sources", FailureReason: "Not enough text"}
		resp, body := do("GET", "/api/brand/distills/run-2", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(body["failure"]).To(Equal(map[string]any{"code": "no_usable_sources", "reason": "Not enough text"}))
	})

	It("answers an unknown run with 404", func() {
		fake.err = sql.ErrNoRows
		resp, _ := do("GET", "/api/brand/distills/nope", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("serves the starter questions", func() {
		resp, body := do("GET", "/api/brand/distills/starter-questions", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(body["questions"]).To(HaveLen(7))
	})

	It("commits the kept items and answers with the brand, what was applied and what was skipped", func() {
		fake.result = &branddistill.CommitResult{
			Run: &models.BrandDistillRun{ID: "run-3", Status: models.BrandDistillStatusCommitted},
			Result: &repository.DistilledResult{
				Voices:     map[string]string{"v1": "voice-9"},
				Audiences:  map[string]string{},
				Facts:      map[string]string{"f1": "fact-9"},
				Guardrails: []string{"g:mayClaim:1"},
				Skipped:    []repository.SkippedItem{{ProposalID: "f2", Reason: "duplicate"}},
			},
		}
		resp, body := do("POST", "/api/brand/distills/run-3/commit", fiber.Map{
			"voices":     []fiber.Map{{"proposalId": "v1", "name": "Warm", "samples": []string{"We roast light."}, "rules": fiber.Map{"emoji": "never"}}},
			"guardrails": fiber.Map{"mayClaim": []fiber.Map{{"proposalId": "g:mayClaim:1", "text": "Roasted to order"}}, "disclaimer": fiber.Map{"proposalId": "g:disclaimer", "text": "Prices include VAT."}},
			"facts": []fiber.Map{
				{"proposalId": "f1", "statement": "Roasted weekly.", "subject": "us", "kind": "documented", "expiresAt": "2026-12-31", "source": "Brand book"},
				{"proposalId": "f2", "statement": "Founded in 2015.", "subject": "us", "kind": "documented"},
			},
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(body["brand"]).To(HaveKey("voices"))
		Expect(body["applied"]).To(Equal(map[string]any{
			"voices":     []any{map[string]any{"proposalId": "v1", "id": "voice-9"}},
			"audiences":  []any{},
			"guardrails": []any{"g:mayClaim:1"},
			"facts":      []any{map[string]any{"proposalId": "f1", "id": "fact-9"}},
		}))
		Expect(body["skipped"]).To(Equal([]any{map[string]any{"proposalId": "f2", "reason": "duplicate"}}))

		in := fake.commit
		Expect(in.RunID).To(Equal("run-3"))
		Expect(in.Voices[0].Rules.Emoji).To(Equal("never"))
		Expect(in.Disclaimer).NotTo(BeNil())
		Expect(in.Facts[0].ExpiresAt.Format("2006-01-02")).To(Equal("2026-12-31"))
		Expect(in.Facts[1].ExpiresAt).To(BeNil())
	})

	It("refuses a bad expiry date before committing", func() {
		resp, body := do("POST", "/api/brand/distills/run-3/commit", fiber.Map{
			"facts": []fiber.Map{{"proposalId": "f1", "statement": "x", "expiresAt": "31/12/2026"}},
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnprocessableEntity))
		Expect(body["proposalId"]).To(Equal("f1"))
		Expect(fake.commit.RunID).To(BeEmpty())
	})

	It("maps a committed run to 409 and discards with 204", func() {
		fake.err = branddistill.ErrCommitted
		resp, body := do("POST", "/api/brand/distills/run-3/answers", fiber.Map{"answers": []fiber.Map{{"questionId": "q", "answer": "a"}}})
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		Expect(body["code"]).To(Equal("run_committed"))

		fake.err, fake.run = nil, &models.BrandDistillRun{ID: "run-4", Status: models.BrandDistillStatusDiscarded}
		resp, _ = do("DELETE", "/api/brand/distills/run-4", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusNoContent))
	})
})
