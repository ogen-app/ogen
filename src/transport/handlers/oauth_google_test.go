package handlers_test

import (
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
	return oauthlogin.New(oauthlogin.Deps{
		DB:         db,
		Provider:   issuer.Provider(GinkgoT()),
		OAuth:      repository.NewOAuthRepository(db),
		Identities: repository.NewAccountIdentityRepository(db),
		Accounts:   repository.NewAccountRepository(db),
		Users:      repository.NewUserRepository(db),
		Sessions:   repository.NewSessionRepository(db),
	})
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
		handlers.NewGoogleAuthHandler(newGoogleLoginService(db, issuer), testCookieName, false, nil).Register(app)
		flow = googleFlow{app: app, issuer: issuer}
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, table := range []string{"account_identities", "oauth_states", "oauth_pending_signups", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(table).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
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
			handlers.NewGoogleAuthHandler(nil, testCookieName, false, nil).Register(app)
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

		It("reports an unlinked Google account", func() {
			resp := flow.signIn("", jane)
			Expect(resp.Header.Get("Location")).To(Equal("/auth/login?error=google_not_linked"))
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
