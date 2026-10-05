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
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// mintPluginToken stores a fresh plugin token for user and returns its
// plaintext.
func mintPluginToken(db *bun.DB, user *models.User) (string, *models.PluginToken) {
	GinkgoHelper()
	raw, hash, err := models.NewPluginToken()
	Expect(err).NotTo(HaveOccurred())
	id, err := models.NewID()
	Expect(err).NotTo(HaveOccurred())
	tok := &models.PluginToken{
		ID: id, TenantID: user.TenantID, AccountID: user.AccountID, UserID: user.ID,
		Client: models.PluginClientFigma, Label: "Figma · " + user.Name, TokenHash: hash,
	}
	Expect(repository.NewPluginTokenRepository(db).Create(context.Background(), nil, tok)).To(Succeed())
	return raw, tok
}

// loginAs signs in through POST /api/sessions and returns the session cookie.
func loginAs(app *fiber.App, email, password string) *http.Cookie {
	GinkgoHelper()
	body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
	req := httptest.NewRequest(fiber.MethodPost, "/api/sessions", bytes.NewReader(body))
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	resp, err := app.Test(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
	Expect(resp.Cookies()).NotTo(BeEmpty())
	return resp.Cookies()[0]
}

var _ = Describe("RequirePluginToken", Ordered, func() {
	var (
		app    *fiber.App
		db     *bun.DB
		tokens repository.PluginTokenRepository
		jane   *models.User
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		app = fiber.New()
		userRepo := repository.NewUserRepository(db)
		tokens = repository.NewPluginTokenRepository(db)
		auth := handlers.RequireAuth(repository.NewSessionRepository(db), userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), repository.NewSessionRepository(db), testCookieName, false, nil).Register(app)

		// Echo what the middleware resolved, the way handlers read it.
		app.Get("/api/plugins/figma/whoami", handlers.RequirePluginToken(tokens, userRepo), func(c *fiber.Ctx) error {
			s := c.Locals("session").(*models.Session)
			tid, _ := tenantctx.From(c.Context())
			return c.JSON(fiber.Map{"user_id": s.UserID, "tenant_id": s.TenantID, "ctx_tenant": tid})
		})
		// A cookie-authenticated route, to prove plugin tokens don't reach it.
		app.Get("/api/cookie-only", auth, func(c *fiber.Ctx) error { return c.SendStatus(fiber.StatusOK) })

		jane = seedTenantUser(db, "Jane", "jane@example.com", "jane-password")
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"plugin_tokens", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(tenantCtx())
	})

	call := func(path, authz string, cookie *http.Cookie) *http.Response {
		GinkgoHelper()
		req := httptest.NewRequest(fiber.MethodGet, path, nil)
		if authz != "" {
			req.Header.Set(fiber.HeaderAuthorization, authz)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	expectInvalid := func(resp *http.Response) {
		GinkgoHelper()
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
		var body map[string]string
		raw, _ := io.ReadAll(resp.Body)
		Expect(json.Unmarshal(raw, &body)).To(Succeed())
		Expect(body["code"]).To(Equal(handlers.CodePluginTokenInvalid))
	}

	It("resolves the token's membership into the request session and tenant", func() {
		raw, tok := mintPluginToken(db, jane)
		resp := call("/api/plugins/figma/whoami", "Bearer "+raw, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var body map[string]string
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body).To(Equal(map[string]string{
			"user_id": jane.ID, "tenant_id": models.DefaultTenantID, "ctx_tenant": models.DefaultTenantID,
		}))

		stored, err := tokens.GetActive(context.Background(), tok.TenantID, tok.ID)
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.LastUsedAt).NotTo(BeNil())
	})

	It("accepts the scheme case-insensitively", func() {
		raw, _ := mintPluginToken(db, jane)
		Expect(call("/api/plugins/figma/whoami", "bearer "+raw, nil).StatusCode).To(Equal(fiber.StatusOK))
	})

	It("refuses a missing, malformed or unknown token", func() {
		expectInvalid(call("/api/plugins/figma/whoami", "", nil))
		expectInvalid(call("/api/plugins/figma/whoami", "Basic abc", nil))
		expectInvalid(call("/api/plugins/figma/whoami", "Bearer not-a-plugin-token", nil))
		expectInvalid(call("/api/plugins/figma/whoami", "Bearer ogp_unknown", nil))
	})

	It("refuses a revoked token", func() {
		raw, tok := mintPluginToken(db, jane)
		ok, err := tokens.Revoke(context.Background(), tok.TenantID, tok.ID, time.Now().UTC())
		Expect(err).NotTo(HaveOccurred())
		Expect(ok).To(BeTrue())
		expectInvalid(call("/api/plugins/figma/whoami", "Bearer "+raw, nil))
	})

	It("refuses the token once the member is removed", func() {
		raw, _ := mintPluginToken(db, jane)
		_, err := db.NewDelete().TableExpr("users").Where("id = ?", jane.ID).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		expectInvalid(call("/api/plugins/figma/whoami", "Bearer "+raw, nil))
	})

	DescribeTable("refuses the token once the workspace is not active",
		func(status string) {
			now := time.Now().UTC()
			tid := "t-plugin-" + status
			_, err := db.NewInsert().Model(&models.Tenant{ID: tid, Name: tid, Slug: tid, TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now}).Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
			member := &models.User{ID: "u-" + tid, AccountID: jane.AccountID, TenantID: tid, Name: "Jane", Email: jane.Email, Role: models.RoleOwner}
			_, err = db.NewInsert().Model(member).Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
			raw, _ := mintPluginToken(db, member)
			Expect(call("/api/plugins/figma/whoami", "Bearer "+raw, nil).StatusCode).To(Equal(fiber.StatusOK))

			q := db.NewUpdate().TableExpr("tenants").Set("status = ?", status).Where("id = ?", tid)
			if status == models.TenantStatusDeleted {
				q = q.Set("deleted_at = ?", now)
			}
			_, err = q.Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
			expectInvalid(call("/api/plugins/figma/whoami", "Bearer "+raw, nil))
		},
		Entry("suspended", models.TenantStatusSuspended),
		Entry("deleted", models.TenantStatusDeleted),
	)

	It("ignores a session cookie on plugin routes", func() {
		cookie := loginAs(app, "jane@example.com", "jane-password")
		expectInvalid(call("/api/plugins/figma/whoami", "", cookie))
	})

	It("is not accepted by cookie-authenticated routes", func() {
		raw, _ := mintPluginToken(db, jane)
		Expect(call("/api/cookie-only", "Bearer "+raw, nil).StatusCode).To(Equal(fiber.StatusUnauthorized))
	})
})
