//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// ztSharedKey is the single, app-wide Zernio API key every tenant authenticates
// with: the secret table is deliberately not tenant-scoped.
const ztSharedKey = "zk-shared-CON102-doNotLeak"

// ztSettingStore adapts repository.SettingRepository to zernio.SettingsStore,
// mirroring the server's adapter so the bootstrapper/handler write tenant-scoped
// zernio.* settings from the request's tenant context.
type ztSettingStore struct{ repo repository.SettingRepository }

func (s ztSettingStore) Get(ctx context.Context, key string) (string, bool, error) {
	row, err := s.repo.GetByKey(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return row.Value, true, nil
}
func (s ztSettingStore) Set(ctx context.Context, key, value string) error {
	return s.repo.Upsert(ctx, &models.Setting{Key: key, Value: value})
}
func (s ztSettingStore) Delete(ctx context.Context, key string) error {
	_, err := s.repo.Delete(ctx, key)
	return err
}

// ztZernioStub is a minimal multi-tenant Zernio stub. It mints a fresh profile
// id per created name, echoes connect links carrying the requested profileId,
// and records every Authorization header it saw plus every connect profileId —
// so a test can assert the shared bearer (FR11) and per-tenant profile scoping
// (FR12).
type ztZernioStub struct {
	mu          sync.Mutex
	authHeaders []string
	connectPIDs []string
	nextID      int
	profiles    []map[string]any // created profiles, served back by GET /profiles
}

func (s *ztZernioStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.authHeaders = append(s.authHeaders, r.Header.Get("Authorization"))
		s.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/profiles":
			// Serve the profiles created so far (a real Zernio would), so the
			// bootstrapper's adopt/re-list path is exercised — a non-distinct
			// profile name would otherwise let one tenant adopt another's profile
			// and this stub would hide it by always returning empty.
			s.mu.Lock()
			list := append([]map[string]any(nil), s.profiles...)
			s.mu.Unlock()
			ztWriteJSON(w, map[string]any{"profiles": list})
		case r.Method == http.MethodPost && r.URL.Path == "/profiles":
			body, _ := io.ReadAll(r.Body)
			var in struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(body, &in)
			now := time.Now().UTC().Format(time.RFC3339)
			s.mu.Lock()
			s.nextID++
			profile := map[string]any{
				"_id":       fmt.Sprintf("prof-%d", s.nextID),
				"name":      in.Name,
				"createdAt": now,
				"updatedAt": now,
			}
			s.profiles = append(s.profiles, profile)
			s.mu.Unlock()
			ztWriteJSON(w, map[string]any{"profile": profile})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/connect/"):
			pid := r.URL.Query().Get("profileId")
			s.mu.Lock()
			s.connectPIDs = append(s.connectPIDs, pid)
			s.mu.Unlock()
			ztWriteJSON(w, map[string]any{"authUrl": "https://zernio.test/oauth?profileId=" + pid})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (s *ztZernioStub) snapshot() (auths, connectPIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authHeaders...), append([]string(nil), s.connectPIDs...)
}

func (s *ztZernioStub) createdNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.profiles))
	for _, p := range s.profiles {
		if n, ok := p["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

func ztWriteJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

type ztRig struct {
	app        *fiber.App
	stub       *httptest.Server
	stubState  *ztZernioStub
	accounts   repository.SocialAccountRepository
	settings   zernio.SettingsStore
	cookieName string
}

func newZernioTenancyRig() *ztRig {
	db := mustOpenIntegrationDB()

	// One app-wide Zernio key seeded into the (unscoped) secret store.
	kek := make([]byte, envelope.KeySize)
	_, err := rand.Read(kek)
	Expect(err).NotTo(HaveOccurred())
	cipher, err := envelope.NewCipher(kek)
	Expect(err).NotTo(HaveOccurred())
	secretStore := secrets.NewStore(repository.NewSecretRepository(db), cipher)
	_, _, err = secretStore.Set(tenantCtx(), secrets.NameZernioAPIKey, ztSharedKey)
	Expect(err).NotTo(HaveOccurred())

	stubState := &ztZernioStub{}
	stub := httptest.NewServer(stubState.handler())

	// The production resolver: tenant-blind (reads the unscoped secret), so the
	// same bearer authenticates every tenant.
	resolver := func(ctx context.Context) (string, error) {
		return secretStore.Get(ctx, secrets.NameZernioAPIKey)
	}
	client := zernio.NewClient(resolver, stub.URL, zernio.ClientOpts{Timeout: 5 * time.Second})
	integ := zernio.NewIntegration(client)
	integ.SetState(zernio.StateOK) // simulate a successful boot warmup

	settingRepo := repository.NewSettingRepository(db)
	settings := ztSettingStore{repo: settingRepo}
	bootstrapper := zernio.NewBootstrapper(integ, settings, "dev")
	accountRepo := repository.NewSocialAccountRepository(db)
	platformRepo := repository.NewPlatformRepository(db)

	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		},
	})
	const cookieName = "c3_session_ztenancy"
	auth := handlers.RequireAuth(repository.NewSessionRepository(db), repository.NewUserRepository(db), cookieName)
	handlers.NewTenantsHandler(signup.New(db, repository.NewAccountRepository(db), repository.NewTenantRepository(db), nil), repository.NewTenantRepository(db), cookieName, false, auth).Register(app)
	handlers.NewZernioHandler(integ, bootstrapper, settings, platformRepo, accountRepo, repository.NewPostRepository(db), nil, zernio.NewConnectLinkRateLimiter(), auth, repository.NewZernioConnectSessionRepository(db), nil, "http://localhost").Register(app)

	return &ztRig{app: app, stub: stub, stubState: stubState, accounts: accountRepo, settings: settings, cookieName: cookieName}
}

