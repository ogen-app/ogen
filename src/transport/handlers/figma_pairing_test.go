package handlers_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/plugins"
)

const testAppBaseURL = "https://app.example.com"

// newTestPluginService wires the pairing use case over the test DB with a
// throwaway KEK.
func newTestPluginService(db *bun.DB) *plugins.Service {
	GinkgoHelper()
	kek := make([]byte, envelope.KeySize)
	_, err := rand.Read(kek)
	Expect(err).NotTo(HaveOccurred())
	cipher, err := envelope.NewCipher(kek)
	Expect(err).NotTo(HaveOccurred())
	return plugins.New(plugins.Deps{
		DB:       db,
		Pairings: repository.NewPluginPairingRepository(db),
		Tokens:   repository.NewPluginTokenRepository(db),
		Users:    repository.NewUserRepository(db),
		Cipher:   cipher,
		Notifier: notify.New(repository.NewNotificationRepository(db), nil),
	})
}

type startedPairing struct {
	ReadKey        string    `json:"read_key"`
	WriteKey       string    `json:"write_key"`
	ApproveURL     string    `json:"approve_url"`
	ExpiresAt      time.Time `json:"expires_at"`
	PollIntervalMS int64     `json:"poll_interval_ms"`
}

type collectedPairing struct {
	Status    string `json:"status"`
	Code      string `json:"code"`
	Token     string `json:"token"`
	Workspace struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"workspace"`
	User struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`
}

