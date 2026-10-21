package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/billing"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// fakeBillingService answers the billing handlers with canned results.
type fakeBillingService struct {
	enabled    bool
	planResult billing.PlanResult
	planErr    error
	webhookErr error
	lastPlan   billing.PlanRequest
	lastSig    string
}

func (f *fakeBillingService) Enabled() bool { return f.enabled }
func (f *fakeBillingService) Billing(context.Context, string) (billing.BillingView, error) {
	return billing.BillingView{TestMode: true}, nil
}
func (f *fakeBillingService) Portal(context.Context, string) (string, time.Time, error) {
	return "https://portal/x", time.Now().Add(time.Hour), nil
}
func (f *fakeBillingService) ChangePlan(_ context.Context, req billing.PlanRequest) (billing.PlanResult, error) {
	f.lastPlan = req
	return f.planResult, f.planErr
}
func (f *fakeBillingService) HandleWebhook(_ context.Context, sig string, _ []byte) error {
	f.lastSig = sig
	return f.webhookErr
}

var _ = Describe("Billing gate and billing routes (CON-322)", Ordered, func() {
	var (
		app  *fiber.App
		db   *bun.DB
		svc  *fakeBillingService
		repo repository.BillingRepository
	)

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

	BeforeEach(func() {
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		userRepo := repository.NewUserRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		repo = repository.NewBillingRepository(db)
		auth := handlers.RequireAuth(repository.NewSessionRepository(db), userRepo, testCookieName, handlers.WithBillingGate(repo.Gated))
		signupSvc := signup.New(db, repository.NewAccountRepository(db), tenantRepo)
		signupSvc.SetBillingRequired(true)
		tenants := handlers.NewTenantsHandler(signupSvc, tenantRepo, testCookieName, false, auth, nil)
		tenants.SetBillingGate(repo.Gated)
		tenants.Register(app)
		// Stands in for any route outside the billing allowlist.
		app.Get("/api/tags", auth, func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })
		svc = &fakeBillingService{enabled: true}
		handlers.NewBillingHandler(svc, userRepo, repository.NewAccountRepository(db), nil, auth).Register(app)
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, table := range []string{"billing_subscriptions", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(table).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().TableExpr("tenants").Where("id <> ?", models.DefaultTenantID).Exec(ctx)
	})

	signupOwner := func(email string) (*http.Cookie, string) {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{
			"tenant": fiber.Map{"name": "Acme " + email},
			"user":   fiber.Map{"name": "Owner", "email": email, "password": "password-owner"},
		})
		req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				return ck, out["tenant"].(map[string]any)["id"].(string)
			}
		}
		Fail("no session cookie")
		return nil, ""
	}

	do := func(method, path string, cookie *http.Cookie, body []byte, headers map[string]string) (int, map[string]any, http.Header) {
		GinkgoHelper()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out, resp.Header
	}

	It("holds a new workspace at 402 until it checks out, and reports the gate as data", func() {
		cookie, tenantID := signupOwner("gate@acme.test")

		code, body, _ := do("GET", "/api/tags", cookie, nil, nil)
		Expect(code).To(Equal(fiber.StatusPaymentRequired))
		Expect(body["error"]).To(Equal("billing_required"))

		code, body, _ = do("GET", "/api/tenants/current", cookie, nil, nil)
		Expect(code).To(Equal(fiber.StatusOK))
		Expect(body["billing_required"]).To(BeTrue())

		Expect(repo.InsertSubscription(context.Background(), nil, &models.BillingSubscription{
			ID: "bs-1", TenantID: tenantID, Provider: models.BillingProviderLemonSqueezy, ProviderSubscriptionID: "1",
			Status: models.SubscriptionStatusOnTrial, TestMode: true, ProviderUpdatedAt: time.Now(),
		})).To(Succeed())

		code, _, _ = do("GET", "/api/tags", cookie, nil, nil)
		Expect(code).To(Equal(fiber.StatusOK))
		_, body, _ = do("GET", "/api/tenants/current", cookie, nil, nil)
		Expect(body["billing_required"]).To(BeFalse())
	})

	It("keeps the plan and billing routes open to a gated workspace", func() {
		cookie, tenantID := signupOwner("plan@acme.test")
		svc.planResult = billing.PlanResult{Checkout: &billing.Checkout{URL: "https://checkout/1", Trial: true}}

		code, body, _ := do("POST", "/api/workspace/plan", cookie, []byte(`{"tier_id":"pro"}`), nil)
		Expect(code).To(Equal(fiber.StatusAccepted))
		Expect(body["checkout"].(map[string]any)["url"]).To(Equal("https://checkout/1"))
		Expect(body["checkout"].(map[string]any)["trial"]).To(BeTrue())
		Expect(svc.lastPlan.TenantID).To(Equal(tenantID))
		Expect(svc.lastPlan.Email).To(Equal("plan@acme.test"))
		Expect(svc.lastPlan.AccountID).NotTo(BeEmpty())

		code, _, hdr := do("POST", "/api/billing/portal", cookie, nil, nil)
		Expect(code).To(Equal(fiber.StatusOK))
		Expect(hdr.Get("Cache-Control")).To(Equal("no-store"))

		code, body, _ = do("GET", "/api/billing", cookie, nil, nil)
		Expect(code).To(Equal(fiber.StatusOK))
		Expect(body["test_mode"]).To(BeTrue())
		Expect(body).To(HaveKeyWithValue("subscription", BeNil()))
	})

	It("maps plan-change errors to the contract codes", func() {
		cookie, _ := signupOwner("errors@acme.test")
		for err, want := range map[error][2]any{
			billing.ErrNotChangeable:          {fiber.StatusConflict, "subscription_not_changeable"},
			billing.ErrTierNotAvailable:       {fiber.StatusConflict, "tier_not_available"},
			billing.ErrUnknownTier:            {fiber.StatusBadRequest, "unknown_tier"},
			billing.ErrPriceMismatch:          {fiber.StatusBadRequest, "price_mismatch"},
			billing.ErrUnavailable:            {fiber.StatusServiceUnavailable, "billing_unavailable"},
			billing.ErrTrialChangeUnavailable: {fiber.StatusConflict, "trial_upgrade_unavailable"},
		} {
			svc.planErr = err
			code, body, _ := do("POST", "/api/workspace/plan", cookie, []byte(`{"tier_id":"max"}`), nil)
			Expect(code).To(Equal(want[0]), err.Error())
			Expect(body["error"]).To(Equal(want[1]))
		}
	})

	It("refuses members", func() {
		member := seedTenantUserWithRole(db, "Mia", "mia@acme.test", "password-mia", string(models.RoleMember))
		_ = member
		body, _ := json.Marshal(fiber.Map{"email": "mia@acme.test", "password": "password-mia"})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		handlers.NewSessionsHandler(repository.NewUserRepository(db), repository.NewAccountRepository(db), repository.NewSessionRepository(db), testCookieName, false, nil).Register(app)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		var cookie *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				cookie = ck
			}
		}
		Expect(cookie).NotTo(BeNil())
		code, _, _ := do("GET", "/api/billing", cookie, nil, nil)
		Expect(code).To(Equal(fiber.StatusForbidden))
		code, _, _ = do("POST", "/api/billing/portal", cookie, nil, nil)
		Expect(code).To(Equal(fiber.StatusForbidden))
	})

	It("answers the webhook by outcome", func() {
		code, _, _ := do("POST", "/api/webhooks/lemonsqueezy", nil, []byte(`{}`), map[string]string{"X-Signature": "abc"})
		Expect(code).To(Equal(fiber.StatusOK))
		Expect(svc.lastSig).To(Equal("abc"))

		svc.webhookErr = billing.ErrInvalidSignature
		code, _, _ = do("POST", "/api/webhooks/lemonsqueezy", nil, []byte(`{}`), nil)
		Expect(code).To(Equal(fiber.StatusUnauthorized))

		svc.webhookErr = context.DeadlineExceeded
		code, _, _ = do("POST", "/api/webhooks/lemonsqueezy", nil, []byte(`{}`), nil)
		Expect(code).To(Equal(fiber.StatusInternalServerError))

		svc.enabled = false
		code, body, _ := do("POST", "/api/webhooks/lemonsqueezy", nil, []byte(`{}`), nil)
		Expect(code).To(Equal(fiber.StatusServiceUnavailable))
		Expect(body["error"]).To(Equal("billing_unavailable"))
	})
})
