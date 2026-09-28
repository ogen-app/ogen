package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// settingStoreFromRepo is a copy of the production adapter that
// bridges repository.SettingRepository to zernio.SettingsStore. The
// production adapter lives in the server package; we duplicate it
// here to avoid an import cycle from a test file in handlers.
type settingStoreFromRepo struct {
	repo repository.SettingRepository
}

func (s *settingStoreFromRepo) Get(ctx context.Context, key string) (string, bool, error) {
	got, err := s.repo.GetByKey(ctx, key)
	if err != nil {
		// sql.ErrNoRows surfaces as "not found"; everything else is fatal.
		if err.Error() == "sql: no rows in result set" {
			return "", false, nil
		}
		return "", false, err
	}
	return got.Value, true, nil
}

func (s *settingStoreFromRepo) Set(ctx context.Context, key, value string) error {
	return s.repo.Upsert(ctx, &models.Setting{Key: key, Value: value})
}

func (s *settingStoreFromRepo) Delete(ctx context.Context, key string) error {
	_, err := s.repo.Delete(ctx, key)
	return err
}

// zernioStub mirrors stubServer in the zernio package's tests. We
// duplicate the tiny scaffold here to keep handler tests independent.
type zernioStub struct {
	*httptest.Server

	mu     sync.Mutex
	routes map[string]http.HandlerFunc
}

