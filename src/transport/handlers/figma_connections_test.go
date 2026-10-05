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
)

type connectionWire struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	User  struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

var _ = Describe("Figma plugin connections", Ordered, func() {
	var (
		app                 *fiber.App
		db                  *bun.DB
		tokens              repository.PluginTokenRepository
		owner, jane, bob    *models.User
		ownerTok, janeTok   *models.PluginToken
		bobTok, foreignTok  *models.PluginToken
		ownerCook, janeCook *http.Cookie
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	BeforeEach(func() {
		app = fiber.New()
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tokens = repository.NewPluginTokenRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewFigmaConnectionsHandler(newTestPluginService(db), userRepo, auth, nil).Register(app)

		owner = seedTenantUserWithRole(db, "Olivia", "olivia@example.com", "olivia-password", models.RoleOwner)
		jane = seedTenantUser(db, "Jane", "jane@example.com", "jane-password")
		bob = seedTenantUser(db, "Bob", "bob@example.com", "bob-password")
		_, ownerTok = mintPluginToken(db, owner)
		_, janeTok = mintPluginToken(db, jane)
		_, bobTok = mintPluginToken(db, bob)

		now := time.Now().UTC()
		_, err := db.NewInsert().Model(&models.Tenant{ID: "t-elsewhere", Name: "Elsewhere", Slug: "elsewhere", TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now}).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		stranger := &models.User{ID: "u-stranger", AccountID: owner.AccountID, TenantID: "t-elsewhere", Name: "Olivia", Email: owner.Email, Role: models.RoleOwner}
		_, err = db.NewInsert().Model(stranger).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, foreignTok = mintPluginToken(db, stranger)

		ownerCook = loginAs(app, "olivia@example.com", "olivia-password")
		janeCook = loginAs(app, "jane@example.com", "jane-password")
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"plugin_tokens", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(tenantCtx())
	})

	send := func(method, path string, cookie *http.Cookie) *http.Response {
		GinkgoHelper()
		req := httptest.NewRequest(method, path, nil)
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	list := func(cookie *http.Cookie) []connectionWire {
		GinkgoHelper()
		resp := send(fiber.MethodGet, "/api/integrations/figma/connections", cookie)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var out struct {
			Connections []connectionWire `json:"connections"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		return out.Connections
	}

	ids := func(conns []connectionWire) []string {
		out := make([]string, 0, len(conns))
		for _, c := range conns {
			out = append(out, c.ID)
		}
		return out
	}

	It("shows a member only their own connections", func() {
		conns := list(janeCook)
		Expect(ids(conns)).To(ConsistOf(janeTok.ID))
		Expect(conns[0].Label).To(Equal("Figma · Jane"))
		Expect(conns[0].User.ID).To(Equal(jane.ID))
		Expect(conns[0].User.Email).To(Equal("jane@example.com"))
	})

	It("shows an owner every connection in the workspace, and none from others", func() {
		Expect(ids(list(ownerCook))).To(ConsistOf(ownerTok.ID, janeTok.ID, bobTok.ID))
	})

	It("lets a member disconnect their own connection but not someone else's", func() {
		Expect(send(fiber.MethodDelete, "/api/integrations/figma/connections/"+bobTok.ID, janeCook).StatusCode).To(Equal(fiber.StatusForbidden))
		Expect(send(fiber.MethodDelete, "/api/integrations/figma/connections/"+janeTok.ID, janeCook).StatusCode).To(Equal(fiber.StatusNoContent))
		Expect(list(janeCook)).To(BeEmpty())
		Expect(send(fiber.MethodDelete, "/api/integrations/figma/connections/"+janeTok.ID, janeCook).StatusCode).To(Equal(fiber.StatusNotFound))

		_, err := tokens.GetActive(context.Background(), bobTok.TenantID, bobTok.ID)
		Expect(err).NotTo(HaveOccurred(), "the refused delete must leave Bob's connection live")
	})

	It("lets an owner disconnect anyone's connection in the workspace only", func() {
		Expect(send(fiber.MethodDelete, "/api/integrations/figma/connections/"+bobTok.ID, ownerCook).StatusCode).To(Equal(fiber.StatusNoContent))
		Expect(ids(list(ownerCook))).To(ConsistOf(ownerTok.ID, janeTok.ID))
		Expect(send(fiber.MethodDelete, "/api/integrations/figma/connections/"+foreignTok.ID, ownerCook).StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("requires a signed-in user", func() {
		req := httptest.NewRequest(fiber.MethodGet, "/api/integrations/figma/connections", nil)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnauthorized))
	})
})
