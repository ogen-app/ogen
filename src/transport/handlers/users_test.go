package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("UsersHandler", Ordered, func() {
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
		handlers.NewSettingsHandler(settingRepo, auth).Register(app)
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		// Credentials live on accounts since CON-147; clear them too so a
		// reused email doesn't collide with an orphaned account across specs.
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewUpdate().TableExpr("settings").Set("value = ?", "false").
			Where("key = ?", "setup_complete").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// ── helpers ──────────────────────────────────────────────────────────────

	// createUser seeds a user directly in the default tenant. POST /api/users
	// now requires auth, so bootstrap users are inserted via the DB;
	// loginAs below still authenticates them over HTTP.
	createUser := func(name, email, password string) *models.User {
		return seedTenantUser(db, name, email, password)
	}

	// createOwner seeds an owner of the default tenant — the role POST /api/users
	// and the CON-26 team-management endpoints require of the caller.
	createOwner := func(name, email, password string) *models.User {
		return seedTenantUserWithRole(db, name, email, password, models.RoleOwner)
	}

	loginAs := func(email, password string) *http.Cookie {
		body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		cookies := resp.Cookies()
		Expect(cookies).To(HaveLen(1))
		return cookies[0]
	}

	// sessionAlive reports whether a session cookie still authenticates, by hitting
	// a protected endpoint: 200 means the session is live, 401 means it was revoked.
	// Any other status is unexpected and fails the spec rather than being silently
	// read as "revoked".
	sessionAlive := func(cookie *http.Cookie) bool {
		GinkgoHelper()
		req := httptest.NewRequest("GET", "/api/current_user", nil)
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(BeElementOf(fiber.StatusOK, fiber.StatusUnauthorized))
		return resp.StatusCode == fiber.StatusOK
	}

	// loginFails asserts credentials are rejected — used to prove an old password
	// no longer works after a change.
	loginFails := func(email, password string) {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
	}

	// ── CurrentUser ──────────────────────────────────────────────────────────

	Describe("GET /api/current_user", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/current_user", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the authenticated user", func() {
				u := createUser("Alice", "alice@example.com", "password-alice")
				cookie := loginAs("alice@example.com", "password-alice")

				req := httptest.NewRequest("GET", "/api/current_user", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.User
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.ID).To(Equal(u.ID))
				Expect(got.Name).To(Equal("Alice"))
				Expect(got.Email).To(Equal("alice@example.com"))
			})

			It("embeds the caller's tenant (CON-97)", func() {
				createUser("Alice", "alice@example.com", "password-alice")
				cookie := loginAs("alice@example.com", "password-alice")

				req := httptest.NewRequest("GET", "/api/current_user", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var raw map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
				tenant, ok := raw["tenant"].(map[string]any)
				Expect(ok).To(BeTrue(), "current_user must embed a tenant object")
				Expect(tenant["id"]).To(Equal(models.DefaultTenantID))
				Expect(tenant["slug"]).To(Equal("default"))
			})

			It("does not expose password_hash in the response", func() {
				createUser("Alice", "alice@example.com", "password-alice")
				cookie := loginAs("alice@example.com", "password-alice")

				req := httptest.NewRequest("GET", "/api/current_user", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())

				var raw map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
				Expect(raw).NotTo(HaveKey("password_hash"))
				Expect(raw).NotTo(HaveKey("PasswordHash"))
			})
		})
	})

	// ── List ─────────────────────────────────────────────────────────────────

	Describe("GET /api/users", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/users", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns all users", func() {
				createUser("Alice", "alice@example.com", "password-alice")
				createUser("Bob", "bob@example.com", "password-bob")
				cookie := loginAs("alice@example.com", "password-alice")

				req := httptest.NewRequest("GET", "/api/users", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var users []models.User
				Expect(json.NewDecoder(resp.Body).Decode(&users)).To(Succeed())
				Expect(users).To(HaveLen(2))
			})

			It("does not expose password_hash in the response", func() {
				createUser("Alice", "alice@example.com", "password-alice")
				cookie := loginAs("alice@example.com", "password-alice")

				req := httptest.NewRequest("GET", "/api/users", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())

				var raw []map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
				for _, u := range raw {
					Expect(u).NotTo(HaveKey("password_hash"))
					Expect(u).NotTo(HaveKey("PasswordHash"))
				}
			})
		})
	})

	// ── Create ───────────────────────────────────────────────────────────────

	Describe("POST /api/users", func() {
		Context("when not authenticated", func() {
			It("returns 401 (signup is the only open bootstrap — CON-97)", func() {
				body, _ := json.Marshal(fiber.Map{"name": "X", "email": "x@example.com", "password": "s3cur3P@ss"})
				req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("with a valid payload and an authenticated caller", func() {
			It("creates a user in the caller's tenant and returns 201", func() {
				createOwner("Admin", "admin@example.com", "admin-password")
				cookie := loginAs("admin@example.com", "admin-password")

				body, _ := json.Marshal(fiber.Map{"name": "Carol", "email": "carol@example.com", "password": "s3cur3P@ss"})
				req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

				var u models.User
				Expect(json.NewDecoder(resp.Body).Decode(&u)).To(Succeed())
				Expect(u.ID).NotTo(BeEmpty())
				Expect(u.Email).To(Equal("carol@example.com"))

				// The new user is attached to the caller's (default) tenant,
				// never to a body-supplied tenant.
				var stored models.User
				Expect(db.NewSelect().Model(&stored).Where("id = ?", u.ID).
					Scan(context.Background())).To(Succeed())
				Expect(stored.TenantID).To(Equal(models.DefaultTenantID))
			})

			It("ignores a tenant_id in the request body", func() {
				createOwner("Admin", "admin@example.com", "admin-password")
				cookie := loginAs("admin@example.com", "admin-password")

				body, _ := json.Marshal(fiber.Map{
					"name": "Mallory", "email": "mallory@example.com",
					"password": "s3cur3P@ss", "tenant_id": "some-other-tenant",
				})
				req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))

				var u models.User
				Expect(json.NewDecoder(resp.Body).Decode(&u)).To(Succeed())
				var stored models.User
				Expect(db.NewSelect().Model(&stored).Where("id = ?", u.ID).
					Scan(context.Background())).To(Succeed())
				Expect(stored.TenantID).To(Equal(models.DefaultTenantID))
			})

			It("does not expose the password hash in the response", func() {
				createOwner("Admin", "admin@example.com", "admin-password")
				cookie := loginAs("admin@example.com", "admin-password")

				body, _ := json.Marshal(fiber.Map{"name": "X", "email": "x@example.com", "password": "s3cur3P@ss"})
				req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())

				var raw map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
				Expect(raw).NotTo(HaveKey("password_hash"))
				Expect(raw).NotTo(HaveKey("PasswordHash"))
				Expect(raw).NotTo(HaveKey("password"))
			})
		})

		Context("with invalid payload (authenticated)", func() {
			DescribeTable("returns 400",
				func(payload fiber.Map) {
					createOwner("Admin", "admin@example.com", "admin-password")
					cookie := loginAs("admin@example.com", "admin-password")

					body, _ := json.Marshal(payload)
					req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.AddCookie(cookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(400))
				},
				Entry("missing name", fiber.Map{"email": "x@example.com", "password": "s3cur3P@ss"}),
				Entry("missing email", fiber.Map{"name": "X", "password": "s3cur3P@ss"}),
				Entry("missing password", fiber.Map{"name": "X", "email": "x@example.com"}),
				Entry("empty body", fiber.Map{}),
				Entry("invalid email format", fiber.Map{"name": "X", "email": "not-an-email", "password": "s3cur3P@ss"}),
				Entry("email missing domain", fiber.Map{"name": "X", "email": "user@", "password": "s3cur3P@ss"}),
				Entry("email missing @", fiber.Map{"name": "X", "email": "userexample.com", "password": "s3cur3P@ss"}),
				Entry("password too short", fiber.Map{"name": "X", "email": "x@example.com", "password": "short"}),
			)

			It("returns a descriptive error message for invalid email", func() {
				createOwner("Admin", "admin@example.com", "admin-password")
				cookie := loginAs("admin@example.com", "admin-password")

				body, _ := json.Marshal(fiber.Map{"name": "X", "email": "bad-email", "password": "s3cur3P@ss"})
				req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))

				var payload map[string]string
				Expect(json.NewDecoder(resp.Body).Decode(&payload)).To(Succeed())
				Expect(payload["error"]).To(ContainSubstring("email"))
			})

			It("returns a descriptive error message for short password", func() {
				createOwner("Admin", "admin@example.com", "admin-password")
				cookie := loginAs("admin@example.com", "admin-password")

				body, _ := json.Marshal(fiber.Map{"name": "X", "email": "x@example.com", "password": "short"})
				req := httptest.NewRequest("POST", "/api/users", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))

				var payload map[string]string
				Expect(json.NewDecoder(resp.Body).Decode(&payload)).To(Succeed())
				Expect(payload["error"]).To(ContainSubstring("password"))
			})
		})
	})

	// ── Get ──────────────────────────────────────────────────────────────────

	Describe("GET /api/users/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/users/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated and the user exists", func() {
			It("returns the user without password hash", func() {
				created := createUser("Dave", "dave@example.com", "password-dave")
				cookie := loginAs("dave@example.com", "password-dave")

				req := httptest.NewRequest("GET", fmt.Sprintf("/api/users/%s", created.ID), nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var raw map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
				Expect(raw["id"]).To(Equal(created.ID))
				Expect(raw["email"]).To(Equal("dave@example.com"))
				Expect(raw).NotTo(HaveKey("password_hash"))
			})
		})

		Context("when authenticated and the user does not exist", func() {
			It("returns 404", func() {
				createUser("Dave", "dave@example.com", "password-dave")
				cookie := loginAs("dave@example.com", "password-dave")

				req := httptest.NewRequest("GET", "/api/users/doesnotexist", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Update ───────────────────────────────────────────────────────────────

	Describe("PUT /api/users/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"name": "X", "email": "x@example.com"})
				req := httptest.NewRequest("PUT", "/api/users/someid", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated and the user exists", func() {
			It("updates name and email without changing the password", func() {
				created := createUser("Eve", "eve@example.com", "password-eve")
				cookie := loginAs("eve@example.com", "password-eve")

				body, _ := json.Marshal(fiber.Map{"name": "Eve Updated", "email": "eve2@example.com"})
				req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", created.ID), bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var u models.User
				Expect(json.NewDecoder(resp.Body).Decode(&u)).To(Succeed())
				Expect(u.Name).To(Equal("Eve Updated"))
				Expect(u.Email).To(Equal("eve2@example.com"))
			})

			It("updates the password when the current password is supplied", func() {
				created := createUser("Eve2", "eve2@example.com", "old-password")
				cookie := loginAs("eve2@example.com", "old-password")

				body, _ := json.Marshal(fiber.Map{
					"name": "Eve2", "email": "eve2@example.com",
					"current_password": "old-password", "password": "new-password",
				})
				req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", created.ID), bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				// The credential actually rotated: new works, old no longer does.
				Expect(loginAs("eve2@example.com", "new-password")).NotTo(BeNil())
				loginFails("eve2@example.com", "old-password")
			})
		})

		// ── CON-193: current-password re-auth + session revocation ────────────────
		Context("when changing the password (CON-193)", func() {
			It("rejects a password change that omits the current password with 400", func() {
				created := createUser("Nadia", "nadia@example.com", "old-password")
				cookie := loginAs("nadia@example.com", "old-password")

				body, _ := json.Marshal(fiber.Map{
					"name": "Nadia", "email": "nadia@example.com", "password": "new-password",
				})
				req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", created.ID), bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))

				// Nothing changed: the old password still logs in.
				Expect(loginAs("nadia@example.com", "old-password")).NotTo(BeNil())
			})

			It("rejects a wrong current password with 403 and leaves the credential intact", func() {
				created := createUser("Omar", "omar@example.com", "old-password")
				cookie := loginAs("omar@example.com", "old-password")

				body, _ := json.Marshal(fiber.Map{
					"name": "Omar", "email": "omar@example.com",
					"current_password": "not-my-password", "password": "new-password",
				})
				req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", created.ID), bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(403))

				// The password was not rotated.
				Expect(loginAs("omar@example.com", "old-password")).NotTo(BeNil())
				loginFails("omar@example.com", "new-password")
			})

			It("revokes the user's other sessions but keeps the caller's own", func() {
				created := createUser("Rita", "rita@example.com", "old-password")
				// Two independent live sessions for the same user.
				caller := loginAs("rita@example.com", "old-password")
				other := loginAs("rita@example.com", "old-password")
				Expect(sessionAlive(caller)).To(BeTrue())
				Expect(sessionAlive(other)).To(BeTrue())

				body, _ := json.Marshal(fiber.Map{
					"name": "Rita", "email": "rita@example.com",
					"current_password": "old-password", "password": "brand-new-password",
				})
				req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", created.ID), bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(caller)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				// The caller stays signed in on the tab making the change...
				Expect(sessionAlive(caller)).To(BeTrue())
				// ...while every other session is evicted.
				Expect(sessionAlive(other)).To(BeFalse())

				// And the credential rotated.
				Expect(loginAs("rita@example.com", "brand-new-password")).NotTo(BeNil())
				loginFails("rita@example.com", "old-password")
			})

			It("does not touch other sessions on a name/email-only edit", func() {
				created := createUser("Sam", "sam@example.com", "password-sam")
				caller := loginAs("sam@example.com", "password-sam")
				other := loginAs("sam@example.com", "password-sam")

				body, _ := json.Marshal(fiber.Map{"name": "Sam Renamed", "email": "sam@example.com"})
				req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", created.ID), bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(caller)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				// No password change → no revocation; both sessions remain live.
				Expect(sessionAlive(caller)).To(BeTrue())
				Expect(sessionAlive(other)).To(BeTrue())
			})
		})

		Context("when authenticated as a different user", func() {
			It("returns 403 and does not modify the target user", func() {
				target := createUser("Target", "target@example.com", "password-target")
				createUser("Other", "other@example.com", "password-other")
				cookie := loginAs("other@example.com", "password-other")

				body, _ := json.Marshal(fiber.Map{"name": "Hacked", "email": "hacked@example.com"})
				req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", target.ID), bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(403))

				// The target user is unchanged and the original password still works.
				Expect(loginAs("target@example.com", "password-target")).NotTo(BeNil())
			})
		})

		Context("when authenticated as self but the route id does not exist", func() {
			It("returns 403", func() {
				createUser("Frank", "frank@example.com", "password-frank")
				cookie := loginAs("frank@example.com", "password-frank")

				body, _ := json.Marshal(fiber.Map{"name": "X", "email": "x@example.com"})
				req := httptest.NewRequest("PUT", "/api/users/doesnotexist", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(403))
			})
		})

		Context("when authenticated with invalid payload", func() {
			DescribeTable("returns 400",
				func(payload fiber.Map) {
					created := createUser("Frank", "frank@example.com", "password-frank")
					cookie := loginAs("frank@example.com", "password-frank")

					body, _ := json.Marshal(payload)
					req := httptest.NewRequest("PUT", fmt.Sprintf("/api/users/%s", created.ID), bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.AddCookie(cookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(400))
				},
				Entry("missing email", fiber.Map{"name": "Frank"}),
				Entry("missing name", fiber.Map{"email": "frank@example.com"}),
				Entry("invalid email format", fiber.Map{"name": "Frank", "email": "not-an-email"}),
				Entry("password too short", fiber.Map{"name": "Frank", "email": "frank@example.com", "password": "short"}),
			)
		})
	})

	// ── Delete ───────────────────────────────────────────────────────────────

	Describe("DELETE /api/users/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/users/someid", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated and the user exists", func() {
			It("deletes the user and returns 204", func() {
				created := createUser("Grace", "grace@example.com", "password-grace")
				cookie := loginAs("grace@example.com", "password-grace")

				req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/users/%s", created.ID), nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(204))

				body, _ := io.ReadAll(resp.Body)
				Expect(body).To(BeEmpty())
			})

			It("makes the user unreachable on subsequent get", func() {
				henry := createUser("Henry", "henry@example.com", "password-henry")
				cookie := loginAs("henry@example.com", "password-henry")

				delReq := httptest.NewRequest("DELETE", fmt.Sprintf("/api/users/%s", henry.ID), nil)
				delReq.AddCookie(cookie)
				delResp, delErr := app.Test(delReq)
				Expect(delErr).NotTo(HaveOccurred())
				Expect(delResp.StatusCode).To(Equal(204))

				// Session was tied to the deleted user, so the follow-up Get is now
				// unauthenticated and the auth middleware returns 401.
				getReq := httptest.NewRequest("GET", fmt.Sprintf("/api/users/%s", henry.ID), nil)
				getReq.AddCookie(cookie)
				resp, err := app.Test(getReq)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated as a different user", func() {
			It("returns 403 and does not delete the target user", func() {
				target := createUser("Target", "target@example.com", "password-target")
				createUser("Other", "other@example.com", "password-other")
				cookie := loginAs("other@example.com", "password-other")

				req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/users/%s", target.ID), nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(403))

				// Target still exists and can still log in.
				Expect(loginAs("target@example.com", "password-target")).NotTo(BeNil())
			})
		})

		Context("when authenticated as self but the route id does not exist", func() {
			It("returns 403", func() {
				createUser("Iris", "iris@example.com", "password-iris")
				cookie := loginAs("iris@example.com", "password-iris")

				req := httptest.NewRequest("DELETE", "/api/users/doesnotexist", nil)
				req.AddCookie(cookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(403))
			})
		})
	})

})

// mustOpenTestDBWithMigrations returns a fresh, isolated, fully-migrated
// Postgres database. Each call gets its own database, so
// Describes never leak rows into each other. The Postgres instance is
// provisioned by the Makefile (TEST_DATABASE_DSN); pgtest.MustDB creates
// and migrates a unique database per call.
func mustOpenTestDBWithMigrations() *bun.DB {
	return pgtest.MustDB()
}