func (r *ztRig) signup(name, email string) (*http.Cookie, string) {
	GinkgoHelper()
	body, _ := json.Marshal(fiber.Map{
		"tenant": fiber.Map{"name": name},
		"user":   fiber.Map{"name": name, "email": email, "password": "password-12345"},
	})
	req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.app.Test(req, -1)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
	var out map[string]any
	Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == r.cookieName {
			cookie = c
		}
	}
	Expect(cookie).NotTo(BeNil())
	return cookie, out["tenant"].(map[string]any)["id"].(string)
}

func (r *ztRig) connect(cookie *http.Cookie, platform string) int {
	GinkgoHelper()
	body, _ := json.Marshal(fiber.Map{"platform": platform})
	req := httptest.NewRequest("POST", "/api/integrations/zernio/connect-links", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := r.app.Test(req, -1)
	Expect(err).NotTo(HaveOccurred())
	return resp.StatusCode
}

func (r *ztRig) profileID(tenantID string) string {
	GinkgoHelper()
	id, ok, err := r.settings.Get(tenantctx.With(context.Background(), tenantID), zernio.SettingProfileID)
	Expect(err).NotTo(HaveOccurred())
	Expect(ok).To(BeTrue())
	return id
}

func (r *ztRig) seedAccount(tenantID, profileID, id, username string) {
	GinkgoHelper()
	now := time.Now().UTC()
	acct := models.SocialAccount{
		ID: id, Platform: "linkedin", ProfileID: profileID,
		Username: username, DisplayName: username, AvatarURL: "", IsActive: true,
		RawJSON: "{}", ConnectedAt: now, LastSyncedAt: now,
	}
	err := r.accounts.ApplyPlan(tenantctx.With(context.Background(), tenantID), []models.SocialAccount{acct}, nil, now)
	Expect(err).NotTo(HaveOccurred())
}

func (r *ztRig) accountUsernames(cookie *http.Cookie) []string {
	GinkgoHelper()
	req := httptest.NewRequest("GET", "/api/integrations/zernio/accounts", nil)
	req.AddCookie(cookie)
	resp, err := r.app.Test(req, -1)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(200))
	var out struct {
		Accounts []struct {
			Username string `json:"username"`
		} `json:"accounts"`
	}
	Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
	names := make([]string, 0, len(out.Accounts))
	for _, a := range out.Accounts {
		names = append(names, a.Username)
	}
	return names
}

var _ = Describe("Zernio per-tenant profiles on one shared key (CON-102)", func() {
	It("authenticates every tenant with the single shared key (FR11)", func() {
		rig := newZernioTenancyRig()
		defer rig.stub.Close()

		cookieA, _ := rig.signup("Acme", "a@acme.test")
		cookieB, _ := rig.signup("Beta", "b@beta.test")

		// Two different tenants each drive outbound Zernio calls (list+create
		// profile, then connect).
		Expect(rig.connect(cookieA, "linkedin")).To(Equal(200))
		Expect(rig.connect(cookieB, "linkedin")).To(Equal(200))

		auths, _ := rig.stubState.snapshot()
		Expect(auths).NotTo(BeEmpty(), "the stub must have received authenticated requests")
		// The secret table has no tenant_id, so the resolver is tenant-blind:
		// every request — regardless of caller tenant — carried the same bearer.
		for _, a := range auths {
			Expect(a).To(Equal("Bearer "+ztSharedKey),
				"every outbound Zernio request must use the one shared key")
		}
	})

	It("isolates each tenant under its own profile (FR12)", func() {
		rig := newZernioTenancyRig()
		defer rig.stub.Close()

		cookieA, tidA := rig.signup("Acme", "a@acme.test")
		cookieB, tidB := rig.signup("Beta", "b@beta.test")
		Expect(rig.connect(cookieA, "linkedin")).To(Equal(200))
		Expect(rig.connect(cookieB, "linkedin")).To(Equal(200))

		// Each tenant got its own distinct profile, stored under its own tenant.
		pidA := rig.profileID(tidA)
		pidB := rig.profileID(tidB)
		Expect(pidA).NotTo(BeEmpty())
		Expect(pidB).NotTo(BeEmpty())
		Expect(pidA).NotTo(Equal(pidB), "tenants must not share a Zernio profile")

		// Each profile carries its env-scoped, tenant-specific name, so the
		// adopt-by-name path (now exercised by the persisting stub) can't cross
		// tenants.
		Expect(rig.stubState.createdNames()).To(ConsistOf(
			zernio.ManagedProfileNameFor("dev", tidA),
			zernio.ManagedProfileNameFor("dev", tidB),
		))

		// The connect-links each carried the caller-tenant's profileId — nothing
		// crossed tenants.
		_, connectPIDs := rig.stubState.snapshot()
		Expect(connectPIDs).To(ConsistOf(pidA, pidB))

		// The local account mirror is scoped by tenant AND profile: each tenant
		// sees only its own connected account.
		rig.seedAccount(tidA, pidA, "acct-A", "acme_li")
		rig.seedAccount(tidB, pidB, "acct-B", "beta_li")
		Expect(rig.accountUsernames(cookieA)).To(ConsistOf("acme_li"))
		Expect(rig.accountUsernames(cookieB)).To(ConsistOf("beta_li"))
	})
})
