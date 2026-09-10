package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("SessionsHandler", Ordered, func() {
	var (
		app *fiber.App
		db  *bun.DB
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
		settingRepo := repository.NewSettingRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewUpdate().TableExpr("settings").Set("value = ?", "false").
			Where("key = ?", "setup_complete").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// ── helpers ──────────────────────────────────────────────────────────────

	seedUser := func(name, email, password string) {
		seedTenantUser(db, name, email, password)
	}

	doLogin := func(email, password string) (*http.Response, models.Session) {
		body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		var session models.Session
		if resp.StatusCode == fiber.StatusCreated {
			Expect(json.NewDecoder(resp.Body).Decode(&session)).To(Succeed())
		}
		return resp, session
	}

	// ── Create (login) ───────────────────────────────────────────────────────

	Describe("POST /api/sessions", func() {
		Context("with valid credentials", func() {
			It("returns 201 with session body and sets an HttpOnly cookie", func() {
				seedUser("Alice", "alice@example.com", "password-alice")

				resp, session := doLogin("alice@example.com", "password-alice")
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
				Expect(session.ID).NotTo(BeEmpty())
				Expect(session.UserID).NotTo(BeEmpty())
				Expect(session.ExpiresAt.IsZero()).To(BeFalse())

				cookies := resp.Cookies()
				Expect(cookies).To(HaveLen(1))
				Expect(cookies[0].Name).To(Equal(testCookieName))
				Expect(cookies[0].Value).To(Equal(session.ID))
				Expect(cookies[0].HttpOnly).To(BeTrue())
				// This suite wires the handler with secureCookie=false (dev mode),
				// so the Secure flag must NOT be set on the cookie.
				Expect(cookies[0].Secure).To(BeFalse())
			})

			It("sets the Secure flag when the handler is configured for production", func() {
				secureApp := fiber.New(fiber.Config{
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
				settingRepo := repository.NewSettingRepository(db)
				auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
				handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(secureApp)
				handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, true).Register(secureApp)

				// Seed a user directly in the default tenant; login below exercises the secure-cookie path.
				seedTenantUser(db, "Sec", "sec@example.com", "password-sec")

				loginBody, _ := json.Marshal(fiber.Map{"email": "sec@example.com", "password": "password-sec"})
				loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
				loginReq.Header.Set("Content-Type", "application/json")
				loginResp, err := secureApp.Test(loginReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))

				cookies := loginResp.Cookies()
				Expect(cookies).To(HaveLen(1))
				Expect(cookies[0].Secure).To(BeTrue())
				Expect(cookies[0].HttpOnly).To(BeTrue())
			})
		})

		Context("with wrong password", func() {
			It("returns 401", func() {
				seedUser("Bob", "bob@example.com", "correct-password")

				body, _ := json.Marshal(fiber.Map{"email": "bob@example.com", "password": "wrong-password"})
				req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
			})
		})

		Context("with unknown email", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"email": "nobody@example.com", "password": "whatever"})
				req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
			})
		})

		Context("with invalid payload", func() {
			DescribeTable("returns 400",
				func(payload fiber.Map) {
					body, _ := json.Marshal(payload)
					req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
				},
				Entry("missing email", fiber.Map{"password": "s3cur3P@ss"}),
				Entry("missing password", fiber.Map{"email": "x@example.com"}),
				Entry("invalid email format", fiber.Map{"email": "not-an-email", "password": "s3cur3P@ss"}),
				Entry("empty body", fiber.Map{}),
			)
		})

		// ── rate limiting (CON-162) ──────────────────────────────────────────
		Context("when login attempts are abused (CON-162)", func() {
			// attempt performs a raw login and returns the response (no assertions),
			// so specs can drive it to 401/429 deliberately.
			attempt := func(email, password string) *http.Response {
				body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
				req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				return resp
			}
			errorBody := func(resp *http.Response) string {
				var payload map[string]string
				Expect(json.NewDecoder(resp.Body).Decode(&payload)).To(Succeed())
				return payload["error"]
			}

			It("throttles repeated failed logins for one address with 429 + Retry-After", func() {
				// loginPerEmailBurst is 10: ten failures are answered 401, the
				// eleventh is throttled.
				for range 10 {
					Expect(attempt("victim@example.com", "wrong").StatusCode).To(Equal(fiber.StatusUnauthorized))
				}
				resp := attempt("victim@example.com", "wrong")
				Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
				Expect(resp.Header.Get("Retry-After")).NotTo(BeEmpty())
				Expect(errorBody(resp)).To(ContainSubstring("Too many login attempts"))
			})

			It("does not reveal whether the address exists — same 429 for a real and an unknown address", func() {
				seedUser("Real", "real@example.com", "correct-password")

				exhaust := func(email string) *http.Response {
					var last *http.Response
					for range 11 {
						last = attempt(email, "wrong")
					}
					return last
				}
				realResp := exhaust("real@example.com")
				unknownResp := exhaust("ghost@example.com")

				Expect(realResp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
				Expect(unknownResp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
				Expect(errorBody(realResp)).To(Equal(errorBody(unknownResp)))
			})

			It("resets the address counter after a successful login", func() {
				seedUser("Fumbles", "fumbles@example.com", "correct-password")

				// Five failures, then a success that refunds the address bucket.
				for range 5 {
					Expect(attempt("fumbles@example.com", "wrong").StatusCode).To(Equal(fiber.StatusUnauthorized))
				}
				Expect(attempt("fumbles@example.com", "correct-password").StatusCode).To(Equal(fiber.StatusCreated))

				// With the counter reset, six more failures are all 401 — a non-reset
				// bucket (5 left) would have thrown a 429 by the sixth.
				for i := range 6 {
					Expect(attempt("fumbles@example.com", "wrong").StatusCode).To(Equal(fiber.StatusUnauthorized),
						"failure %d after reset should be 401, not throttled", i+1)
				}
			})

			It("throttles a spraying IP across many distinct addresses", func() {
				// loginPerIPBurst is 30, well above the per-address burst, so 30
				// single failures against distinct unknown addresses stay 401 and the
				// 31st trips the per-IP budget.
				for i := range 30 {
					email := fmt.Sprintf("spray-%d@example.com", i)
					Expect(attempt(email, "wrong").StatusCode).To(Equal(fiber.StatusUnauthorized))
				}
				resp := attempt("spray-final@example.com", "wrong")
				Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
				Expect(resp.Header.Get("Retry-After")).NotTo(BeEmpty())
			})
		})
	})

	// ── Delete (logout) ──────────────────────────────────────────────────────

	Describe("DELETE /api/sessions", func() {
		Context("with a valid session cookie", func() {
			It("returns 204 and clears the cookie", func() {
				seedUser("Carol", "carol@example.com", "password-carol")
				_, session := doLogin("carol@example.com", "password-carol")

				delReq := httptest.NewRequest("DELETE", "/api/sessions", nil)
				delReq.AddCookie(&http.Cookie{Name: testCookieName, Value: session.ID})
				delResp, err := app.Test(delReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(delResp.StatusCode).To(Equal(fiber.StatusNoContent))

				cookies := delResp.Cookies()
				Expect(cookies).To(HaveLen(1))
				Expect(cookies[0].Name).To(Equal(testCookieName))
				Expect(cookies[0].Value).To(BeEmpty())
			})

			It("returns 401 on a second logout with the same token", func() {
				seedUser("Dave", "dave@example.com", "password-dave")
				_, session := doLogin("dave@example.com", "password-dave")

				delReq1 := httptest.NewRequest("DELETE", "/api/sessions", nil)
				delReq1.AddCookie(&http.Cookie{Name: testCookieName, Value: session.ID})
				app.Test(delReq1) //nolint:errcheck

				delReq2 := httptest.NewRequest("DELETE", "/api/sessions", nil)
				delReq2.AddCookie(&http.Cookie{Name: testCookieName, Value: session.ID})
				delResp2, err := app.Test(delReq2)
				Expect(err).NotTo(HaveOccurred())
				Expect(delResp2.StatusCode).To(Equal(fiber.StatusUnauthorized))
			})
		})

		Context("without a cookie", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/sessions", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
			})
		})

		Context("with an invalid token in the cookie", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/sessions", nil)
				req.AddCookie(&http.Cookie{Name: testCookieName, Value: "bogus-token"})
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
			})
		})
	})
})
