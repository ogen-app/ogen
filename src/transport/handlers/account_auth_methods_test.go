package handlers_test

import (
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
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin/oauthtest"
)

var _ = Describe("Account sign-in methods", Ordered, func() {
	var (
		db   *bun.DB
		app  *fiber.App
		flow googleFlow
		jane = oauthtest.Identity{Subject: "google-sub-jane", Email: "jane.personal@gmail.com", EmailVerified: true, Name: "Jane"}
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		sessions := repository.NewSessionRepository(db)
		users := repository.NewUserRepository(db)
		accounts := repository.NewAccountRepository(db)
		identities := repository.NewAccountIdentityRepository(db)
		issuer := oauthtest.New(GinkgoT())
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		auth := handlers.RequireAuth(sessions, users, testCookieName)
		handlers.NewGoogleAuthHandler(newGoogleLoginService(db, issuer), sessions, testCookieName, false, nil).Register(app)
		handlers.NewAccountAuthMethodsHandler(oauthlogin.NewMethods(db, identities, sessions), accounts, auth, nil).Register(app)
		handlers.NewPasswordResetHandler(db, users, accounts, "https://app.example.com", nil, nil).Register(app)
		handlers.NewSessionsHandler(users, accounts, sessions, testCookieName, false, nil).Register(app)
		flow = googleFlow{app: app, issuer: issuer}
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, table := range []string{"account_identities", "oauth_states", "password_reset_tokens", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(table).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
	})

	// signedIn seeds an account and a live session for it.
	signedIn := func(password string) (*models.User, *http.Cookie) {
		GinkgoHelper()
		u := seedTenantUser(db, "Jane", "jane@acme.com", "password-jane")
		if password == "" {
			Expect(repository.NewAccountRepository(db).ClearPasswordTx(context.Background(), nil, u.AccountID)).To(Succeed())
		}
		token, err := models.NewSessionToken()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Session{ID: token, AccountID: u.AccountID, UserID: u.ID, TenantID: u.TenantID, ExpiresAt: time.Now().UTC().Add(time.Hour)}).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		return u, &http.Cookie{Name: testCookieName, Value: token}
	}

	linkGoogle := func(u *models.User) {
		GinkgoHelper()
		_, err := db.NewInsert().Model(&models.AccountIdentity{ID: "ai-jane", AccountID: u.AccountID, Provider: models.IdentityProviderGoogle, Subject: jane.Subject, Email: jane.Email}).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	}

	do := func(method, path string, cookie *http.Cookie) *http.Response {
		GinkgoHelper()
		req := httptest.NewRequest(method, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req, -1)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	type methodsBody struct {
		HasPassword bool `json:"has_password"`
		Identities  []struct {
			Provider string `json:"provider"`
			Email    string `json:"email"`
		} `json:"identities"`
	}
	readMethods := func(cookie *http.Cookie) methodsBody {
		GinkgoHelper()
		resp := do("GET", "/api/account/auth-methods", cookie)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var b methodsBody
		Expect(json.NewDecoder(resp.Body).Decode(&b)).To(Succeed())
		return b
	}

	It("requires a session", func() {
		Expect(do("GET", "/api/account/auth-methods", nil).StatusCode).To(Equal(fiber.StatusUnauthorized))
	})

	It("connects Google from Settings, keeping the password, and lists it", func() {
		_, cookie := signedIn("password-jane")
		start := httptest.NewRequest("GET", "/api/auth/google/start?intent=link&return_to=%2Fsettings%2Faccount", nil)
		start.AddCookie(cookie)
		resp, err := app.Test(start, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusFound))
		state := cookieNamed(resp, "ogen_oauth_state")
		code, st := flow.issuer.Authorize(GinkgoT(), resp.Header.Get("Location"), jane)

		cb := httptest.NewRequest("GET", "/api/auth/google/callback?code="+code+"&state="+st, nil)
		cb.AddCookie(cookie)
		cb.AddCookie(&http.Cookie{Name: "ogen_oauth_state", Value: state.Value})
		done, err := app.Test(cb, -1)
		Expect(err).NotTo(HaveOccurred())
		Expect(done.Header.Get("Location")).To(Equal("/settings/account?linked=google"))

		b := readMethods(cookie)
		Expect(b.HasPassword).To(BeTrue())
		Expect(b.Identities).To(HaveLen(1))
		Expect(b.Identities[0].Email).To(Equal(jane.Email))
	})

	It("refuses to start connecting without a session", func() {
		Expect(do("GET", "/api/auth/google/start?intent=link", nil).StatusCode).To(Equal(fiber.StatusUnauthorized))
	})

	It("disconnects Google when a password remains", func() {
		u, cookie := signedIn("password-jane")
		linkGoogle(u)
		Expect(do("DELETE", "/api/account/identities/google", cookie).StatusCode).To(Equal(fiber.StatusNoContent))
		Expect(readMethods(cookie).Identities).To(BeEmpty())
		Expect(do("DELETE", "/api/account/identities/google", cookie).StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("refuses to disconnect the only way to sign in", func() {
		u, cookie := signedIn("")
		linkGoogle(u)
		Expect(readMethods(cookie).HasPassword).To(BeFalse())
		resp := do("DELETE", "/api/account/identities/google", cookie)
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		var b struct {
			Code string `json:"code"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&b)).To(Succeed())
		Expect(b.Code).To(Equal("last_sign_in_method"))
	})

	It("lets a Google-only account set its first password through the reset flow", func() {
		u, _ := signedIn("")
		linkGoogle(u)
		token, hash, err := models.NewResetToken()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.PasswordResetToken{ID: "rt", UserID: u.ID, TenantID: u.TenantID, TokenHash: hash, ExpiresAt: time.Now().UTC().Add(time.Hour)}).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())

		confirm := postJSON(app, "/api/password-reset/confirm", fiber.Map{"token": token, "password": "brand-new-password"})
		Expect(confirm.StatusCode).To(Equal(fiber.StatusNoContent))

		login := postJSON(app, "/api/sessions", fiber.Map{"email": "jane@acme.com", "password": "brand-new-password"})
		Expect(login.StatusCode).To(Equal(fiber.StatusCreated))
	})

	It("never lets a passwordless account sign in with an empty password", func() {
		signedIn("")
		login := postJSON(app, "/api/sessions", fiber.Map{"email": "jane@acme.com", "password": " "})
		Expect(login.StatusCode).To(Equal(fiber.StatusUnauthorized))
	})
})
