package handlers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// fakeLifecycleEnqueuer records the tenant ids the workspace handler asks to
// bootstrap (on create) and tear down (on delete) their Zernio profile. It lets
// the CON-203 tests assert the tx-coupling: a committed delete enqueues exactly
// one teardown, a rolled-back delete enqueues none. It ignores the *sql.Tx (the
// real enqueuer inserts a River job into it) and returns nil so the handler's
// transaction commits normally.
type fakeLifecycleEnqueuer struct {
	mu           sync.Mutex
	bootstrapped []string
	tornDown     []string
}

func (f *fakeLifecycleEnqueuer) EnqueueBootstrapProfileTx(_ context.Context, _ *sql.Tx, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootstrapped = append(f.bootstrapped, tenantID)
	return nil
}

func (f *fakeLifecycleEnqueuer) EnqueueTeardownProfileTx(_ context.Context, _ *sql.Tx, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tornDown = append(f.tornDown, tenantID)
	return nil
}

func (f *fakeLifecycleEnqueuer) teardowns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.tornDown...)
}

// Exercises the CON-147 PR2 workspace surface end-to-end: one account holding
// several workspaces, per-request workspace selection via the X-Workspace-Id
// header (the multi-tab mechanism), and the default-workspace switch. Tags stand
// in for any tenant-scoped entity — the point is which workspace a request reads
// and writes, decided by the header, not the session.
var _ = Describe("Workspaces (CON-147)", Ordered, func() {
	var (
		app *fiber.App
		db  *bun.DB
		enq *fakeLifecycleEnqueuer
	)

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

	BeforeEach(func() {
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		userRepo := repository.NewUserRepository(db)
		accountRepo := repository.NewAccountRepository(db)
		workspaceRepo := repository.NewWorkspaceRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tagRepo := repository.NewTagRepository(db)
		settingRepo := repository.NewSettingRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		// signup's own bootstrap enqueuer stays nil (creation still succeeds); the
		// workspace handler gets a recording fake so the CON-203 teardown enqueue
		// (and its tx-coupling) can be asserted.
		enq = &fakeLifecycleEnqueuer{}
		handlers.NewTenantsHandler(signup.New(db, accountRepo, tenantRepo, nil), tenantRepo, testCookieName, false, auth).Register(app)
		handlers.NewWorkspacesHandler(db, workspaceRepo, userRepo, accountRepo, tenantRepo, sessionRepo, enq, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, accountRepo, sessionRepo, testCookieName, false).Register(app)
		handlers.NewUsersHandler(db, userRepo, accountRepo, settingRepo, auth).Register(app)
		handlers.NewTagsHandler(tagRepo, auth).Register(app)
	})

	AfterEach(func() {
		ctx := tenantCtx()
		for _, t := range []string{"tags", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(t).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(ctx)
	})

	// signup creates a fresh account + first workspace, returning the auth cookie
	// and the created workspace (tenant) id.
	signup := func(tenantName, email string) (*http.Cookie, string) {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{
			"tenant": fiber.Map{"name": tenantName},
			"user":   fiber.Map{"name": "Owner", "email": email, "password": "password-owner"},
		})
		req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var out struct {
			Tenant models.Tenant `json:"tenant"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		var cookie *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				cookie = ck
			}
		}
		Expect(cookie).NotTo(BeNil(), "signup did not set a session cookie")
		return cookie, out.Tenant.ID
	}

	// do issues a request as cookie, optionally selecting a workspace via the
	// X-Workspace-Id header (empty = none, i.e. the session default).
	do := func(method, path string, cookie *http.Cookie, workspaceID string, payload fiber.Map) *http.Response {
		GinkgoHelper()
		var r *bytes.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			r = bytes.NewReader(b)
		} else {
			r = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
		if workspaceID != "" {
			req.Header.Set("X-Workspace-Id", workspaceID)
		}
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	// login authenticates an account and returns its session cookie.
	login := func(email, password string) *http.Cookie {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		return resp.Cookies()[0]
	}

	// seedMember inserts an account and a member (non-owner) membership of an
	// existing workspace, for the owner-gating tests.
	seedMember := func(tenantID, email, password string) {
		GinkgoHelper()
		hash, err := models.HashPassword(password)
		Expect(err).NotTo(HaveOccurred())
		id, err := models.NewID()
		Expect(err).NotTo(HaveOccurred())
		now := time.Now().UTC()
		bg := context.Background() // Account/User aren't tenant-scoped
		_, err = db.NewInsert().Model(&models.Account{ID: id, Email: email, PasswordHash: hash, Name: "Mem", CreatedAt: now, UpdatedAt: now}).Exec(bg)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewInsert().Model(&models.User{ID: id, AccountID: id, TenantID: tenantID, Name: "Mem", Email: email, Role: models.RoleMember, CreatedAt: now, UpdatedAt: now}).Exec(bg)
		Expect(err).NotTo(HaveOccurred())
	}

	// createWorkspace posts a new workspace for cookie and returns its id.
	createWorkspace := func(cookie *http.Cookie, name string) string {
		GinkgoHelper()
		resp := do("POST", "/api/workspaces", cookie, "", fiber.Map{"name": name})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var t models.Tenant
		Expect(json.NewDecoder(resp.Body).Decode(&t)).To(Succeed())
		return t.ID
	}

	createTag := func(cookie *http.Cookie, workspaceID, name string) *http.Response {
		GinkgoHelper()
		return do("POST", "/api/tags", cookie, workspaceID, fiber.Map{"name": name})
	}

	listTagNames := func(cookie *http.Cookie, workspaceID string) []string {
		GinkgoHelper()
		resp := do("GET", "/api/tags", cookie, workspaceID, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var tags []models.Tag
		Expect(json.NewDecoder(resp.Body).Decode(&tags)).To(Succeed())
		names := make([]string, 0, len(tags))
		for _, t := range tags {
			names = append(names, t.Name)
		}
		return names
	}

	Describe("GET /api/workspaces", func() {
		It("lists every workspace the account belongs to, marking the default", func() {
			cookie, w1 := signup("Alpha", "alpha@test.local")
			w2 := createWorkspace(cookie, "Beta")

			resp := do("GET", "/api/workspaces", cookie, "", nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
			var items []repository.WorkspaceListItem
			Expect(json.NewDecoder(resp.Body).Decode(&items)).To(Succeed())

			Expect(items).To(HaveLen(2))
			byID := map[string]repository.WorkspaceListItem{}
			for _, it := range items {
				byID[it.ID] = it
			}
			Expect(byID[w1].Role).To(Equal(models.RoleOwner))
			Expect(byID[w2].Role).To(Equal(models.RoleOwner))
			Expect(byID[w1].MemberCount).To(Equal(1))
			Expect(byID[w2].MemberCount).To(Equal(1))
			// The signup workspace is the default until a switch moves it.
			Expect(byID[w1].IsDefault).To(BeTrue())
			Expect(byID[w2].IsDefault).To(BeFalse())
		})
	})

	Describe("X-Workspace-Id resolution (the multi-tab mechanism)", func() {
		It("scopes a request to the header workspace, isolated from the default", func() {
			cookie, _ := signup("Alpha", "alpha@test.local")
			w2 := createWorkspace(cookie, "Beta")

			// Write into W2 by carrying its id in the header.
			Expect(createTag(cookie, w2, "beta-only").StatusCode).To(Equal(fiber.StatusCreated))

			// The same session reading W2 sees it; reading the default (no header)
			// does not — two tabs, two workspaces, one cookie.
			Expect(listTagNames(cookie, w2)).To(ContainElement("beta-only"))
			Expect(listTagNames(cookie, "")).NotTo(ContainElement("beta-only"))
		})

		It("rejects a workspace the account is not a member of with 403", func() {
			cookieA, _ := signup("Alpha", "alpha@test.local")
			_, foreign := signup("Foreign", "foreign@test.local") // a workspace A can't see

			resp := do("GET", "/api/tags", cookieA, foreign, nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusForbidden))
		})
	})

	Describe("POST /api/workspaces/:id/switch", func() {
		It("moves the default workspace without disturbing header-scoped reads", func() {
			cookie, w1 := signup("Alpha", "alpha@test.local")
			w2 := createWorkspace(cookie, "Beta")

			// A no-header request defaults to W1 before the switch.
			Expect(createTag(cookie, "", "in-w1").StatusCode).To(Equal(fiber.StatusCreated))
			Expect(listTagNames(cookie, w1)).To(ContainElement("in-w1"))

			resp := do("POST", "/api/workspaces/"+w2+"/switch", cookie, "", nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusNoContent))

			// Now a no-header request defaults to W2, and the list marks it default.
			Expect(createTag(cookie, "", "in-w2").StatusCode).To(Equal(fiber.StatusCreated))
			Expect(listTagNames(cookie, w2)).To(ContainElement("in-w2"))
			Expect(listTagNames(cookie, "")).To(ContainElement("in-w2"))
			Expect(listTagNames(cookie, "")).NotTo(ContainElement("in-w1"))

			lresp := do("GET", "/api/workspaces", cookie, "", nil)
			var items []repository.WorkspaceListItem
			Expect(json.NewDecoder(lresp.Body).Decode(&items)).To(Succeed())
			for _, it := range items {
				Expect(it.IsDefault).To(Equal(it.ID == w2))
			}
		})

		It("returns 404 for a workspace the account is not a member of", func() {
			cookieA, _ := signup("Alpha", "alpha@test.local")
			_, foreign := signup("Foreign", "foreign@test.local")

			resp := do("POST", "/api/workspaces/"+foreign+"/switch", cookieA, "", nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
		})
	})

	Describe("DELETE /api/workspaces/:id (soft-delete, CON-147 PR4)", func() {
		It("soft-deletes a workspace: it leaves the list and can't be entered", func() {
			cookie, _ := signup("Alpha", "alpha@test.local")
			w2 := createWorkspace(cookie, "Beta")
			// Prove it's reachable first.
			Expect(createTag(cookie, w2, "before-delete").StatusCode).To(Equal(fiber.StatusCreated))

			resp := do("DELETE", "/api/workspaces/"+w2, cookie, "", nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusNoContent))

			// Gone from the switcher.
			lresp := do("GET", "/api/workspaces", cookie, "", nil)
			var items []repository.WorkspaceListItem
			Expect(json.NewDecoder(lresp.Body).Decode(&items)).To(Succeed())
			ids := make([]string, 0, len(items))
			for _, it := range items {
				ids = append(ids, it.ID)
			}
			Expect(ids).NotTo(ContainElement(w2))
			// Can't be entered via the header, nor switched to.
			Expect(do("GET", "/api/tags", cookie, w2, nil).StatusCode).To(Equal(fiber.StatusForbidden))
			Expect(do("POST", "/api/workspaces/"+w2+"/switch", cookie, "", nil).StatusCode).To(Equal(fiber.StatusNotFound))
		})

		It("refuses to delete the account's only workspace (409) and rolls the soft-delete back", func() {
			cookie, w1 := signup("Solo", "solo@test.local")
			resp := do("DELETE", "/api/workspaces/"+w1, cookie, "", nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
			// The guard runs inside the tx, so the soft-delete is rolled back — the
			// workspace is still live and listed.
			lresp := do("GET", "/api/workspaces", cookie, "", nil)
			var items []repository.WorkspaceListItem
			Expect(json.NewDecoder(lresp.Body).Decode(&items)).To(Succeed())
			Expect(items).To(HaveLen(1))
			Expect(items[0].ID).To(Equal(w1))
		})

		It("enqueues a Zernio profile teardown for the deleted workspace (CON-203)", func() {
			cookie, _ := signup("Alpha", "alpha@test.local")
			w2 := createWorkspace(cookie, "Beta")

			Expect(do("DELETE", "/api/workspaces/"+w2, cookie, "", nil).StatusCode).To(Equal(fiber.StatusNoContent))
			// The teardown is enqueued in the same tx as the soft-delete.
			Expect(enq.teardowns()).To(ContainElement(w2))
		})

		It("enqueues no teardown when the delete rolls back (only workspace, 409) — CON-203", func() {
			cookie, w1 := signup("Solo", "solo@test.local")
			Expect(do("DELETE", "/api/workspaces/"+w1, cookie, "", nil).StatusCode).To(Equal(fiber.StatusConflict))
			// The last-workspace guard rolls the tx back, so the tx-coupled enqueue
			// never commits: no teardown job for a workspace that still exists.
			Expect(enq.teardowns()).NotTo(ContainElement(w1))
		})

		It("is owner-only: a member gets 403", func() {
			cookie, w1 := signup("Alpha", "alpha@test.local")
			_ = createWorkspace(cookie, "Beta") // so the owner isn't blocked by the only-workspace guard
			seedMember(w1, "mem@test.local", "mem-password")
			memCookie := login("mem@test.local", "mem-password")

			resp := do("DELETE", "/api/workspaces/"+w1, memCookie, "", nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusForbidden))
		})

		It("returns 404 deleting a workspace the account isn't a member of", func() {
			cookieA, _ := signup("Alpha", "alpha@test.local")
			_, foreign := signup("Foreign", "foreign@test.local")
			resp := do("DELETE", "/api/workspaces/"+foreign, cookieA, "", nil)
			Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
		})

		It("re-points the session default when the deleted workspace was it", func() {
			// W1 is the signup default; switch the default to W2, delete W2, and a
			// no-header request should land back on a surviving workspace (W1).
			cookie, w1 := signup("Alpha", "alpha@test.local")
			w2 := createWorkspace(cookie, "Beta")
			Expect(do("POST", "/api/workspaces/"+w2+"/switch", cookie, "", nil).StatusCode).To(Equal(fiber.StatusNoContent))

			Expect(do("DELETE", "/api/workspaces/"+w2, cookie, "", nil).StatusCode).To(Equal(fiber.StatusNoContent))

			// No-header request still works (default re-pointed to the survivor) and
			// the list marks W1 as the default again.
			lresp := do("GET", "/api/workspaces", cookie, "", nil)
			Expect(lresp.StatusCode).To(Equal(fiber.StatusOK))
			var items []repository.WorkspaceListItem
			Expect(json.NewDecoder(lresp.Body).Decode(&items)).To(Succeed())
			Expect(items).To(HaveLen(1))
			Expect(items[0].ID).To(Equal(w1))
			Expect(items[0].IsDefault).To(BeTrue())
		})
	})

	Describe("profile update propagation (CON-147)", func() {
		It("renaming propagates to the account's membership in every workspace", func() {
			// One account, two workspaces => two membership rows carrying the
			// denormalised name. A profile rename must update both, not just the one
			// the request is scoped to.
			cookie, w1 := signup("Alpha", "alpha@test.local")
			w2 := createWorkspace(cookie, "Beta")

			// The caller's membership id in the active (default) workspace.
			cu := do("GET", "/api/current_user", cookie, "", nil)
			Expect(cu.StatusCode).To(Equal(fiber.StatusOK))
			var me struct {
				ID string `json:"id"`
			}
			Expect(json.NewDecoder(cu.Body).Decode(&me)).To(Succeed())

			resp := do("PUT", "/api/users/"+me.ID, cookie, "", fiber.Map{"name": "Renamed", "email": "alpha@test.local"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusOK))

			var names []string
			Expect(db.NewSelect().ColumnExpr("name").TableExpr("users").
				Where("tenant_id IN (?, ?)", w1, w2).
				Scan(context.Background(), &names)).To(Succeed())
			Expect(names).To(ConsistOf("Renamed", "Renamed"))
		})
	})
})
