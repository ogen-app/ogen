package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/loginsecurity"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

const testDeviceCookie = "test_device"

// recordedAlerts captures new-device emails instead of queueing them.
type recordedAlerts struct {
	mu   sync.Mutex
	urls []string
}

func (r *recordedAlerts) EnqueueNewDeviceLoginEmailTx(_ context.Context, _ *sql.Tx, _, _, _ string, v queues.NewDeviceLoginVars) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, v.SecureURL)
	return nil
}

func (r *recordedAlerts) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.urls)
}

func (r *recordedAlerts) lastToken() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	Expect(r.urls).NotTo(BeEmpty())
	u, err := url.Parse(r.urls[len(r.urls)-1])
	Expect(err).NotTo(HaveOccurred())
	return u.Query().Get("token")
}

func newLoginSecurity(db *bun.DB, emails loginsecurity.AlertEnqueuer) *loginsecurity.Service {
	return loginsecurity.New(loginsecurity.Deps{
		DB:         db,
		Devices:    repository.NewKnownDeviceRepository(db),
		Alerts:     repository.NewLoginAlertTokenRepository(db),
		Sessions:   repository.NewSessionRepository(db),
		Users:      repository.NewUserRepository(db),
		Accounts:   repository.NewAccountRepository(db),
		Emails:     emails,
		AppBaseURL: "https://app.example",
	})
}

