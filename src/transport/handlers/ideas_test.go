package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// ideaWire decodes an Idea exactly as the UI sees it, keeping nulls visible.
type ideaWire struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Note          string     `json:"note"`
	CampaignID    *string    `json:"campaign_id"`
	Verdict       *string    `json:"verdict"`
	RemindAt      *time.Time `json:"remind_at"`
	DecidedAt     *time.Time `json:"decided_at"`
	DecidedBy     *string    `json:"decided_by"`
	CreatedBy     *string    `json:"created_by"`
	CreatedByName string     `json:"created_by_name"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	FitScore      *int       `json:"fit_score"`
}

var _ = Describe("IdeasHandler (CON-315)", Ordered, func() {
	var (
		app       *fiber.App
		db        *bun.DB
		adminUser *models.User
		bobUser   *models.User
		admin     *http.Cookie
		bob       *http.Cookie
		ideaHub   *quietHub
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

	login := func(email, password string) *http.Cookie {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(resp.Cookies()).To(HaveLen(1))
		return resp.Cookies()[0]
	}

	BeforeEach(func() {
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		userRepo := repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		accountRepo := repository.NewAccountRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		tagRepo := repository.NewTagRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, accountRepo, sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewTenantsHandler(signup.New(db, accountRepo, tenantRepo), tenantRepo, testCookieName, false, auth, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		ideaHub = &quietHub{}
		handlers.NewIdeasHandler(ideas.New(repository.NewIdeaRepository(db), userRepo), auth, nil).WithEventHub(ideaHub).Register(app)

		adminUser = seedTenantUser(db, "Admin", "admin@example.com", "admin-password")
		bobUser = seedTenantUser(db, "Bob", "bob@example.com", "bob-password")
		admin = login("admin@example.com", "admin-password")
		bob = login("bob@example.com", "bob-password")
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"ideas", "campaigns", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(tenantCtx())
	})

	// ── request helpers ──────────────────────────────────────────────────────

	doRaw := func(cookie *http.Cookie, method, path, body string) *http.Response {
		GinkgoHelper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	do := func(cookie *http.Cookie, method, path string, body any) *http.Response {
		GinkgoHelper()
		b := ""
		if body != nil {
			raw, _ := json.Marshal(body)
			b = string(raw)
		}
		return doRaw(cookie, method, path, b)
	}

	decodeIdea := func(resp *http.Response, status int) ideaWire {
		GinkgoHelper()
		Expect(resp.StatusCode).To(Equal(status))
		var i ideaWire
		Expect(json.NewDecoder(resp.Body).Decode(&i)).To(Succeed())
		return i
	}

	capture := func(cookie *http.Cookie, body fiber.Map) ideaWire {
		GinkgoHelper()
		return decodeIdea(do(cookie, "POST", "/api/ideas", body), fiber.StatusCreated)
	}

	list := func(cookie *http.Cookie, query string) []ideaWire {
		GinkgoHelper()
		resp := do(cookie, "GET", "/api/ideas"+query, nil)
		Expect(resp.StatusCode).To(Equal(200))
		var body struct {
			Ideas []ideaWire `json:"ideas"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		return body.Ideas
	}

	ids := func(list []ideaWire) []string {
		out := make([]string, len(list))
		for i, idea := range list {
			out[i] = idea.ID
		}
		return out
	}

	createCampaign := func(cookie *http.Cookie, name string) string {
		GinkgoHelper()
		resp := do(cookie, "POST", "/api/campaigns", fiber.Map{"name": name, "campaign_type_id": "Uk"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var c models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
		return c.ID
	}

	softDeleteCampaign := func(id string) {
		GinkgoHelper()
		_, err := db.NewUpdate().TableExpr("campaigns").Set("deleted_at = now()").Where("id = ?", id).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	}

	signupTenant := func(name, email string) *http.Cookie {
		GinkgoHelper()
		resp := do(nil, "POST", "/api/tenants", fiber.Map{
			"tenant": fiber.Map{"name": name},
			"user":   fiber.Map{"name": "Owner", "email": email, "password": "password-owner"},
		})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				return ck
			}
		}
		Fail("signup did not set a session cookie")
		return nil
	}

	inAWeek := func() string { return time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339) }

	// ── GET ──────────────────────────────────────────────────────────────────

	It("requires auth", func() {
		Expect(do(nil, "GET", "/api/ideas", nil).StatusCode).To(Equal(401))
	})

	It("returns a wrapped, empty (not null) list for a fresh workspace", func() {
		resp := do(admin, "GET", "/api/ideas", nil)
		Expect(resp.StatusCode).To(Equal(200))
		raw := map[string]json.RawMessage{}
		Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
		Expect(string(raw["ideas"])).To(Equal("[]"))
	})

	// ── POST ─────────────────────────────────────────────────────────────────

	It("captures a title-only idea as undecided, workspace-wide, authored by the session user", func() {
		idea := capture(admin, fiber.Map{"title": "  Teardown of our onboarding  ", "created_by": bobUser.ID})
		Expect(idea.ID).NotTo(BeEmpty())
		Expect(idea.Title).To(Equal("Teardown of our onboarding"))
		Expect(idea.Note).To(Equal(""))
		Expect(idea.CampaignID).To(BeNil())
		Expect(idea.Verdict).To(BeNil())
		Expect(idea.DecidedAt).To(BeNil())
		Expect(idea.RemindAt).To(BeNil())
		Expect(*idea.CreatedBy).To(Equal(adminUser.ID)) // body created_by ignored
		Expect(idea.CreatedByName).To(Equal("Admin"))

		Expect(list(bob, "")).To(HaveLen(1)) // teammates see it
	})

	It("rejects a blank or oversized title", func() {
		Expect(do(admin, "POST", "/api/ideas", fiber.Map{"title": "   "}).StatusCode).To(Equal(400))
		Expect(do(admin, "POST", "/api/ideas", fiber.Map{"note": "no title"}).StatusCode).To(Equal(400))
		Expect(do(admin, "POST", "/api/ideas", fiber.Map{"title": strings.Repeat("a", 501)}).StatusCode).To(Equal(400))
		Expect(do(admin, "POST", "/api/ideas", fiber.Map{"title": "x", "note": strings.Repeat("a", 10001)}).StatusCode).To(Equal(400))
	})

	It("rejects an unknown, foreign, or soft-deleted campaign", func() {
		Expect(do(admin, "POST", "/api/ideas", fiber.Map{"title": "x", "campaign_id": "nope"}).StatusCode).To(Equal(400))

		other := signupTenant("Other", "owner@other.example")
		foreign := createCampaign(other, "Theirs")
		Expect(do(admin, "POST", "/api/ideas", fiber.Map{"title": "x", "campaign_id": foreign}).StatusCode).To(Equal(400))

		gone := createCampaign(admin, "Gone")
		softDeleteCampaign(gone)
		resp := do(admin, "POST", "/api/ideas", fiber.Map{"title": "x", "campaign_id": gone})
		Expect(resp.StatusCode).To(Equal(400))
		var body map[string]string
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body["error"]).To(Equal("campaign not found"))

		idea := capture(admin, fiber.Map{"title": "x"})
		Expect(do(admin, "PATCH", "/api/ideas/"+idea.ID, fiber.Map{"campaign_id": foreign}).StatusCode).To(Equal(400))
		Expect(do(admin, "PATCH", "/api/ideas/"+idea.ID, fiber.Map{"campaign_id": gone}).StatusCode).To(Equal(400))
	})

	// ── Workspace-wide vs campaign-attached ─────────────────────────────────

	It("filters by campaign, keeps workspace-wide ideas separate, and orders oldest first", func() {
		camp := createCampaign(admin, "Launch")
		first := capture(admin, fiber.Map{"title": "one"})
		second := capture(admin, fiber.Map{"title": "two", "campaign_id": camp})
		third := capture(bob, fiber.Map{"title": "three"})

		Expect(ids(list(admin, ""))).To(Equal([]string{first.ID, second.ID, third.ID}))
		Expect(ids(list(admin, "?campaign_id="+camp))).To(Equal([]string{second.ID}))
		Expect(ids(list(admin, "?campaign_id=none"))).To(Equal([]string{first.ID, third.ID}))
		Expect(list(admin, "?campaign_id=unknown")).To(BeEmpty())
	})

	It("sorts by fit score, unscored last, and reads the fit fields back as null when unset", func() {
		first := capture(admin, fiber.Map{"title": "unscored"})
		second := capture(admin, fiber.Map{"title": "middling"})
		third := capture(admin, fiber.Map{"title": "best"})
		for id, score := range map[string]int{second.ID: 55, third.ID: 90} {
			_, err := db.NewUpdate().TableExpr("ideas").
				Set("fit_score = ?, fit_rubric_version = 1, fit_breakdown = ?", score, `{"lens":"workspace","campaign_id":null,"criteria":{},"caps":[]}`).
				Where("id = ?", id).Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}

		Expect(ids(list(admin, "?sort=fit"))).To(Equal([]string{third.ID, second.ID, first.ID}))
		Expect(ids(list(admin, "?sort=created"))).To(Equal([]string{first.ID, second.ID, third.ID}))
		Expect(ids(list(admin, ""))).To(Equal([]string{first.ID, second.ID, third.ID}))
		Expect(do(admin, "GET", "/api/ideas?sort=score", nil).StatusCode).To(Equal(400))

		resp := do(admin, "GET", "/api/ideas?sort=fit", nil)
		var body struct {
			Ideas []map[string]json.RawMessage `json:"ideas"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(string(body.Ideas[0]["fit_score"])).To(Equal("90"))
		Expect(string(body.Ideas[0]["fit_breakdown"])).To(ContainSubstring(`"lens":"workspace"`))
		for _, key := range []string{"fit_score", "fit_breakdown", "fit_rubric_version"} {
			Expect(body.Ideas[2]).To(HaveKeyWithValue(key, json.RawMessage("null")), key)
		}
	})

	It("attaches, re-attaches, and detaches without losing id, verdict, or author", func() {
		a := createCampaign(admin, "A")
		b := createCampaign(admin, "B")
		idea := capture(bob, fiber.Map{"title": "movable"})
		decided := decodeIdea(do(admin, "PUT", "/api/ideas/"+idea.ID+"/verdict", fiber.Map{"verdict": "yes", "remind_at": nil}), 200)

		for _, target := range []any{a, b, nil} {
			moved := decodeIdea(do(admin, "PATCH", "/api/ideas/"+idea.ID, fiber.Map{"campaign_id": target}), 200)
			Expect(moved.ID).To(Equal(idea.ID))
			Expect(*moved.Verdict).To(Equal("yes"))
			Expect(moved.DecidedAt.Equal(*decided.DecidedAt)).To(BeTrue())
			Expect(*moved.DecidedBy).To(Equal(adminUser.ID))
			Expect(*moved.CreatedBy).To(Equal(bobUser.ID))
			Expect(moved.CreatedByName).To(Equal("Bob"))
			if target == nil {
				Expect(moved.CampaignID).To(BeNil())
			} else {
				Expect(*moved.CampaignID).To(Equal(target))
			}
		}
		Expect(list(admin, "?campaign_id="+a)).To(BeEmpty())
		Expect(list(admin, "?campaign_id="+b)).To(BeEmpty())
		Expect(ids(list(admin, "?campaign_id=none"))).To(Equal([]string{idea.ID}))
	})

	It("allows an archived campaign and reads a soft-deleted one back as unfiled", func() {
		camp := createCampaign(admin, "Paused")
		_, err := db.NewUpdate().TableExpr("campaigns").Set("status = ?", models.StatusArchived).Where("id = ?", camp).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		idea := capture(admin, fiber.Map{"title": "for later", "campaign_id": camp})
		Expect(*idea.CampaignID).To(Equal(camp))

		softDeleteCampaign(camp)
		got := list(admin, "")
		Expect(got).To(HaveLen(1))
		Expect(got[0].CampaignID).To(BeNil())
		Expect(list(admin, "?campaign_id="+camp)).To(BeEmpty())
		Expect(ids(list(admin, "?campaign_id=none"))).To(Equal([]string{idea.ID}))
	})

	// ── PATCH ────────────────────────────────────────────────────────────────

	It("is presence-aware: one field changes, the others stay", func() {
		camp := createCampaign(admin, "C")
		idea := capture(admin, fiber.Map{"title": "title", "note": "note", "campaign_id": camp})

		got := decodeIdea(do(admin, "PATCH", "/api/ideas/"+idea.ID, fiber.Map{"title": " new title "}), 200)
		Expect(got.Title).To(Equal("new title"))
		Expect(got.Note).To(Equal("note"))
		Expect(*got.CampaignID).To(Equal(camp))
		Expect(got.UpdatedAt).To(BeTemporally(">=", idea.UpdatedAt))

		got = decodeIdea(do(bob, "PATCH", "/api/ideas/"+idea.ID, fiber.Map{"note": ""}), 200)
		Expect(got.Title).To(Equal("new title"))
		Expect(got.Note).To(Equal(""))
		Expect(*got.CampaignID).To(Equal(camp))
		Expect(*got.CreatedBy).To(Equal(adminUser.ID))
	})

	It("rejects an empty body, reserved fields, and null title", func() {
		idea := capture(admin, fiber.Map{"title": "x"})
		path := "/api/ideas/" + idea.ID
		for _, body := range []string{
			`{}`,
			`{"verdict":"yes"}`,
			`{"title":"y","remind_at":null}`,
			`{"created_by":"someone"}`,
			`{"decided_at":null}`,
			`{"title":null}`,
			`{"title":"   "}`,
			`[]`,
			`not json`,
		} {
			Expect(doRaw(admin, "PATCH", path, body).StatusCode).To(Equal(400), body)
		}
		got := list(admin, "")[0]
		Expect(got.Title).To(Equal("x"))
		Expect(*got.CreatedBy).To(Equal(adminUser.ID))
	})

	// ── PUT verdict ──────────────────────────────────────────────────────────

	It("enforces the verdict invariants", func() {
		idea := capture(admin, fiber.Map{"title": "triage me"})
		path := "/api/ideas/" + idea.ID + "/verdict"

		later := decodeIdea(do(bob, "PUT", path, fiber.Map{"verdict": "later", "remind_at": inAWeek()}), 200)
		Expect(*later.Verdict).To(Equal("later"))
		Expect(later.RemindAt).NotTo(BeNil())
		Expect(later.DecidedAt).NotTo(BeNil())
		Expect(*later.DecidedBy).To(Equal(bobUser.ID))

		yes := decodeIdea(do(admin, "PUT", path, fiber.Map{"verdict": "yes", "remind_at": nil}), 200)
		Expect(*yes.Verdict).To(Equal("yes"))
		Expect(yes.RemindAt).To(BeNil())
		Expect(*yes.DecidedBy).To(Equal(adminUser.ID))

		inbox := decodeIdea(do(admin, "PUT", path, fiber.Map{"verdict": nil, "remind_at": nil}), 200)
		Expect(inbox.Verdict).To(BeNil())
		Expect(inbox.RemindAt).To(BeNil())
		Expect(inbox.DecidedAt).To(BeNil())
		Expect(inbox.DecidedBy).To(BeNil())

		for _, body := range []string{
			`{"verdict":"later","remind_at":null}`,
			`{"verdict":"yes","remind_at":"` + inAWeek() + `"}`,
			`{"verdict":null,"remind_at":"` + inAWeek() + `"}`,
			`{"verdict":"maybe","remind_at":null}`,
			`{"verdict":"yes"}`,
			`{"remind_at":null}`,
			`{"verdict":"yes","remind_at":null,"title":"x"}`,
			`{"verdict":"later","remind_at":"next week"}`,
			`{"verdict":"later","remind_at":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `"}`,
			`{"verdict":"later","remind_at":"` + time.Now().Add(400*24*time.Hour).UTC().Format(time.RFC3339) + `"}`,
		} {
			Expect(doRaw(admin, "PUT", path, body).StatusCode).To(Equal(400), body)
		}
	})

	It("re-postponing moves both decided_at and remind_at", func() {
		idea := capture(admin, fiber.Map{"title": "again"})
		path := "/api/ideas/" + idea.ID + "/verdict"
		first := decodeIdea(do(admin, "PUT", path, fiber.Map{"verdict": "later", "remind_at": inAWeek()}), 200)
		month := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
		second := decodeIdea(do(admin, "PUT", path, fiber.Map{"verdict": "later", "remind_at": month}), 200)
		Expect(second.RemindAt.After(*first.RemindAt)).To(BeTrue())
		Expect(*second.DecidedAt).To(BeTemporally(">=", *first.DecidedAt))
	})

	It("leaves a woken idea stored as later", func() {
		past := time.Now().Add(-48 * time.Hour).UTC()
		creator := adminUser.ID
		verdict := models.IdeaVerdictLater
		row := &models.Idea{
			ID: "woken1", Title: "woke up", CreatedBy: &creator, CreatedByName: "Admin",
			Verdict: &verdict, RemindAt: &past, DecidedAt: &past, DecidedBy: &creator,
		}
		_, err := db.NewInsert().Model(row).Exec(tenantCtx())
		Expect(err).NotTo(HaveOccurred())

		got := list(admin, "")
		Expect(got).To(HaveLen(1))
		Expect(*got[0].Verdict).To(Equal("later"))
		Expect(got[0].RemindAt.Equal(past.Truncate(time.Microsecond))).To(BeTrue())
	})

	It("rejects an inconsistent row at the database level", func() {
		creator := adminUser.ID
		yes := models.IdeaVerdictYes
		now := time.Now()
		// remind_at on a yes.
		_, err := db.NewInsert().Model(&models.Idea{
			ID: "bad1", Title: "t", CreatedBy: &creator, CreatedByName: "Admin",
			Verdict: &yes, RemindAt: &now, DecidedAt: &now,
		}).Exec(tenantCtx())
		Expect(err).To(HaveOccurred())
		// remind_at on an undecided idea.
		_, err = db.NewInsert().Model(&models.Idea{
			ID: "bad2", Title: "t", CreatedBy: &creator, CreatedByName: "Admin", RemindAt: &now,
		}).Exec(tenantCtx())
		Expect(err).To(HaveOccurred())
		// decided without decided_at.
		_, err = db.NewInsert().Model(&models.Idea{
			ID: "bad3", Title: "t", CreatedBy: &creator, CreatedByName: "Admin", Verdict: &yes,
		}).Exec(tenantCtx())
		Expect(err).To(HaveOccurred())
	})

	// ── DELETE ───────────────────────────────────────────────────────────────

	It("hard-deletes an idea", func() {
		idea := capture(admin, fiber.Map{"title": "bye"})
		Expect(do(bob, "DELETE", "/api/ideas/"+idea.ID, nil).StatusCode).To(Equal(204))
		Expect(list(admin, "")).To(BeEmpty())
		Expect(do(bob, "DELETE", "/api/ideas/"+idea.ID, nil).StatusCode).To(Equal(404))
	})

	// ── Author + tenancy ─────────────────────────────────────────────────────

	It("keeps the author after the user row is hard-deleted", func() {
		idea := capture(bob, fiber.Map{"title": "bob's idea"})
		decodeIdea(do(bob, "PUT", "/api/ideas/"+idea.ID+"/verdict", fiber.Map{"verdict": "no", "remind_at": nil}), 200)

		ctx := context.Background()
		_, err := db.NewDelete().TableExpr("sessions").Where("user_id = ?", bobUser.ID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("id = ?", bobUser.ID).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		got := list(admin, "")
		Expect(got).To(HaveLen(1))
		Expect(got[0].ID).To(Equal(idea.ID))
		Expect(got[0].CreatedBy).To(BeNil())
		Expect(got[0].CreatedByName).To(Equal("Bob"))
		Expect(got[0].DecidedBy).To(BeNil())
		Expect(*got[0].Verdict).To(Equal("no"))
	})

	It("isolates tenants: another workspace's idea is a 404 everywhere", func() {
		idea := capture(admin, fiber.Map{"title": "ours"})
		other := signupTenant("Other", "owner@other.example")

		Expect(list(other, "")).To(BeEmpty())
		Expect(do(other, "PATCH", "/api/ideas/"+idea.ID, fiber.Map{"title": "theirs"}).StatusCode).To(Equal(404))
		Expect(do(other, "PUT", "/api/ideas/"+idea.ID+"/verdict", fiber.Map{"verdict": "no", "remind_at": nil}).StatusCode).To(Equal(404))
		Expect(do(other, "DELETE", "/api/ideas/"+idea.ID, nil).StatusCode).To(Equal(404))

		got := list(admin, "")
		Expect(got).To(HaveLen(1))
		Expect(got[0].Title).To(Equal("ours"))
		Expect(got[0].Verdict).To(BeNil())
	})

	// ── List query ───────────────────────────────────────────────────────────

	It("pages, counts piles, and names the author", func() {
		for _, title := range []string{"one", "two", "three"} {
			capture(admin, fiber.Map{"title": title})
		}
		resp := do(admin, "GET", "/api/ideas?limit=2&sort=created_desc", nil)
		Expect(resp.StatusCode).To(Equal(200))
		var page struct {
			Ideas []struct {
				Title  string `json:"title"`
				Author *struct {
					ID   *string `json:"id"`
					Name string  `json:"name"`
				} `json:"author"`
			} `json:"ideas"`
			Total           int            `json:"total"`
			TotalUnfiltered int            `json:"total_unfiltered"`
			Piles           map[string]int `json:"piles"`
			NextCursor      *string        `json:"next_cursor"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&page)).To(Succeed())
		Expect(page.Ideas).To(HaveLen(2))
		Expect(page.Ideas[0].Title).To(Equal("three"))
		Expect(page.Ideas[0].Author).NotTo(BeNil())
		Expect(*page.Ideas[0].Author.ID).To(Equal(adminUser.ID))
		Expect(page.Ideas[0].Author.Name).To(Equal("Admin"))
		Expect(page.Total).To(Equal(3))
		Expect(page.TotalUnfiltered).To(Equal(3))
		Expect(page.Piles).To(Equal(map[string]int{"waiting": 3, "yes": 0, "later": 0, "no": 0}))
		Expect(page.NextCursor).NotTo(BeNil())

		rest := list(admin, "?limit=2&sort=created_desc&cursor="+*page.NextCursor)
		Expect(rest).To(HaveLen(1))
		Expect(rest[0].Title).To(Equal("one"))
	})

	It("rejects an unknown filter value, a foreign cursor and an unknown scope", func() {
		expectCode := func(path string, status int, code string) {
			GinkgoHelper()
			resp := do(admin, "GET", path, nil)
			Expect(resp.StatusCode).To(Equal(status))
			if code != "" {
				var body map[string]string
				Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
				Expect(body["code"]).To(Equal(code))
			}
		}
		expectCode("/api/ideas?pile=someday", 400, "invalid_filter")
		expectCode("/api/ideas?sort=random", 400, "invalid_filter")
		expectCode("/api/ideas?limit=2&cursor=bm90LWEtY3Vyc29y", 400, "invalid_cursor")
		expectCode("/api/ideas?scope_campaign=nope", 404, "")
	})

	It("refuses edits to an analysed idea's text but moves it between campaigns", func() {
		campaignID := createCampaign(admin, "Launch")
		ctx := tenantctx.With(context.Background(), adminUser.TenantID)
		now := time.Now().UTC()
		_, err := db.NewInsert().Model(&models.Idea{
			ID: "analysed-1", Title: "From the report", Origin: models.IdeaOriginAssetAnalysis,
			CreatedByName: "Ogen", CreatedAt: now, UpdatedAt: now,
		}).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		for _, body := range []fiber.Map{{"title": "Mine now"}, {"note": "edited"}, {"title": "x", "campaign_id": campaignID}} {
			resp := do(admin, "PATCH", "/api/ideas/analysed-1", body)
			Expect(resp.StatusCode).To(Equal(fiber.StatusConflict))
			var got map[string]string
			Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
			Expect(got["code"]).To(Equal("idea_read_only"))
		}
		moved := decodeIdea(do(admin, "PATCH", "/api/ideas/analysed-1", fiber.Map{"campaign_id": campaignID}), 200)
		Expect(*moved.CampaignID).To(Equal(campaignID))
		Expect(moved.Title).To(Equal("From the report"))
	})

	// ── Bulk ─────────────────────────────────────────────────────────────────

	It("selects every match of a filter with ids_only", func() {
		one := capture(admin, fiber.Map{"title": "one"})
		two := capture(bob, fiber.Map{"title": "two"})
		decided := capture(admin, fiber.Map{"title": "decided"})
		decodeIdea(do(admin, "PUT", "/api/ideas/"+decided.ID+"/verdict", fiber.Map{"verdict": "yes", "remind_at": nil}), 200)

		var body struct {
			IDs   []string `json:"ids"`
			Total int      `json:"total"`
		}
		resp := do(admin, "GET", "/api/ideas?pile=waiting&ids_only=true&limit=1", nil)
		Expect(resp.StatusCode).To(Equal(200))
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body.IDs).To(ConsistOf(one.ID, two.ID))
		Expect(body.Total).To(Equal(2))

		Expect(do(admin, "GET", "/api/ideas?ids_only=maybe", nil).StatusCode).To(Equal(400))
	})

	It("deletes a selection in one request and reports the ids it could not delete", func() {
		one := capture(admin, fiber.Map{"title": "one"})
		two := capture(bob, fiber.Map{"title": "two"})
		keep := capture(admin, fiber.Map{"title": "keep"})
		theirs := capture(signupTenant("Other", "owner@other.example"), fiber.Map{"title": "theirs"})

		resp := do(bob, "POST", "/api/ideas/bulk-delete", fiber.Map{"ids": []string{one.ID, two.ID, "ghost", theirs.ID}})
		Expect(resp.StatusCode).To(Equal(200))
		var body struct {
			Deleted []string `json:"deleted"`
			Failed  []struct {
				ID   string `json:"id"`
				Code string `json:"code"`
			} `json:"failed"`
		}
		Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
		Expect(body.Deleted).To(ConsistOf(one.ID, two.ID))
		Expect(body.Failed).To(HaveLen(2))
		for _, f := range body.Failed {
			Expect(f.ID).To(BeElementOf("ghost", theirs.ID))
			Expect(f.Code).To(Equal("not_found"))
		}
		Expect(ids(list(admin, ""))).To(Equal([]string{keep.ID}))

		Expect(ideaHub.events).To(HaveLen(1))
		ev := ideaHub.events[0]
		Expect(ev.Type).To(Equal("ideas.deleted"))
		Expect(ev.Topic).To(Equal("entity:ideas:list"))
		Expect(ev.TenantID).To(Equal(adminUser.TenantID))
		Expect(ev.Payload).To(HaveKeyWithValue("ids", ConsistOf(one.ID, two.ID)))

		for _, bad := range []string{`{"ids": []}`, `{"ids": "x"}`, `{"ids": ["a"], "verdict": "no"}`, `[]`} {
			Expect(doRaw(admin, "POST", "/api/ideas/bulk-delete", bad).StatusCode).To(Equal(400), bad)
		}
	})
})
