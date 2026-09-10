package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// fakeProfileEnqueuer records the tenant ids signup asks to provision a Zernio
// profile for (CON-102 FR2). Returning nil lets the signup transaction commit.
type fakeProfileEnqueuer struct {
	mu        sync.Mutex
	tenantIDs []string
}

func (f *fakeProfileEnqueuer) EnqueueBootstrapProfileTx(_ context.Context, _ *sql.Tx, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenantIDs = append(f.tenantIDs, tenantID)
	return nil
}

func (f *fakeProfileEnqueuer) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tenantIDs...)
}

var _ = Describe("TenantsHandler", Ordered, func() {
	var (
		app *fiber.App
		db  *bun.DB
		enq *fakeProfileEnqueuer
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
		tenantRepo := repository.NewTenantRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		enq = &fakeProfileEnqueuer{}
		signupSvc := signup.New(db, repository.NewAccountRepository(db), tenantRepo, enq)
		handlers.NewTenantsHandler(signupSvc, tenantRepo, testCookieName, false, auth).Register(app)
	})

	AfterEach(func() {
		ctx := context.Background()
		_, _ = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		// Credentials live on accounts since CON-147 (signup creates one); clear
		// them so a reused email doesn't collide across specs.
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
		// Leave the default tenant (seeded by the migration); drop signup tenants.
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).
			Where("id <> ?", models.DefaultTenantID).Exec(ctx)
	})

	// signup performs a successful signup and returns the auth cookie + decoded body.
	signup := func(tenantName, name, email, password string) (*http.Cookie, map[string]any) {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{
			"tenant": fiber.Map{"name": tenantName},
			"user":   fiber.Map{"name": name, "email": email, "password": password},
		})
		req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out map[string]any
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		var cookie *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				cookie = ck
			}
		}
		Expect(cookie).NotTo(BeNil(), "signup must set the session cookie")
		return cookie, out
	}

	tenantID := func(body map[string]any) string {
		t, ok := body["tenant"].(map[string]any)
		Expect(ok).To(BeTrue())
		return t["id"].(string)
	}

	Describe("POST /api/tenants (signup)", func() {
		It("creates a tenant + first user + session and sets a cookie", func() {
			cookie, out := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			Expect(cookie.Value).NotTo(BeEmpty())

			tenant := out["tenant"].(map[string]any)
			Expect(tenant["id"]).NotTo(BeEmpty())
			Expect(tenant["name"]).To(Equal("Acme Inc"))
			Expect(tenant["slug"]).To(Equal("acme-inc"))

			user := out["user"].(map[string]any)
			Expect(user["email"]).To(Equal("alice@acme.test"))
			Expect(user).NotTo(HaveKey("password_hash"))
		})

		It("enqueues an eager Zernio profile-bootstrap for the new tenant (CON-102)", func() {
			_, out := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			Expect(enq.ids()).To(ConsistOf(tenantID(out)),
				"signup must enqueue exactly one bootstrap job for the created tenant")
		})

		It("rejects a duplicate email with 409", func() {
			signup("Acme Inc", "Alice", "dupe@acme.test", "password-alice")

			body, _ := json.Marshal(fiber.Map{
				"tenant": fiber.Map{"name": "Other Co"},
				"user":   fiber.Map{"name": "Bob", "email": "dupe@acme.test", "password": "password-bob"},
			})
			req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		})

		It("returns 409 (never a raw 500) when concurrent signups race on the same email", func() {
			const n = 5 // matches the test pool's max connections, so they truly race
			start := make(chan struct{})
			codes := make([]int, n)
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Add(1)
				// No Ginkgo assertions inside the goroutine — record results and
				// assert on the spec goroutine after Wait().
				go func(idx int) {
					defer wg.Done()
					body, _ := json.Marshal(fiber.Map{
						"tenant": fiber.Map{"name": fmt.Sprintf("Org %d", idx)},
						"user":   fiber.Map{"name": "Racer", "email": "race@example.com", "password": "password-race"},
					})
					req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					<-start // release all goroutines together to force the TOCTOU window
					resp, err := app.Test(req, 5000)
					if err != nil {
						errs[idx] = err
						return
					}
					codes[idx] = resp.StatusCode
				}(i)
			}
			close(start)
			wg.Wait()

			created, conflict := 0, 0
			for i, code := range codes {
				Expect(errs[i]).NotTo(HaveOccurred())
				switch code {
				case fiber.StatusCreated:
					created++
				case fiber.StatusConflict:
					conflict++
				default:
					Fail(fmt.Sprintf("unexpected status %d — a raw DB error leaked instead of 409", code))
				}
			}
			Expect(created).To(Equal(1), "exactly one signup wins")
			Expect(conflict).To(Equal(n-1), "the rest get 409, not 500")
		})

		It("derives a distinct slug when tenant names collide", func() {
			_, a := signup("Acme Inc", "Alice", "a@acme.test", "password-alice")
			_, b := signup("Acme Inc", "Bob", "b@acme.test", "password-bob")
			Expect(a["tenant"].(map[string]any)["slug"]).To(Equal("acme-inc"))
			Expect(b["tenant"].(map[string]any)["slug"]).To(Equal("acme-inc-2"))
		})

		DescribeTable("returns 400 for invalid payloads",
			func(payload fiber.Map) {
				body, _ := json.Marshal(payload)
				req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			},
			Entry("missing tenant name", fiber.Map{"user": fiber.Map{"name": "A", "email": "a@x.test", "password": "password-x"}}),
			Entry("missing user email", fiber.Map{"tenant": fiber.Map{"name": "T"}, "user": fiber.Map{"name": "A", "password": "password-x"}}),
			Entry("bad email", fiber.Map{"tenant": fiber.Map{"name": "T"}, "user": fiber.Map{"name": "A", "email": "nope", "password": "password-x"}}),
			Entry("short password", fiber.Map{"tenant": fiber.Map{"name": "T"}, "user": fiber.Map{"name": "A", "email": "a@x.test", "password": "short"}}),
		)

		It("throttles signup per IP with 429 + Retry-After (CON-162)", func() {
			// signupPerIPBurst is 10 and every well-formed attempt is charged. Reuse
			// one email: the first attempt is 201, the rest 409 (duplicate), but each
			// still spends a token — the eleventh trips the per-IP budget.
			post := func() *http.Response {
				body, _ := json.Marshal(fiber.Map{
					"tenant": fiber.Map{"name": "Repeat Co"},
					"user":   fiber.Map{"name": "Dup", "email": "dup@throttle.test", "password": "password-dup"},
				})
				req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				return resp
			}
			for i := range 10 {
				Expect(post().StatusCode).To(BeElementOf(fiber.StatusCreated, fiber.StatusConflict),
					"attempt %d should reach the handler, not be throttled", i+1)
			}
			resp := post()
			Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
			Expect(resp.Header.Get("Retry-After")).NotTo(BeEmpty())
		})
	})

	Describe("GET /api/tenants/current", func() {
		It("returns 401 without a session", func() {
			req := httptest.NewRequest("GET", "/api/tenants/current", nil)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(401))
		})

		It("returns the caller's tenant", func() {
			cookie, out := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			req := httptest.NewRequest("GET", "/api/tenants/current", nil)
			req.AddCookie(cookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var got map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
			Expect(got["id"]).To(Equal(tenantID(out)))
		})
	})

	Describe("GET /api/tenants/:id (isolation)", func() {
		It("returns the caller's own tenant", func() {
			cookie, out := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			id := tenantID(out)
			req := httptest.NewRequest("GET", "/api/tenants/"+id, nil)
			req.AddCookie(cookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))
		})

		It("returns 404 for another tenant's id (no existence leak)", func() {
			cookieA, _ := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			_, outB := signup("Beta Co", "Bob", "bob@beta.test", "password-bob")
			idB := tenantID(outB)

			req := httptest.NewRequest("GET", "/api/tenants/"+idB, nil)
			req.AddCookie(cookieA)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(404))
		})
	})

	Describe("PUT /api/tenants/:id", func() {
		It("updates the caller's own tenant name", func() {
			cookie, out := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			id := tenantID(out)

			body, _ := json.Marshal(fiber.Map{"name": "Acme Renamed"})
			req := httptest.NewRequest("PUT", "/api/tenants/"+id, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(cookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var got map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
			Expect(got["name"]).To(Equal("Acme Renamed"))
			// Slug is stable across renames.
			Expect(got["slug"]).To(Equal("acme-inc"))
		})

		It("returns 404 when updating another tenant", func() {
			cookieA, _ := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			_, outB := signup("Beta Co", "Bob", "bob@beta.test", "password-bob")
			idB := tenantID(outB)

			body, _ := json.Marshal(fiber.Map{"name": "Hacked"})
			req := httptest.NewRequest("PUT", "/api/tenants/"+idB, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(cookieA)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(404))
		})

		It("returns 400 for a blank name", func() {
			cookie, out := signup("Acme Inc", "Alice", "alice@acme.test", "password-alice")
			id := tenantID(out)

			body, _ := json.Marshal(fiber.Map{"name": ""})
			req := httptest.NewRequest("PUT", "/api/tenants/"+id, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(cookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(400))
		})
	})
})
