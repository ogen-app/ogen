package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin/oauthtest"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// googleFlow drives the browser's side of a Google sign-in against a fiber app
// and a local issuer.
type googleFlow struct {
	app    *fiber.App
	issuer *oauthtest.Issuer
}

// start opens /start and returns the provider URL and the state cookie.
func (g googleFlow) start(query string) (authURL, stateCookie string) {
	GinkgoHelper()
	resp, err := g.app.Test(httptest.NewRequest("GET", "/api/auth/google/start"+query, nil), -1)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(fiber.StatusFound))
	c := cookieNamed(resp, "ogen_oauth_state")
	Expect(c).NotTo(BeNil())
	Expect(c.HttpOnly).To(BeTrue())
	Expect(c.Path).To(Equal("/api/auth/google"))
	return resp.Header.Get("Location"), c.Value
}

// callback hits the callback with the given state cookie ("" sends none).
func (g googleFlow) callback(code, state, cookie string) *http.Response {
	GinkgoHelper()
	q := url.Values{"code": {code}, "state": {state}}
	req := httptest.NewRequest("GET", "/api/auth/google/callback?"+q.Encode(), nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "ogen_oauth_state", Value: cookie})
	}
	resp, err := g.app.Test(req, -1)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(fiber.StatusFound))
	return resp
}

// signIn runs start → provider → callback as who.
func (g googleFlow) signIn(query string, who oauthtest.Identity) *http.Response {
	GinkgoHelper()
	authURL, cookie := g.start(query)
	code, state := g.issuer.Authorize(GinkgoT(), authURL, who)
	return g.callback(code, state, cookie)
}

func newGoogleLoginService(db *bun.DB, issuer *oauthtest.Issuer) *oauthlogin.Service {
	accounts := repository.NewAccountRepository(db)
	return oauthlogin.New(oauthlogin.Deps{
		DB:         db,
		Provider:   issuer.Provider(GinkgoT()),
		OAuth:      repository.NewOAuthRepository(db),
		Identities: repository.NewAccountIdentityRepository(db),
		Accounts:   accounts,
		Users:      repository.NewUserRepository(db),
		Sessions:   repository.NewSessionRepository(db),
		Signup:     signup.New(db, accounts, repository.NewTenantRepository(db), nil),
	})
}

// postJSON sends a JSON body and returns the response.
func postJSON(app *fiber.App, path string, body any) *http.Response {
	GinkgoHelper()
	b, err := json.Marshal(body)
	Expect(err).NotTo(HaveOccurred())
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	Expect(err).NotTo(HaveOccurred())
	return resp
}

