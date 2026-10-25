package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// inviteLinks captures the tester-invite links the usecase would email.
type inviteLinks struct {
	mu    sync.Mutex
	links []string
}

func (l *inviteLinks) EnqueueTenantInviteEmailTx(_ context.Context, _ *sql.Tx, _ string, _ int, _, link, _, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.links = append(l.links, link)
	return nil
}

func (l *inviteLinks) lastToken() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	u, err := url.Parse(l.links[len(l.links)-1])
	Expect(err).NotTo(HaveOccurred())
	return u.Query().Get("invite")
}

var _ = Describe("Signup access (tester invites)", Ordered, func() {
	var (
		app    *fiber.App
		db     *bun.DB
		access *signupaccess.Service
		links  *inviteLinks
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
		links = &inviteLinks{}
		userRepo := repository.NewUserRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		accountRepo := repository.NewAccountRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		invites := repository.NewTenantInviteRepository(db)
		versions := repository.NewTenantTierVersionRepository(db)
		access = signupaccess.New(signupaccess.Deps{
			DB: db, Invites: invites, Settings: repository.NewSignupSettingsRepository(db), Accounts: accountRepo,
			Tiers: repository.NewTenantTierRepository(db), TierVersions: versions, Groups: repository.NewTenantGroupRepository(db),
			Emails: links, AppBaseURL: "https://app",
		})
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		signupSvc := signup.New(db, accountRepo, tenantRepo)
		signupSvc.SetAccess(access, signup.InviteDeps{Invites: invites, TierVersions: versions, Assignments: repository.NewTenantTierAssignmentRepository(db)})
		handlers.NewTenantsHandler(signupSvc, tenantRepo, testCookieName, false, auth, nil).Register(app)
		handlers.NewSignupAccessHandler(access).Register(app)
		ws := handlers.NewWorkspacesHandler(db, repository.NewWorkspaceRepository(db), userRepo, accountRepo, tenantRepo, sessionRepo, nil, auth, nil)
		ws.SetWorkspaceCap(access)
		ws.Register(app)
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, table := range []string{"sessions", "users", "tenant_tier_assignments", "tenant_group_assignments"} {
			_, _ = db.NewDelete().TableExpr(table).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewUpdate().TableExpr("tenants").Set("invite_id = NULL").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("tenant_invites").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(ctx)
		_, _ = db.NewUpdate().TableExpr("platform_signup_settings").Set("mode = 'open'").Set("max_owned_workspaces_per_account = 2").Where("1 = 1").Exec(ctx)
	})

	setMode := func(mode string, workspaceCap int) {
		GinkgoHelper()
		_, err := access.UpdateSettings(context.Background(), signupaccess.UpdateSettingsInput{Mode: mode, MaxOwnedWorkspacesPerAccount: workspaceCap, UpdatedBy: "ops"})
		Expect(err).NotTo(HaveOccurred())
	}

	invite := func(email string) string {
		GinkgoHelper()
		_, _, err := access.CreateInvite(context.Background(), signupaccess.CreateInviteInput{Email: email, TierID: models.DefaultTierID, CreatedBy: "ops"})
		Expect(err).NotTo(HaveOccurred())
		return links.lastToken()
	}

	signupReq := func(email, token string) *http.Response {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{
			"tenant":       fiber.Map{"name": "Acme"},
			"user":         fiber.Map{"name": "Jane", "email": email, "password": "correct horse"},
			"invite_token": token,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/tenants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	decode := func(resp *http.Response) map[string]any {
		GinkgoHelper()
		var out map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out
	}

	It("reports the signup mode publicly", func() {
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/signup-config", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(resp.Header.Get("Cache-Control")).To(Equal("public, max-age=30"))
		Expect(decode(resp)).To(HaveKeyWithValue("mode", "open"))
	})

	It("refuses self-registration while invite-only, with a code", func() {
		setMode(models.SignupModeInviteOnly, 2)
		resp := signupReq("stranger@x.co", "")
		Expect(resp.StatusCode).To(Equal(fiber.StatusForbidden))
		Expect(decode(resp)).To(HaveKeyWithValue("code", "invite_required"))
	})

	It("previews a usable invite and answers 410 for anything else", func() {
		token := invite("jane@acme.com")
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/tenant-invites/"+token, nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		body := decode(resp)
		Expect(body).To(HaveKeyWithValue("email", "jane@acme.com"))
		Expect(body).To(HaveKey("expires_at"))
		Expect(body).NotTo(HaveKey("tier_id"))

		resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/api/tenant-invites/nope", nil))
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusGone))
	})

	It("redeems an invite into a complimentary workspace while invite-only", func() {
		setMode(models.SignupModeInviteOnly, 2)
		token := invite("jane@acme.com")

		resp := signupReq("someone@else.io", token)
		Expect(resp.StatusCode).To(Equal(fiber.StatusForbidden))
		Expect(decode(resp)).To(HaveKeyWithValue("code", "invite_email_mismatch"))

		resp = signupReq("jane@acme.com", token)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		body := decode(resp)
		tenantID := body["tenant"].(map[string]any)["id"].(string)
		tenant, err := repository.NewTenantRepository(db).GetByID(context.Background(), tenantID)
		Expect(err).NotTo(HaveOccurred())
		Expect(tenant.Complimentary).To(BeTrue())
		Expect(tenant.BillingRequired).To(BeFalse())

		// The invite is spent.
		resp = signupReq("jane2@acme.com", token)
		Expect(resp.StatusCode).To(Equal(fiber.StatusGone))
	})

	It("caps owned workspaces while invite-only", func() {
		setMode(models.SignupModeInviteOnly, 1)
		resp := signupReq("cap@acme.com", invite("cap@acme.com"))
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var cookie *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				cookie = ck
			}
		}
		Expect(cookie).NotTo(BeNil())

		body, _ := json.Marshal(fiber.Map{"name": "Second"})
		req := httptest.NewRequest(http.MethodPost, "/api/workspaces", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusForbidden))
		Expect(decode(resp)).To(HaveKeyWithValue("code", "workspace_limit"))

		// Open mode lifts the cap.
		setMode(models.SignupModeOpen, 1)
		req = httptest.NewRequest(http.MethodPost, "/api/workspaces", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err = app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
	})
})
