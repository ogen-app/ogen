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
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("PasswordResetHandler", Ordered, func() {
	var (
		app *fiber.App
		db  *bun.DB
	)

	ctx := context.Background()

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
		accountRepo := repository.NewAccountRepository(db)
		// No activity recorder and no email enqueuer wired: both are nil-safe, so
		// a reset request mints + stores a token (observable) without needing the
		// River/analytics stack. Enqueuer-specific behaviour is covered in the
		// jobs package.
		handlers.NewPasswordResetHandler(db, userRepo, accountRepo, "https://app.example.com").Register(app)
	})

	AfterEach(func() {
		_, err := db.NewDelete().TableExpr("password_reset_tokens").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		// The credential now lives on accounts (CON-147); each seedTenantUser
		// inserts one, and specs reuse the same emails, so clear it too or the
		// unique-email constraint collides across specs.
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	// ── helpers ────────────────────────────────────────────────────────────────

	doRequest := func(email string) *http.Response {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{"email": email})
		req := httptest.NewRequest("POST", "/api/password-reset", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	doConfirm := func(token, password string) *http.Response {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{"token": token, "password": password})
		req := httptest.NewRequest("POST", "/api/password-reset/confirm", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	bodyError := func(resp *http.Response) string {
		GinkgoHelper()
		var m map[string]string
		Expect(json.NewDecoder(resp.Body).Decode(&m)).To(Succeed())
		return m["error"]
	}

	bodyBytes := func(resp *http.Response) []byte {
		GinkgoHelper()
		b, err := io.ReadAll(resp.Body)
		Expect(err).NotTo(HaveOccurred())
		return b
	}

	seedResetToken := func(userID, tenantID string, expiresAt time.Time) string {
		GinkgoHelper()
		token, hash, err := models.NewResetToken()
		Expect(err).NotTo(HaveOccurred())
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		row := &models.PasswordResetToken{
			ID: id, UserID: userID, TenantID: tenantID,
			TokenHash: hash, ExpiresAt: expiresAt, CreatedAt: time.Now().UTC(),
		}
		_, err = db.NewInsert().Model(row).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		return token
	}

	seedSession := func(userID, tenantID string) {
		GinkgoHelper()
		tok, err := models.NewSessionToken()
		Expect(err).NotTo(HaveOccurred())
		// seedTenantUser seeds the account with the same id as the user (1:1), so
		// the session's account_id can reuse the user id here.
		s := &models.Session{ID: tok, AccountID: userID, UserID: userID, TenantID: tenantID, ExpiresAt: time.Now().UTC().Add(time.Hour)}
		_, err = db.NewInsert().Model(s).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	}

	// getAccountFor loads the login account behind a user membership. Since
	// CON-147 the credential lives on accounts, so a reset asserts on
	// account.PasswordHash rather than the (removed) user.PasswordHash.
	getAccountFor := func(userID string) *models.Account {
		GinkgoHelper()
		acct := new(models.Account)
		Expect(db.NewSelect().Model(acct).
			Where("id = (SELECT account_id FROM users WHERE id = ?)", userID).
			Scan(ctx)).To(Succeed())
		return acct
	}

	countTokens := func(userID string) int {
		GinkgoHelper()
		n, err := db.NewSelect().Model((*models.PasswordResetToken)(nil)).Where("user_id = ?", userID).Count(ctx)
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	countSessions := func(userID string) int {
		GinkgoHelper()
		n, err := db.NewSelect().Model((*models.Session)(nil)).Where("user_id = ?", userID).Count(ctx)
		Expect(err).NotTo(HaveOccurred())
		return n
	}

	getToken := func(userID string) *models.PasswordResetToken {
		GinkgoHelper()
		row := new(models.PasswordResetToken)
		Expect(db.NewSelect().Model(row).Where("user_id = ?", userID).Scan(ctx)).To(Succeed())
		return row
	}

	// ── POST /api/password-reset ────────────────────────────────────────────────

	Describe("POST /api/password-reset", func() {
		It("returns 202 and stores only a hashed, unspent, 1h token on a hit", func() {
			u := seedTenantUser(db, "Reset Me", "reset@example.com", "old-password-123")

			resp := doRequest("reset@example.com")
			Expect(resp.StatusCode).To(Equal(fiber.StatusAccepted))

			// The mint happens off the response path, so observe it eventually.
			Eventually(func() int { return countTokens(u.ID) }).Should(Equal(1))

			row := getToken(u.ID)
			Expect(row.TokenHash).To(HaveLen(64)) // hex sha256, never the plaintext
			Expect(row.ConsumedAt).To(BeNil())
			Expect(row.ExpiresAt).To(BeTemporally("~", time.Now().UTC().Add(time.Hour), time.Minute))
		})

		It("returns 202 for an unknown address, indistinguishable from a hit", func() {
			seedTenantUser(db, "Reset Me", "reset@example.com", "old-password-123")

			hit := doRequest("reset@example.com")
			miss := doRequest("nobody@example.com")

			// Same status, same (empty) body — no structural tell.
			Expect(miss.StatusCode).To(Equal(fiber.StatusAccepted))
			Expect(hit.StatusCode).To(Equal(miss.StatusCode))
			Expect(bodyBytes(miss)).To(Equal(bodyBytes(hit)))

			// And a miss mints nothing.
			Consistently(func() int {
				n, err := db.NewSelect().Model((*models.PasswordResetToken)(nil)).
					Where("tenant_id = ?", models.DefaultTenantID).Count(ctx)
				Expect(err).NotTo(HaveOccurred())
				return n
			}, "300ms", "50ms").Should(BeNumerically("<=", 1)) // only the hit's token, never the miss's
		})

		It("returns 429 with Retry-After once the per-address budget is spent", func() {
			u := seedTenantUser(db, "Reset Me", "reset@example.com", "old-password-123")

			for range 5 {
				Expect(doRequest("reset@example.com").StatusCode).To(Equal(fiber.StatusAccepted))
			}
			resp := doRequest("reset@example.com")
			Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
			Expect(resp.Header.Get("Retry-After")).NotTo(BeEmpty())

			// Each of the 5 accepted requests mints off the response path. Wait for
			// all five to commit before the spec exits, so the detached goroutines
			// aren't still inserting when AfterEach deletes the user (which would log
			// spurious FK violations). The rate-limited 6th never reaches dispatch,
			// so exactly five are expected.
			Eventually(func() int { return countTokens(u.ID) }).Should(Equal(5))
		})

		DescribeTable("returns 400 on an invalid payload",
			func(payload fiber.Map) {
				body, _ := json.Marshal(payload)
				req := httptest.NewRequest("POST", "/api/password-reset", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
			},
			Entry("missing email", fiber.Map{}),
			Entry("invalid email format", fiber.Map{"email": "not-an-email"}),
		)
	})

	// ── POST /api/password-reset/confirm ────────────────────────────────────────

	Describe("POST /api/password-reset/confirm", func() {
		It("sets the new password, consumes the token, and revokes all sessions", func() {
			u := seedTenantUser(db, "Carla", "carla@example.com", "old-password-123")
			seedSession(u.ID, u.TenantID)
			seedSession(u.ID, u.TenantID)
			token := seedResetToken(u.ID, u.TenantID, time.Now().UTC().Add(time.Hour))

			resp := doConfirm(token, "new-password-456")
			Expect(resp.StatusCode).To(Equal(fiber.StatusNoContent))

			got := getAccountFor(u.ID)
			newOK, _ := models.VerifyPassword("new-password-456", got.PasswordHash)
			Expect(newOK).To(BeTrue())
			oldOK, _ := models.VerifyPassword("old-password-123", got.PasswordHash)
			Expect(oldOK).To(BeFalse())

			Expect(getToken(u.ID).ConsumedAt).NotTo(BeNil())
			Expect(countSessions(u.ID)).To(Equal(0))
		})

		It("rejects a replayed token with 400 and a human-readable error", func() {
			u := seedTenantUser(db, "Rey", "rey@example.com", "old-password-123")
			token := seedResetToken(u.ID, u.TenantID, time.Now().UTC().Add(time.Hour))

			Expect(doConfirm(token, "new-password-456").StatusCode).To(Equal(fiber.StatusNoContent))

			replay := doConfirm(token, "another-password-789")
			Expect(replay.StatusCode).To(Equal(fiber.StatusBadRequest))
			Expect(bodyError(replay)).To(ContainSubstring("invalid or has expired"))
		})

		It("rejects an expired token with 400 and leaves the password unchanged", func() {
			u := seedTenantUser(db, "Expo", "expo@example.com", "old-password-123")
			token := seedResetToken(u.ID, u.TenantID, time.Now().UTC().Add(-time.Minute))

			resp := doConfirm(token, "new-password-456")
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))

			oldOK, _ := models.VerifyPassword("old-password-123", getAccountFor(u.ID).PasswordHash)
			Expect(oldOK).To(BeTrue())
		})

		It("rejects an unknown token with 400", func() {
			resp := doConfirm("totally-bogus-token", "new-password-456")
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
		})

		It("rejects a too-short password without consuming the token", func() {
			u := seedTenantUser(db, "Weak", "weak@example.com", "old-password-123")
			token := seedResetToken(u.ID, u.TenantID, time.Now().UTC().Add(time.Hour))

			weak := doConfirm(token, "short")
			Expect(weak.StatusCode).To(Equal(fiber.StatusBadRequest))
			Expect(getToken(u.ID).ConsumedAt).To(BeNil()) // link not burned

			// The same link still works with a valid password.
			Expect(doConfirm(token, "new-password-456").StatusCode).To(Equal(fiber.StatusNoContent))
		})

		It("voids the user's other outstanding tokens when one is spent", func() {
			u := seedTenantUser(db, "Multi", "multi@example.com", "old-password-123")
			t1 := seedResetToken(u.ID, u.TenantID, time.Now().UTC().Add(time.Hour))
			t2 := seedResetToken(u.ID, u.TenantID, time.Now().UTC().Add(time.Hour))

			Expect(doConfirm(t1, "new-password-456").StatusCode).To(Equal(fiber.StatusNoContent))

			// t2 was invalidated by spending t1.
			replay := doConfirm(t2, "another-password-789")
			Expect(replay.StatusCode).To(Equal(fiber.StatusBadRequest))
		})
	})
})