var _ = Describe("GoogleAuthHandler", Ordered, func() {
	var (
		db   *bun.DB
		flow googleFlow
		jane = oauthtest.Identity{Subject: "google-sub-jane", Email: "jane@gmail.com", EmailVerified: true, Name: "Jane"}
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		issuer := oauthtest.New(GinkgoT())
		app := fiber.New()
		handlers.NewGoogleAuthHandler(newGoogleLoginService(db, issuer), repository.NewSessionRepository(db), testCookieName, false, nil).Register(app)
		flow = googleFlow{app: app, issuer: issuer}
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, table := range []string{"account_identities", "oauth_states", "oauth_pending_signups", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(table).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := db.NewDelete().TableExpr("tenants").Where("id != ?", models.DefaultTenantID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	linkJane := func() *models.User {
		u := seedTenantUser(db, "Jane", "jane@acme.com", "password-jane")
		_, err := db.NewInsert().Model(&models.AccountIdentity{ID: "ai-jane", AccountID: u.AccountID, Provider: models.IdentityProviderGoogle, Subject: jane.Subject, Email: jane.Email}).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		return u
	}

	Describe("GET /api/auth/providers", func() {
		probe := func(app *fiber.App) bool {
			resp, err := app.Test(httptest.NewRequest("GET", "/api/auth/providers", nil))
			Expect(err).NotTo(HaveOccurred())
			var body struct {
				Google struct {
					Enabled bool `json:"enabled"`
				} `json:"google"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			return body.Google.Enabled
		}

		It("reports Google enabled when configured", func() {
			Expect(probe(flow.app)).To(BeTrue())
		})

		It("reports Google disabled and serves no Google routes without a client", func() {
			app := fiber.New()
			handlers.NewGoogleAuthHandler(nil, nil, testCookieName, false, nil).Register(app)
			Expect(probe(app)).To(BeFalse())
			resp, err := app.Test(httptest.NewRequest("GET", "/api/auth/google/start", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
		})
	})

	Describe("GET /api/auth/google/start", func() {
		It("rejects an unknown intent", func() {
			resp, err := flow.app.Test(httptest.NewRequest("GET", "/api/auth/google/start?intent=bogus", nil))
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		})
	})

	Describe("GET /api/auth/google/callback", func() {
		It("signs a linked identity in and lands on the return path", func() {
			u := linkJane()
			resp := flow.signIn("?return_to=%2Fcampaigns", jane)
			Expect(resp.Header.Get("Location")).To(Equal("/campaigns"))

			session := cookieNamed(resp, testCookieName)
			Expect(session).NotTo(BeNil())
			Expect(session.HttpOnly).To(BeTrue())
			s, err := repository.NewSessionRepository(db).GetByID(context.Background(), session.Value)
			Expect(err).NotTo(HaveOccurred())
			Expect(s.AccountID).To(Equal(u.AccountID))

			state := cookieNamed(resp, "ogen_oauth_state")
			Expect(state).NotTo(BeNil())
			Expect(state.Value).To(BeEmpty(), "the state cookie is cleared")
		})

		It("refuses to finish a sign-in started in another browser", func() {
			linkJane()
			authURL, _ := flow.start("")
			code, state := flow.issuer.Authorize(GinkgoT(), authURL, jane)
			resp := flow.callback(code, state, "")
			Expect(resp.Header.Get("Location")).To(Equal("/auth/login?error=google_state_invalid"))
			Expect(cookieNamed(resp, testCookieName)).To(BeNil())
		})

		It("links a password account with the same address and removes its password", func() {
			u := seedTenantUser(db, "Jane", "JANE@gmail.com", "password-jane")
			resp := flow.signIn("", jane)
			Expect(resp.Header.Get("Location")).To(Equal("/"))
			Expect(cookieNamed(resp, testCookieName)).NotTo(BeNil())

			account, err := repository.NewAccountRepository(db).GetByID(context.Background(), u.AccountID)
			Expect(err).NotTo(HaveOccurred())
			Expect(account.HasPassword()).To(BeFalse())
		})

		It("hands a newcomer to the workspace step, then signs them up", func() {
			resp := flow.signIn("?return_to=%2Fcampaigns", jane)
			loc, err := url.Parse(resp.Header.Get("Location"))
			Expect(err).NotTo(HaveOccurred())
			Expect(loc.Path).To(Equal("/auth/register/google"))
			token := loc.Query().Get("token")
			Expect(token).NotTo(BeEmpty())
			Expect(cookieNamed(resp, testCookieName)).To(BeNil(), "no session before the workspace exists")

			preview, err := flow.app.Test(httptest.NewRequest("GET", "/api/auth/google/pending/"+token, nil), -1)
			Expect(err).NotTo(HaveOccurred())
			Expect(preview.StatusCode).To(Equal(fiber.StatusOK))
			var p struct {
				Email string `json:"email"`
			}
			Expect(json.NewDecoder(preview.Body).Decode(&p)).To(Succeed())
			Expect(p.Email).To(Equal(jane.Email))

			done := postJSON(flow.app, "/api/auth/google/complete", fiber.Map{"token": token, "tenant_name": "Acme"})
			Expect(done.StatusCode).To(Equal(fiber.StatusCreated))
			var body struct {
				Tenant   models.Tenant `json:"tenant"`
				ReturnTo string        `json:"return_to"`
			}
			Expect(json.NewDecoder(done.Body).Decode(&body)).To(Succeed())
			Expect(body.Tenant.Name).To(Equal("Acme"))
			Expect(body.ReturnTo).To(Equal("/campaigns"))
			Expect(cookieNamed(done, testCookieName)).NotTo(BeNil())

			again := postJSON(flow.app, "/api/auth/google/complete", fiber.Map{"token": token, "tenant_name": "Acme"})
			Expect(again.StatusCode).To(Equal(fiber.StatusNotFound))
		})

		It("answers 409 email_in_use when the address was registered meanwhile", func() {
			resp := flow.signIn("", jane)
			loc, err := url.Parse(resp.Header.Get("Location"))
			Expect(err).NotTo(HaveOccurred())
			seedTenantUser(db, "Jane", jane.Email, "password-jane")

			done := postJSON(flow.app, "/api/auth/google/complete", fiber.Map{"token": loc.Query().Get("token"), "tenant_name": "Acme"})
			Expect(done.StatusCode).To(Equal(fiber.StatusConflict))
			var body struct {
				Code string `json:"code"`
			}
			Expect(json.NewDecoder(done.Body).Decode(&body)).To(Succeed())
			Expect(body.Code).To(Equal("email_in_use"))
		})

		It("rejects an incomplete sign-up form", func() {
			resp := postJSON(flow.app, "/api/auth/google/complete", fiber.Map{"token": "x"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		})

		It("throttles a client that keeps failing", func() {
			for range 30 {
				flow.callback("c", "s", "")
			}
			linkJane()
			resp := flow.signIn("", jane)
			Expect(resp.Header.Get("Location")).To(Equal("/auth/login?error=throttled"))
		})
	})
})