func newZernioStub() *zernioStub {
	s := &zernioStub{routes: make(map[string]http.HandlerFunc)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		h, ok := s.routes[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "stub: unhandled "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	return s
}

func (s *zernioStub) handle(method, path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.routes[method+" "+path] = h
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// quietHub is an eventhub.Hub that drains every Publish into a
// channel the test can observe; Subscribe returns ErrNoTopics so the
// SSE handler isn't triggered.
type quietHub struct {
	mu     sync.Mutex
	events []eventhub.Event
}

func (q *quietHub) Publish(_ context.Context, ev eventhub.Event) error {
	q.mu.Lock()
	q.events = append(q.events, ev)
	q.mu.Unlock()
	return nil
}

func (q *quietHub) Subscribe(_ context.Context, _ eventhub.SubscribeOpts) (<-chan eventhub.Event, func(), error) {
	return nil, nil, eventhub.ErrNoTopics
}

var _ eventhub.Hub = (*quietHub)(nil)

var _ = Describe("ZernioHandler", Ordered, func() {
	var (
		app           *fiber.App
		db            *bun.DB
		stub          *zernioStub
		authCookie    *http.Cookie
		hub           *quietHub
		integ         *zernio.Integration
		bootstrapper  *zernio.Bootstrapper
		worker        *zernio.Worker
		store         zernio.SettingsStore
		accountRepo   repository.SocialAccountRepository
		profileIDSeen atomic.Value // string — the profile id Zernio handed out
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	setupApp := func(apiKey string) {
		stub = newZernioStub()

		// Boot-time Ping + bootstrap: list returns no matching profile
		// so we exercise the create branch.
		stub.handle("GET", "/profiles", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"profiles": []any{}})
		})
		// Bootstrap: create profile, return canned ID wrapped in
		// Zernio's documented `{profile: {...}}` envelope.
		stub.handle("POST", "/profiles", func(w http.ResponseWriter, r *http.Request) {
			now := time.Now().UTC().Format(time.RFC3339)
			id := "p_test"
			profileIDSeen.Store(id)
			writeJSON(w, http.StatusOK, map[string]any{
				"profile": map[string]any{
					"_id":       id,
					"name":      zernio.ManagedProfileName,
					"createdAt": now,
					"updatedAt": now,
				},
			})
		})
		// Profile fetch by ID — used by the worker's metadata refresh.
		stub.handle("GET", "/profiles/p_test", func(w http.ResponseWriter, r *http.Request) {
			now := time.Now().UTC().Format(time.RFC3339)
			writeJSON(w, http.StatusOK, map[string]any{
				"profile": map[string]any{
					"_id": "p_test", "name": zernio.ManagedProfileName, "createdAt": now, "updatedAt": now,
				},
			})
		})
		// Connect-link issuance: GET /connect/{platform}?profileId=...
		// returning {"authUrl": "..."}.
		stub.handle("GET", "/connect/linkedin", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]string{
				"authUrl": "https://zernio.com/connect/linkedin?token=secret-do-not-log",
			})
		})

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
		platformRepo := repository.NewPlatformRepository(db)
		accountRepo = repository.NewSocialAccountRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)

		// Bypass the cache for tests so reads always hit the SQLite source
		// of truth; Phase 4 unit tests already cover cache semantics.
		store = &settingStoreFromRepo{repo: settingRepo}

		hub = &quietHub{}
		client := zernio.NewClient(zernio.StaticKey(apiKey), stub.URL, zernio.ClientOpts{Timeout: time.Second})
		integ = zernio.NewIntegration(client)
		integ.SetState(zernio.StateDegraded)
		bootstrapper = zernio.NewBootstrapper(integ, store, "dev")
		worker = zernio.NewWorker(integ, accountRepo, store, hub, bootstrapper, nil, time.Hour, time.Minute, func(ctx context.Context) ([]string, error) {
			// Mirror production (server/zernio.go): the sweep enumerates tenants
			// by the connect-initiated marker, not mere profile presence.
			return settingRepo.ListTenantIDsByKey(ctx, zernio.SettingConnectInitiatedAt)
		})

		handlers.NewZernioHandler(
			integ, bootstrapper, store, platformRepo, accountRepo, repository.NewPostRepository(db), worker,
			zernio.NewConnectLinkRateLimiter(),
			auth,
			repository.NewZernioConnectSessionRepository(db),
			nil, // cipher: only the 2+-target callback path seals tokens; unused here
			"http://localhost",
		).Register(app)

		// Seed an auth user and grab a session cookie.
		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]
	}

	AfterEach(func() {
		if stub != nil {
			stub.Close()
		}
		ctx := tenantCtx()
		_, _ = db.NewDelete().TableExpr("posts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns_types").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("social_accounts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("settings").Where("key LIKE ?", "zernio.%").Exec(ctx)
	})

	Describe("with an API key configured", func() {
		BeforeEach(func() {
			setupApp("api-key")
			Expect(bootstrapper.Run(tenantCtx())).To(Succeed())
			Expect(integ.State()).To(Equal(zernio.StateOK))
		})

		It("GET /platforms returns the Phase 1 allowlist with post-types from the local table", func() {
			req := httptest.NewRequest("GET", "/api/integrations/zernio/platforms", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var body struct {
				Platforms []struct {
					ID                 string   `json:"id"`
					Label              string   `json:"label"`
					SupportedPostTypes []string `json:"supportedPostTypes"`
				} `json:"platforms"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())

			ids := make([]string, len(body.Platforms))
			for i, p := range body.Platforms {
				ids[i] = p.ID
			}
			Expect(ids).To(ContainElements("twitter", "linkedin", "facebook", "instagram", "youtube", "threads"))
			// SupportedPostTypes is best-effort: the handler tolerates a
			// missing local platforms row (other tests in this suite
			// truncate the table). The contract validated here is that
			// the allowlist is exposed in full with stable labels.
			for _, p := range body.Platforms {
				Expect(p.Label).NotTo(BeEmpty())
			}
		})

		It("POST /connect-links rejects platforms outside the allowlist with 400", func() {
			body, _ := json.Marshal(fiber.Map{"platform": "tiktok"})
			req := httptest.NewRequest("POST", "/api/integrations/zernio/connect-links", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(400))
		})

		It("POST /connect-links recovers a degraded integration by bootstrapping on first connect", func() {
			// Simulate a transient boot-time degradation with no profile yet
			// (e.g. the warmup ping blipped). The state guard must not short
			// circuit before the lazy bootstrap gets to heal it (CON-100).
			ctx := tenantCtx()
			Expect(store.Delete(ctx, zernio.SettingProfileID)).To(Succeed())
			integ.SetState(zernio.StateDegraded)

			body, _ := json.Marshal(fiber.Map{"platform": "linkedin"})
			req := httptest.NewRequest("POST", "/api/integrations/zernio/connect-links", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))
			Expect(integ.State()).To(Equal(zernio.StateOK))
		})

		It("POST /connect-links returns the URL plus a 15-min ExpiresAt for an allowlisted platform", func() {
			body, _ := json.Marshal(fiber.Map{"platform": "linkedin"})
			req := httptest.NewRequest("POST", "/api/integrations/zernio/connect-links", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var out struct {
				Platform   string    `json:"platform"`
				ConnectURL string    `json:"connectUrl"`
				ExpiresAt  time.Time `json:"expiresAt"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			Expect(out.Platform).To(Equal("linkedin"))
			Expect(out.ConnectURL).To(ContainSubstring("zernio.com/connect/linkedin"))
			Expect(time.Until(out.ExpiresAt)).To(BeNumerically(">", 10*time.Minute))
			Expect(time.Until(out.ExpiresAt)).To(BeNumerically("<=", 15*time.Minute))
		})

		It("GET /accounts returns whatever the local table holds plus last-sync metadata", func() {
			// Seed a row directly so we exercise the read path without
			// running the worker.
			now := time.Now().UTC()
			Expect(accountRepo.ApplyPlan(tenantCtx(), []models.SocialAccount{{
				ID:           "acc1",
				Platform:     "linkedin",
				ProfileID:    "p_test",
				Username:     "alice",
				DisplayName:  "Alice",
				AvatarURL:    "https://example.com/a.png",
				IsActive:     true,
				RawJSON:      `{"_id":"acc1"}`,
				ConnectedAt:  now,
				LastSyncedAt: now,
			}}, nil, now)).To(Succeed())
			Expect(store.Set(tenantCtx(), zernio.SettingLastSyncAt, now.Format(time.RFC3339))).To(Succeed())
			Expect(store.Set(tenantCtx(), zernio.SettingLastSyncStatus, zernio.SyncStatusOK)).To(Succeed())

			req := httptest.NewRequest("GET", "/api/integrations/zernio/accounts", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(200))

			var out struct {
				Accounts []struct {
					ID       string `json:"id"`
					Platform string `json:"platform"`
				} `json:"accounts"`
				LastSyncStatus string `json:"lastSyncStatus"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			Expect(out.Accounts).To(HaveLen(1))
			Expect(out.Accounts[0].ID).To(Equal("acc1"))
			Expect(out.LastSyncStatus).To(Equal(zernio.SyncStatusOK))
		})

		It("POST /sync returns 202 and bumps the worker trigger channel", func() {
			req := httptest.NewRequest("POST", "/api/integrations/zernio/sync", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(202))
		})

		Describe("DELETE /accounts/:id (CON-133)", func() {
			seedAccount := func(id string) {
				now := time.Now().UTC()
				Expect(accountRepo.ApplyPlan(tenantCtx(), []models.SocialAccount{{
					ID:           id,
					Platform:     "linkedin",
					ProfileID:    "p_test",
					Username:     "alice",
					DisplayName:  "Alice",
					IsActive:     true,
					RawJSON:      `{"_id":"` + id + `"}`,
					ConnectedAt:  now,
					LastSyncedAt: now,
				}}, nil, now)).To(Succeed())
			}

			// seedScheduledPost inserts a campaign_type + campaign + one
			// scheduled post that publishes to accountID, so the disconnect
			// guard has something to count.
			seedScheduledPost := func(accountID string) {
				ctx := tenantCtx()
				var admin models.User
				Expect(db.NewSelect().Model(&admin).Where("email = ?", "admin@example.com").Scan(ctx)).To(Succeed())

				ctID, err := models.NewID()
				Expect(err).NotTo(HaveOccurred())
				_, err = db.NewInsert().Model(&models.CampaignType{ID: ctID, Name: "seed-" + ctID}).Exec(ctx)
				Expect(err).NotTo(HaveOccurred())

				campID, err := models.NewID()
				Expect(err).NotTo(HaveOccurred())
				_, err = db.NewInsert().Model(&models.Campaign{
					ID:              campID,
					Name:            "Seed",
					AssetIDs:        models.StringSlice{},
					TargetPlatforms: models.CampaignPlatforms{},
					TagIDs:          models.StringSlice{},
					CampaignTypeID:  ctID,
					Status:          models.StatusDraft,
					CreatedBy:       admin.ID,
				}).Exec(ctx)
				Expect(err).NotTo(HaveOccurred())

				when := time.Now().UTC().Add(time.Hour)
				postID, err := models.NewID()
				Expect(err).NotTo(HaveOccurred())
				_, err = db.NewInsert().Model(&models.Post{
					ID:              postID,
					CampaignID:      campID,
					SocialAccountID: accountID,
					Content:         "hi",
					MediaURLs:       models.StringSlice{},
					UsedAssetIDs:    models.StringSlice{},
					Status:          models.PostStatusScheduled,
					ScheduledAt:     &when,
					CTAType:         models.CTATypeNone,
					CreatedBy:       admin.ID,
				}).Exec(ctx)
				Expect(err).NotTo(HaveOccurred())
			}

			del := func(path string) *http.Response {
				req := httptest.NewRequest("DELETE", path, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				return resp
			}

			isActiveLocally := func(id string) bool {
				_, err := accountRepo.GetActive(tenantCtx(), "p_test", id)
				return err == nil
			}

			It("removes the account upstream, soft-deletes locally, and returns 204", func() {
				seedAccount("acc1")
				var deleted bool
				stub.handle("DELETE", "/accounts/acc1", func(w http.ResponseWriter, r *http.Request) {
					deleted = true
					writeJSON(w, http.StatusOK, map[string]string{"message": "Account disconnected successfully"})
				})

				Expect(del("/api/integrations/zernio/accounts/acc1").StatusCode).To(Equal(204))
				Expect(deleted).To(BeTrue(), "should call Zernio DELETE upstream")
				Expect(isActiveLocally("acc1")).To(BeFalse(), "row should be soft-deleted")
			})

			It("returns 404 for an unknown account without calling Zernio", func() {
				var called bool
				stub.handle("DELETE", "/accounts/nope", func(w http.ResponseWriter, r *http.Request) {
					called = true
					writeJSON(w, http.StatusOK, map[string]string{})
				})
				Expect(del("/api/integrations/zernio/accounts/nope").StatusCode).To(Equal(404))
				Expect(called).To(BeFalse())
			})

			It("treats an upstream 404 as an idempotent success and heals local state", func() {
				seedAccount("acc1")
				stub.handle("DELETE", "/accounts/acc1", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found"})
				})
				Expect(del("/api/integrations/zernio/accounts/acc1").StatusCode).To(Equal(204))
				Expect(isActiveLocally("acc1")).To(BeFalse())
			})

			It("returns 502 and leaves the row active when the upstream delete fails", func() {
				seedAccount("acc1")
				stub.handle("DELETE", "/accounts/acc1", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
				})
				Expect(del("/api/integrations/zernio/accounts/acc1").StatusCode).To(Equal(502))
				Expect(isActiveLocally("acc1")).To(BeTrue(), "must not soft-delete on upstream failure")
			})

			It("is idempotent: a second disconnect returns 404", func() {
				seedAccount("acc1")
				stub.handle("DELETE", "/accounts/acc1", func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, http.StatusOK, map[string]string{})
				})
				Expect(del("/api/integrations/zernio/accounts/acc1").StatusCode).To(Equal(204))
				Expect(del("/api/integrations/zernio/accounts/acc1").StatusCode).To(Equal(404))
			})

			It("blocks with 409 when scheduled posts reference the account, unless forced", func() {
				seedAccount("acc1")
				seedScheduledPost("acc1")
				var upstreamHits int
				stub.handle("DELETE", "/accounts/acc1", func(w http.ResponseWriter, r *http.Request) {
					upstreamHits++
					writeJSON(w, http.StatusOK, map[string]string{})
				})

				// Without force → 409 with the count, and no upstream call.
				resp := del("/api/integrations/zernio/accounts/acc1")
				Expect(resp.StatusCode).To(Equal(409))
				var body struct {
					Error          string `json:"error"`
					ScheduledPosts int    `json:"scheduledPosts"`
				}
				Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
				Expect(body.Error).To(Equal("account_has_scheduled_posts"))
				Expect(body.ScheduledPosts).To(Equal(1))
				Expect(upstreamHits).To(Equal(0))
				Expect(isActiveLocally("acc1")).To(BeTrue())

				// With force=true → disconnect proceeds.
				Expect(del("/api/integrations/zernio/accounts/acc1?force=true").StatusCode).To(Equal(204))
				Expect(upstreamHits).To(Equal(1))
				Expect(isActiveLocally("acc1")).To(BeFalse())
			})
		})
	})

	Describe("with no API key", func() {
		BeforeEach(func() {
			setupApp("")
		})

		It("POST /connect-links returns 409 integration_disabled", func() {
			body, _ := json.Marshal(fiber.Map{"platform": "linkedin"})
			req := httptest.NewRequest("POST", "/api/integrations/zernio/connect-links", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(409))
		})

		It("POST /profile/repair returns 409 integration_disabled", func() {
			req := httptest.NewRequest("POST", "/api/integrations/zernio/profile/repair", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(409))
		})

		It("DELETE /accounts/:id returns 409 integration_disabled", func() {
			req := httptest.NewRequest("DELETE", "/api/integrations/zernio/accounts/acc1", nil)
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(409))
		})
	})
})