var _ = Describe("Figma plugin pairing", Ordered, func() {
	var (
		app    *fiber.App
		db     *bun.DB
		tokens repository.PluginTokenRepository
		jane   *models.User
		cookie *http.Cookie
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
		svc := newTestPluginService(db)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewFigmaPluginHandler(handlers.FigmaPluginDeps{Pairing: svc, AppBaseURL: testAppBaseURL + "/", Tokens: tokens, Users: userRepo}).Register(app)
		handlers.NewFigmaConnectionsHandler(svc, userRepo, auth, nil).Register(app)

		jane = seedTenantUser(db, "Jane", "jane@example.com", "jane-password")
		cookie = loginAs(app, "jane@example.com", "jane-password")
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"plugin_pairings", "plugin_tokens", "notifications", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(tenantCtx())
	})

	send := func(method, path string, body any, cookie *http.Cookie, headers map[string]string) *http.Response {
		GinkgoHelper()
		var rd io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	decode := func(resp *http.Response, into any) {
		GinkgoHelper()
		Expect(json.NewDecoder(resp.Body).Decode(into)).To(Succeed())
	}

	start := func(headers map[string]string) startedPairing {
		GinkgoHelper()
		resp := send(fiber.MethodPost, "/api/plugins/figma/pairings", fiber.Map{"client_label": "Figma · Jane"}, nil, headers)
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var p startedPairing
		decode(resp, &p)
		return p
	}

	poll := func(readKey string) (int, collectedPairing) {
		GinkgoHelper()
		resp := send(fiber.MethodGet, "/api/plugins/figma/pairings/"+readKey, nil, nil, nil)
		var body collectedPairing
		decode(resp, &body)
		return resp.StatusCode, body
	}

	It("pairs end to end: start → preview → approve → collect once", func() {
		p := start(nil)
		Expect(p.ReadKey).NotTo(BeEmpty())
		Expect(p.WriteKey).NotTo(Equal(p.ReadKey))
		Expect(p.ApproveURL).To(Equal(testAppBaseURL + "/integrations/figma/connect?key=" + p.WriteKey))
		Expect(p.PollIntervalMS).To(BeEquivalentTo(2000))
		Expect(p.ExpiresAt).To(BeTemporally("~", time.Now().Add(models.PluginPairingTTL), time.Minute))

		status, body := poll(p.ReadKey)
		Expect(status).To(Equal(fiber.StatusAccepted))
		Expect(body.Status).To(Equal("pending"))

		resp := send(fiber.MethodGet, "/api/integrations/figma/pairings/"+p.WriteKey, nil, cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var preview map[string]any
		decode(resp, &preview)
		Expect(preview).To(HaveKeyWithValue("client", "figma"))
		Expect(preview).To(HaveKeyWithValue("client_label", "Figma · Jane"))
		Expect(preview).To(HaveKeyWithValue("status", "pending"))
		Expect(preview).To(HaveKey("created_ip"))

		resp = send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.WriteKey+"/approve", nil, cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var approved struct {
			Connection struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"connection"`
		}
		decode(resp, &approved)
		Expect(approved.Connection.Label).To(Equal("Figma · Jane"))

		resp = send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.WriteKey+"/approve", nil, cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))

		status, body = poll(p.ReadKey)
		Expect(status).To(Equal(fiber.StatusOK))
		Expect(body.Token).To(HavePrefix(models.PluginTokenPrefix))
		Expect(body.Workspace.ID).To(Equal(models.DefaultTenantID))
		Expect(body.Workspace.Name).NotTo(BeEmpty())
		Expect(body.User.ID).To(Equal(jane.ID))
		Expect(body.User.Email).To(Equal("jane@example.com"))

		stored, err := tokens.GetActiveByHash(context.Background(), models.HashPluginSecret(body.Token))
		Expect(err).NotTo(HaveOccurred())
		Expect(stored.ID).To(Equal(approved.Connection.ID))
		Expect(stored.UserID).To(Equal(jane.ID))
		Expect(stored.AccountID).To(Equal(jane.AccountID))

		status, body = poll(p.ReadKey)
		Expect(status).To(Equal(fiber.StatusGone))
		Expect(body.Code).To(Equal(handlers.CodePairingExpired))

		notes, err := repository.NewNotificationRepository(db).List(tenantCtx(), jane.ID, repository.NotificationListOpts{Limit: 10})
		Expect(err).NotTo(HaveOccurred())
		Expect(notes).To(HaveLen(1))
		Expect(notes[0].Type).To(Equal("integration.plugin_connected"))
		Expect(notes[0].EntityID).To(Equal(stored.ID))
	})

	It("binds the token to the active workspace of the approval request", func() {
		now := time.Now().UTC()
		_, err := db.NewInsert().Model(&models.Tenant{ID: "t-design", Name: "Design", Slug: "design", TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now}).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		member := &models.User{ID: "u-jane-design", AccountID: jane.AccountID, TenantID: "t-design", Name: "Jane", Email: jane.Email, Role: models.RoleMember}
		_, err = db.NewInsert().Model(member).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())

		p := start(nil)
		resp := send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.WriteKey+"/approve", nil, cookie, map[string]string{"X-Workspace-Id": "t-design"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))

		_, body := poll(p.ReadKey)
		Expect(body.Workspace).To(Equal(struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{ID: "t-design", Name: "Design"}))
		Expect(body.User.ID).To(Equal(member.ID))
	})

	It("reports a denied pairing to the plugin and refuses a later approve", func() {
		p := start(nil)
		resp := send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.WriteKey+"/deny", nil, cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusNoContent))

		status, body := poll(p.ReadKey)
		Expect(status).To(Equal(fiber.StatusForbidden))
		Expect(body.Code).To(Equal(handlers.CodePairingDenied))

		resp = send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.WriteKey+"/approve", nil, cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
		var rej map[string]string
		decode(resp, &rej)
		Expect(rej["code"]).To(Equal(handlers.CodePairingNotPending))
	})

	It("treats an expired pairing as gone everywhere", func() {
		p := start(nil)
		_, err := db.NewUpdate().TableExpr("plugin_pairings").Set("expires_at = ?", time.Now().Add(-time.Second)).Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())

		status, body := poll(p.ReadKey)
		Expect(status).To(Equal(fiber.StatusGone))
		Expect(body.Code).To(Equal(handlers.CodePairingExpired))
		for _, path := range []string{"", "/approve", "/deny"} {
			method := fiber.MethodPost
			if path == "" {
				method = fiber.MethodGet
			}
			resp := send(method, "/api/integrations/figma/pairings/"+p.WriteKey+path, nil, cookie, nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusGone), "path %q", path)
		}
	})

	It("doesn't hand out a token revoked before it was collected", func() {
		p := start(nil)
		resp := send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.WriteKey+"/approve", nil, cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		_, err := db.NewUpdate().TableExpr("plugin_tokens").Set("revoked_at = now()").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())

		status, _ := poll(p.ReadKey)
		Expect(status).To(Equal(fiber.StatusGone))
	})

	It("keeps the read key out of the web app's reach", func() {
		p := start(nil)
		resp := send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.ReadKey+"/approve", nil, cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusGone))
		status, _ := poll(p.WriteKey)
		Expect(status).To(Equal(fiber.StatusGone))
	})

	It("requires a signed-in user for the approval routes", func() {
		p := start(nil)
		Expect(send(fiber.MethodGet, "/api/integrations/figma/pairings/"+p.WriteKey, nil, nil, nil).StatusCode).To(Equal(fiber.StatusUnauthorized))
		Expect(send(fiber.MethodPost, "/api/integrations/figma/pairings/"+p.WriteKey+"/approve", nil, nil, nil).StatusCode).To(Equal(fiber.StatusUnauthorized))
	})

	DescribeTable("rejects a bad client label",
		func(label string) {
			resp := send(fiber.MethodPost, "/api/plugins/figma/pairings", fiber.Map{"client_label": label}, nil, nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
			var rej map[string]string
			decode(resp, &rej)
			Expect(rej["code"]).To(Equal(handlers.CodeInvalidLabel))
		},
		Entry("empty", "   "),
		Entry("too long", strings.Repeat("é", plugins.MaxClientLabelLen+1)),
	)

	It("rate-limits pairing starts per client and polls per read key", func() {
		for range 10 {
			start(nil)
		}
		resp := send(fiber.MethodPost, "/api/plugins/figma/pairings", fiber.Map{"client_label": "Figma · Jane"}, nil, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
		Expect(resp.Header.Get(fiber.HeaderRetryAfter)).NotTo(BeEmpty())

		// A fresh handler: its limiters are per instance.
		svc := newTestPluginService(db)
		app = fiber.New()
		handlers.NewFigmaPluginHandler(handlers.FigmaPluginDeps{Pairing: svc, AppBaseURL: testAppBaseURL, Tokens: tokens, Users: repository.NewUserRepository(db)}).Register(app)
		p := start(nil)
		for range 60 {
			status, _ := poll(p.ReadKey)
			Expect(status).To(Equal(fiber.StatusAccepted))
		}
		resp = send(fiber.MethodGet, "/api/plugins/figma/pairings/"+p.ReadKey, nil, nil, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusTooManyRequests))
	})
})
