package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// This suite guards the CON-295 fix (Alec's Phase-1 review, finding #1): the
// team_seats cap was enforced on POST /api/users but NOT on the invitation
// accept path — the route the product actually uses to add a teammate. The gate
// runs on a Trial-tier tenant (team_seats = 1, from the seeded trial-v1) whose
// single seat is already taken, so accepting one more must be refused with 402.
var _ = Describe("InvitationsHandler seat cap (CON-295)", Ordered, func() {
	var (
		app *fiber.App
		db  *bun.DB
	)
	ctx := context.Background()
	const trialTenantID = "tn-seatcap-trial"

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

	BeforeEach(func() {
		now := time.Now().UTC()
		// A Trial-tier workspace already at its single-seat cap: one owner member.
		_, err := db.NewInsert().Model(&models.Tenant{
			ID: trialTenantID, Name: "SeatCap Co", Slug: "seatcap-co", TierID: "trial", CreatedAt: now, UpdatedAt: now,
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		hash, err := models.HashPassword("owner-password")
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Account{ID: "seatcap-owner", Email: "seatowner@example.com", PasswordHash: hash, Name: "Seat Owner"}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.User{ID: "seatcap-owner", AccountID: "seatcap-owner", TenantID: trialTenantID, Name: "Seat Owner", Email: "seatowner@example.com", Role: models.RoleOwner}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		// Mirror the production error handler so a *QuotaExceededError renders as 402
		// (defaultErrorHandler in src/transport/server/server.go).
		app = fiber.New(fiber.Config{
			ErrorHandler: func(c *fiber.Ctx, err error) error {
				var qe *entitlements.QuotaExceededError
				if errors.As(err, &qe) {
					return c.Status(fiber.StatusPaymentRequired).JSON(fiber.Map{
						"error": "entitlement_exceeded", "feature": qe.Key, "limit": qe.Limit, "current": qe.Current,
					})
				}
				code := fiber.StatusInternalServerError
				var fe *fiber.Error
				if errors.As(err, &fe) {
					code = fe.Code
				}
				return c.Status(code).JSON(fiber.Map{"error": err.Error()})
			},
		})
		userRepo := repository.NewUserRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		inviteRepo := repository.NewInvitationRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)

		cat, err := entitlements.LoadCatalog()
		Expect(err).NotTo(HaveOccurred())
		resolver := entitlements.NewResolver(repository.NewTenantTierVersionRepository(db), repository.NewTenantTierAssignmentRepository(db), tenantRepo, cat)
		// Enforce mode + the REAL seat counter, so the test also proves the counter is
		// scoped to the invite's tenant (a bug here would count every tenant's users).
		lim := entitlements.NewLimiter(resolver, cat, entitlements.ModeEnforce).
			Register("team_seats", entitlements.CounterFunc(func(c context.Context, _ string) (int64, error) { return userRepo.CountInTenant(c) }))

		ih := handlers.NewInvitationsHandler(db, userRepo, repository.NewAccountRepository(db), tenantRepo, inviteRepo, sessionRepo, "https://app.example.com", testCookieName, false, auth)
		ih.SetLimiter(lim)
		ih.Register(app)
	})

	AfterEach(func() {
		for _, tbl := range []string{"users_invitations", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	seedInvite := func(email string) string {
		GinkgoHelper()
		token, tokenHash, err := models.NewInvitationToken()
		Expect(err).NotTo(HaveOccurred())
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.Invitation{
			ID: id, TenantID: trialTenantID, Email: email, Role: models.RoleMember,
			TokenHash: tokenHash, InvitedBy: "seatcap-owner", Status: models.InvitationPending,
			ExpiresAt: time.Now().Add(24 * time.Hour), CreatedAt: time.Now().UTC(),
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		return token
	}

	accept := func(token string, payload fiber.Map) *http.Response {
		GinkgoHelper()
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/invitations/accept/"+token, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	invStatus := func(email string) string {
		GinkgoHelper()
		inv := new(models.Invitation)
		Expect(db.NewSelect().Model(inv).Where("tenant_id = ? AND email = ?", trialTenantID, email).Scan(ctx)).To(Succeed())
		return inv.Status
	}

	It("refuses acceptNew with 402 when the workspace is at its seat cap", func() {
		token := seedInvite("newbie@example.com")
		resp := accept(token, fiber.Map{"name": "New Bie", "password": "password123"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusPaymentRequired))
		// The invite is left unconsumed so the invitee can retry once a seat frees.
		Expect(invStatus("newbie@example.com")).To(Equal(models.InvitationPending))
	})

	It("admits acceptNew when a seat is available", func() {
		// Free a seat by moving the workspace to Pro (team_seats = 3) so its single
		// owner is under cap. Done this way rather than deleting the owner, because
		// the owner is the invite's invited_by and that FK must still resolve.
		_, err := db.NewUpdate().Model((*models.Tenant)(nil)).Set("tier_id = ?", "pro").Where("id = ?", trialTenantID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		token := seedInvite("newbie@example.com")
		resp := accept(token, fiber.Map{"name": "New Bie", "password": "password123"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(invStatus("newbie@example.com")).To(Equal(models.InvitationAccepted))
	})
})