func cookieNamed(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var _ = Describe("Login security", Ordered, func() {
	var (
		app    *fiber.App
		db     *bun.DB
		alerts *recordedAlerts
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	buildApp := func(sec *loginsecurity.Service) *fiber.App {
		a := fiber.New(fiber.Config{
			ErrorHandler: func(c *fiber.Ctx, err error) error {
				code := fiber.StatusInternalServerError
				if e, ok := err.(*fiber.Error); ok {
					code = e.Code
				}
				return c.Status(code).JSON(fiber.Map{"error": err.Error()})
			},
		})
		userRepo := repository.NewUserRepository(db)
		accountRepo := repository.NewAccountRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)

		handlers.NewUsersHandler(db, userRepo, accountRepo, repository.NewSettingRepository(db), auth, nil, nil).Register(a)
		sessions := handlers.NewSessionsHandler(userRepo, accountRepo, sessionRepo, testCookieName, false, nil)
		sessions.SetLoginSecurity(sec, testDeviceCookie)
		sessions.Register(a)
		tenants := handlers.NewTenantsHandler(signup.New(db, accountRepo, tenantRepo, nil), tenantRepo, testCookieName, false, auth, nil)
		tenants.SetLoginSecurity(sec, testDeviceCookie)
		tenants.Register(a)
		handlers.NewInvitationsHandler(db, userRepo, accountRepo, tenantRepo, repository.NewInvitationRepository(db), sessionRepo, "https://app.example", testCookieName, false, auth,
			handlers.InvitationsOptions{LoginSecurity: sec, DeviceCookieName: testDeviceCookie}).Register(a)
		handlers.NewLoginAlertsHandler(sec).Register(a)
		return a
	}

	BeforeEach(func() {
		alerts = &recordedAlerts{}
		app = buildApp(newLoginSecurity(db, alerts))
	})

	AfterEach(func() {
		for _, table := range []string{"login_alert_tokens", "account_known_devices", "password_reset_tokens", "users_invitations", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(table).Where("1 = 1").Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := db.NewDelete().TableExpr("tenants").Where("id != ?", models.DefaultTenantID).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	do := func(method, path string, body any, cookies ...*http.Cookie) *http.Response {
		var buf bytes.Buffer
		if body != nil {
			Expect(json.NewEncoder(&buf).Encode(body)).To(Succeed())
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:131.0) Gecko/20100101 Firefox/131.0")
		for _, c := range cookies {
			if c != nil {
				req.AddCookie(c)
			}
		}
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	login := func(device *http.Cookie) *http.Response {
		resp := do("POST", "/api/sessions", fiber.Map{"email": "jane@acme.com", "password": "password123"}, device)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		return resp
	}

	decode := func(resp *http.Response, v any) {
		Expect(json.NewDecoder(resp.Body).Decode(v)).To(Succeed())
	}

	It("sets a long-lived device cookie and alerts only for unknown devices", func() {
		seedTenantUser(db, "Jane", "jane@acme.com", "password123")

		first := login(nil)
		device := cookieNamed(first, testDeviceCookie)
		Expect(device).NotTo(BeNil())
		Expect(device.HttpOnly).To(BeTrue())
		Expect(device.SameSite).To(Equal(http.SameSiteLaxMode))
		Expect(device.MaxAge).To(Equal(400 * 24 * 60 * 60))
		Expect(alerts.count()).To(Equal(0), "first device is enrolled silently")

		again := login(device)
		Expect(cookieNamed(again, testDeviceCookie).Value).To(Equal(device.Value), "a known device keeps its cookie")
		Expect(alerts.count()).To(Equal(0))

		login(nil)
		Expect(alerts.count()).To(Equal(1), "an unknown device is reported")
	})

	It("secures the account from the alert link", func() {
		seedTenantUser(db, "Jane", "jane@acme.com", "password123")
		login(nil)
		intruder := login(nil)
		intruderSession := cookieNamed(intruder, testCookieName)
		Expect(do("GET", "/api/current_user", nil, intruderSession).StatusCode).To(Equal(fiber.StatusOK))
		token := alerts.lastToken()

		resp := do("GET", "/api/security/login-alerts/"+token, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var preview loginsecurity.Preview
		decode(resp, &preview)
		Expect(preview.Status).To(Equal(models.LoginAlertPending))
		Expect(preview.Device).To(Equal("Firefox on Windows"))
		Expect(preview.Email).To(Equal("j***@acme.com"))

		resp = do("POST", "/api/security/login-alerts/"+token+"/secure", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var secured loginsecurity.Secured
		decode(resp, &secured)
		Expect(secured.SessionsRevoked).To(Equal(2))
		Expect(secured.ResetURL).To(HavePrefix("https://app.example/auth/reset?token="))

		Expect(do("GET", "/api/current_user", nil, intruderSession).StatusCode).To(Equal(fiber.StatusUnauthorized))

		resp = do("POST", "/api/security/login-alerts/"+token+"/secure", nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusGone))
		var body map[string]string
		decode(resp, &body)
		Expect(body["error"]).To(Equal("token_used"))

		// The intruder's copy of the password no longer works.
		resp = do("POST", "/api/sessions", fiber.Map{"email": "jane@acme.com", "password": "password123"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))

		// The reset link it returned completes a normal password reset.
		resetToken := strings.TrimPrefix(secured.ResetURL, "https://app.example/auth/reset?token=")
		pr := handlers.NewPasswordResetHandler(db, repository.NewUserRepository(db), repository.NewAccountRepository(db), "https://app.example", nil, nil)
		pr.Register(app)
		resp = do("POST", "/api/password-reset/confirm", fiber.Map{"token": resetToken, "password": "new-password-1"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusNoContent))
	})

	It("answers 404 for unknown and malformed tokens", func() {
		for _, tok := range []string{"nope", strings.Repeat("A", 43)} {
			Expect(do("GET", "/api/security/login-alerts/"+tok, nil).StatusCode).To(Equal(fiber.StatusNotFound))
			Expect(do("POST", "/api/security/login-alerts/"+tok+"/secure", nil).StatusCode).To(Equal(fiber.StatusNotFound))
		}
	})

	It("enrols the signup browser so the owner's first login is silent", func() {
		resp := do("POST", "/api/tenants", fiber.Map{
			"tenant": fiber.Map{"name": "Acme"},
			"user":   fiber.Map{"name": "Jane", "email": "jane@acme.com", "password": "password123"},
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		device := cookieNamed(resp, testDeviceCookie)
		Expect(device).NotTo(BeNil())

		login(device)
		Expect(alerts.count()).To(Equal(0))
		login(nil)
		Expect(alerts.count()).To(Equal(1))
	})

	It("enrols the browser that accepts an invite as a new account", func() {
		owner := seedTenantUserWithRole(db, "Owner", "owner@acme.com", "password123", models.RoleOwner)
		token, hash, err := models.NewInvitationToken()
		Expect(err).NotTo(HaveOccurred())
		inv := &models.Invitation{
			ID: "inv-1", TenantID: models.DefaultTenantID, Email: "jane@acme.com", Role: models.RoleMember,
			TokenHash: hash, InvitedBy: owner.ID, Status: models.InvitationPending,
			ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now().UTC(),
		}
		_, err = db.NewInsert().Model(inv).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())

		resp := do("POST", "/api/invitations/accept/"+token, fiber.Map{"name": "Jane", "password": "password123"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		device := cookieNamed(resp, testDeviceCookie)
		Expect(device).NotTo(BeNil())

		login(device)
		Expect(alerts.count()).To(Equal(0))
		login(nil)
		Expect(alerts.count()).To(Equal(1))
	})

	It("never fails a login when device tracking breaks", func() {
		seedTenantUser(db, "Jane", "jane@acme.com", "password123")
		broken := pgtest.MustDB()
		Expect(broken.Close()).To(Succeed())
		app = buildApp(newLoginSecurity(broken, alerts))

		resp := login(nil)
		Expect(cookieNamed(resp, testCookieName)).NotTo(BeNil())
		Expect(alerts.count()).To(Equal(0))
	})
})
