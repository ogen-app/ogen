package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/series"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// seriesWire decodes a series exactly as the UI sees it, keeping nulls visible.
type seriesWire struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Promise       string               `json:"promise"`
	Recipe        string               `json:"recipe"`
	Supply        string               `json:"supply"`
	ContentFormat *string              `json:"content_format"`
	DefaultRhythm *models.SeriesRhythm `json:"default_rhythm"`
	CampaignID    *string              `json:"campaign_id"`
	Usage         models.SeriesUsage   `json:"usage"`
}

type runWire struct {
	SeriesID string               `json:"series_id"`
	Rhythm   *models.SeriesRhythm `json:"rhythm"`
}

type campaignSeriesWire struct {
	CampaignID string    `json:"campaign_id"`
	Runs       []runWire `json:"runs"`
}

var _ = Describe("SeriesHandler", Ordered, func() {
	var (
		app      *fiber.App
		db       *bun.DB
		admin    *http.Cookie
		postRepo repository.PostRepository
		svc      *series.Service
		userID   string
		// held are open transactions a spec may still hold locks with.
		held []bun.Tx
	)

	// Runs before AfterEach's table cleanup, which would wait on a held lock.
	JustAfterEach(func() {
		for _, tx := range held {
			_ = tx.Rollback()
		}
		held = nil
	})

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
	})

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
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), campaignTypeRepo)
		postRepo = repository.NewPostRepository(db)
		svc = series.New(repository.NewSeriesRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)

		handlers.NewSessionsHandler(userRepo, accountRepo, sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewTenantsHandler(signup.New(db, accountRepo, tenantRepo, nil), tenantRepo, testCookieName, false, auth, nil).Register(app)
		handlers.NewSeriesHandler(svc, auth, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth, handlers.PostsOptions{Series: svc}).Register(app)

		userID = seedTenantUser(db, "Admin", "admin@example.com", "admin-password").ID
		body, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		admin = resp.Cookies()[0]
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"post_logs", "post_versions", "posts", "campaign_series", "brand_series", "campaigns", "sessions", "users", "accounts"} {
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

	decodeInto := func(resp *http.Response, status int, out any) {
		GinkgoHelper()
		Expect(resp.StatusCode).To(Equal(status))
		Expect(json.NewDecoder(resp.Body).Decode(out)).To(Succeed())
	}

	create := func(cookie *http.Cookie, body fiber.Map) seriesWire {
		GinkgoHelper()
		var s seriesWire
		decodeInto(do(cookie, "POST", "/api/series", body), fiber.StatusCreated, &s)
		return s
	}

	list := func(cookie *http.Cookie) []seriesWire {
		GinkgoHelper()
		var body struct {
			Series []seriesWire `json:"series"`
		}
		decodeInto(do(cookie, "GET", "/api/series", nil), 200, &body)
		return body.Series
	}

	runsOf := func(cookie *http.Cookie, campaignID string) campaignSeriesWire {
		GinkgoHelper()
		var cs campaignSeriesWire
		decodeInto(do(cookie, "GET", "/api/campaigns/"+campaignID+"/series", nil), 200, &cs)
		return cs
	}

	campaignWrite := func(resp *http.Response) campaignSeriesWire {
		GinkgoHelper()
		var cs campaignSeriesWire
		decodeInto(resp, 200, &cs)
		return cs
	}

	createCampaign := func(cookie *http.Cookie, name string) string {
		GinkgoHelper()
		resp := do(cookie, "POST", "/api/campaigns", fiber.Map{"name": name, "campaign_type_id": "Uk"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var c models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
		return c.ID
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

	weekly := func(times int) fiber.Map { return fiber.Map{"times": times, "per": "week"} }

	digest := func(extra fiber.Map) fiber.Map {
		m := fiber.Map{"name": "Weekly news digest", "supply": "self"}
		maps.Copy(m, extra)
		return m
	}

	seedPost := func(id, campaignID string, seriesID *string, status models.PostStatus) {
		GinkgoHelper()
		p := &models.Post{
			ID: id, CampaignID: campaignID, Content: "hi",
			Status: status, CTAType: models.CTATypeNone, SeriesID: seriesID,
			MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
			CreatedBy: userID,
		}
		if status != models.PostStatusDraft {
			p.PlatformID, p.PlatformPostType = "AXqWG7U2qnpt", "text-post"
		}
		Expect(postRepo.Create(tenantCtx(), p)).To(Succeed())
	}

	// ── CRUD ─────────────────────────────────────────────────────────────────

	It("requires auth", func() {
		Expect(do(nil, "GET", "/api/series", nil).StatusCode).To(Equal(401))
		Expect(do(nil, "GET", "/api/campaigns/x/series", nil).StatusCode).To(Equal(401))
	})

	It("returns a wrapped, empty (not null) list for a fresh workspace", func() {
		resp := do(admin, "GET", "/api/series", nil)
		Expect(resp.StatusCode).To(Equal(200))
		raw := map[string]json.RawMessage{}
		Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
		Expect(string(raw["series"])).To(Equal("[]"))
	})

	It("creates a library series, trimming text, with nulls and zero usage", func() {
		s := create(admin, fiber.Map{"name": "  This day in finance history ", "supply": "self", "usage": fiber.Map{"drafts": 9}})
		Expect(s.ID).NotTo(BeEmpty())
		Expect(s.Name).To(Equal("This day in finance history"))
		Expect(s.Promise).To(Equal(""))
		Expect(s.Recipe).To(Equal(""))
		Expect(s.ContentFormat).To(BeNil())
		Expect(s.DefaultRhythm).To(BeNil())
		Expect(s.CampaignID).To(BeNil())
		Expect(s.Usage).To(Equal(models.SeriesUsage{}))

		Expect(list(admin)).To(HaveLen(1))
	})

	It("validates the series", func() {
		bad := []fiber.Map{
			{"supply": "self"},
			{"name": "   ", "supply": "self"},
			{"name": strings.Repeat("a", 121), "supply": "self"},
			digest(fiber.Map{"promise": strings.Repeat("a", 281)}),
			digest(fiber.Map{"recipe": strings.Repeat("a", 4001)}),
			{"name": "x"},
			{"name": "x", "supply": "both"},
			digest(fiber.Map{"content_format": "guide"}),
			digest(fiber.Map{"content_format": ""}),
			digest(fiber.Map{"default_rhythm": weekly(0)}),
			digest(fiber.Map{"default_rhythm": weekly(32)}),
			digest(fiber.Map{"default_rhythm": fiber.Map{"times": 2, "per": "day"}}),
			digest(fiber.Map{"default_rhythm": fiber.Map{}}),
		}
		for _, body := range bad {
			Expect(do(admin, "POST", "/api/series", body).StatusCode).To(Equal(400), "%v", body)
		}
		Expect(doRaw(admin, "POST", "/api/series", `{"name":`).StatusCode).To(Equal(400))
		Expect(list(admin)).To(BeEmpty())
	})

	It("replaces the editable fields on PUT and ignores campaign_id and usage", func() {
		s := create(admin, digest(nil))
		camp := createCampaign(admin, "Launch")

		var got seriesWire
		decodeInto(do(admin, "PUT", "/api/series/"+s.ID, fiber.Map{
			"name": "Digest", "promise": "The week in five links", "recipe": "Pick five", "supply": "idea",
			"content_format": "digest", "default_rhythm": weekly(2),
			"campaign_id": camp, "usage": fiber.Map{"published": 99},
		}), 200, &got)
		Expect(got.Name).To(Equal("Digest"))
		Expect(got.Promise).To(Equal("The week in five links"))
		Expect(got.Supply).To(Equal("idea"))
		Expect(*got.ContentFormat).To(Equal("digest"))
		Expect(got.DefaultRhythm).To(Equal(&models.SeriesRhythm{Times: 2, Per: "week"}))
		Expect(got.CampaignID).To(BeNil())
		Expect(got.Usage).To(Equal(models.SeriesUsage{}))

		// A whole-resource write: what is left out is cleared.
		decodeInto(do(admin, "PUT", "/api/series/"+s.ID, fiber.Map{"name": "Digest", "supply": "idea"}), 200, &got)
		Expect(got.Promise).To(Equal(""))
		Expect(got.ContentFormat).To(BeNil())
		Expect(got.DefaultRhythm).To(BeNil())

		Expect(do(admin, "PUT", "/api/series/"+s.ID, fiber.Map{"name": "", "supply": "idea"}).StatusCode).To(Equal(400))
		Expect(do(admin, "PUT", "/api/series/nope", digest(nil)).StatusCode).To(Equal(404))
	})

	It("keeps another workspace's series invisible", func() {
		mine := create(admin, digest(nil))
		other := signupTenant("Other", "owner@other.example")
		theirs := create(other, digest(fiber.Map{"name": "Theirs"}))

		Expect(list(admin)).To(HaveLen(1))
		Expect(list(other)).To(HaveLen(1))
		Expect(do(admin, "PUT", "/api/series/"+theirs.ID, digest(nil)).StatusCode).To(Equal(404))
		Expect(do(admin, "DELETE", "/api/series/"+theirs.ID, nil).StatusCode).To(Equal(404))
		Expect(do(admin, "POST", "/api/series/"+theirs.ID+"/promote", nil).StatusCode).To(Equal(404))
		Expect(do(other, "PUT", "/api/series/"+mine.ID, digest(nil)).StatusCode).To(Equal(404))

		camp := createCampaign(admin, "Mine")
		Expect(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": theirs.ID}).StatusCode).To(Equal(404))
		theirCamp := createCampaign(other, "Theirs")
		Expect(do(admin, "GET", "/api/campaigns/"+theirCamp+"/series", nil).StatusCode).To(Equal(404))
		Expect(do(admin, "POST", "/api/campaigns/"+theirCamp+"/series", fiber.Map{"series_id": mine.ID}).StatusCode).To(Equal(404))
	})

	// ── Campaign-local series and promote ────────────────────────────────────

	It("attaches a campaign-local series to its campaign at its default rhythm", func() {
		camp := createCampaign(admin, "Launch")
		local := create(admin, digest(fiber.Map{"campaign_id": camp, "default_rhythm": weekly(1)}))
		Expect(*local.CampaignID).To(Equal(camp))

		cs := runsOf(admin, camp)
		Expect(cs.CampaignID).To(Equal(camp))
		Expect(cs.Runs).To(Equal([]runWire{{SeriesID: local.ID, Rhythm: &models.SeriesRhythm{Times: 1, Per: "week"}}}))

		Expect(do(admin, "POST", "/api/series", digest(fiber.Map{"campaign_id": "nope"})).StatusCode).To(Equal(404))
	})

	It("promotes one-way, keeping the run, and treats promoting a library series as a no-op", func() {
		camp := createCampaign(admin, "Launch")
		local := create(admin, digest(fiber.Map{"campaign_id": camp}))

		var got seriesWire
		decodeInto(do(admin, "POST", "/api/series/"+local.ID+"/promote", nil), 200, &got)
		Expect(got.CampaignID).To(BeNil())
		Expect(runsOf(admin, camp).Runs).To(HaveLen(1))

		decodeInto(do(admin, "POST", "/api/series/"+local.ID+"/promote", nil), 200, &got)
		Expect(got.CampaignID).To(BeNil())

		// Now in the library, another campaign can pick it up.
		other := createCampaign(admin, "Other")
		Expect(do(admin, "POST", "/api/campaigns/"+other+"/series", fiber.Map{"series_id": local.ID}).StatusCode).To(Equal(200))
	})

	// ── Campaign runs ────────────────────────────────────────────────────────

	It("attaches idempotently, keeping an existing rhythm", func() {
		camp := createCampaign(admin, "Launch")
		a := create(admin, digest(fiber.Map{"name": "A", "default_rhythm": weekly(2)}))
		b := create(admin, digest(fiber.Map{"name": "B"}))

		cs := campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": a.ID}))
		Expect(cs.Runs).To(Equal([]runWire{{SeriesID: a.ID, Rhythm: &models.SeriesRhythm{Times: 2, Per: "week"}}}))

		monthly := fiber.Map{"times": 3, "per": "month"}
		campaignWrite(do(admin, "PUT", "/api/campaigns/"+camp+"/series/"+a.ID, fiber.Map{"rhythm": monthly}))

		cs = campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": a.ID}))
		Expect(cs.Runs).To(Equal([]runWire{{SeriesID: a.ID, Rhythm: &models.SeriesRhythm{Times: 3, Per: "month"}}}))

		cs = campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": b.ID}))
		Expect(cs.Runs).To(Equal([]runWire{
			{SeriesID: a.ID, Rhythm: &models.SeriesRhythm{Times: 3, Per: "month"}},
			{SeriesID: b.ID, Rhythm: nil},
		}))
		Expect(runsOf(admin, camp)).To(Equal(cs))
	})

	It("detaches and sets a rhythm one row at a time", func() {
		camp := createCampaign(admin, "Launch")
		a := create(admin, digest(fiber.Map{"name": "A"}))
		b := create(admin, digest(fiber.Map{"name": "B", "default_rhythm": weekly(1)}))
		campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": a.ID}))
		campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": b.ID}))

		cs := campaignWrite(do(admin, "PUT", "/api/campaigns/"+camp+"/series/"+a.ID, fiber.Map{"rhythm": weekly(4)}))
		Expect(cs.Runs).To(Equal([]runWire{
			{SeriesID: a.ID, Rhythm: &models.SeriesRhythm{Times: 4, Per: "week"}},
			{SeriesID: b.ID, Rhythm: &models.SeriesRhythm{Times: 1, Per: "week"}},
		}))

		cs = campaignWrite(do(admin, "PUT", "/api/campaigns/"+camp+"/series/"+b.ID, fiber.Map{"rhythm": nil}))
		Expect(cs.Runs[1].Rhythm).To(BeNil())

		cs = campaignWrite(do(admin, "DELETE", "/api/campaigns/"+camp+"/series/"+a.ID, nil))
		Expect(cs.Runs).To(Equal([]runWire{{SeriesID: b.ID, Rhythm: nil}}))

		// Detaching what is not attached answers unchanged.
		cs = campaignWrite(do(admin, "DELETE", "/api/campaigns/"+camp+"/series/"+a.ID, nil))
		Expect(cs.Runs).To(HaveLen(1))

		// The library series themselves are untouched.
		Expect(list(admin)).To(HaveLen(2))
	})

	It("rejects bad campaign series writes", func() {
		camp := createCampaign(admin, "Launch")
		a := create(admin, digest(nil))

		Expect(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{}).StatusCode).To(Equal(400))
		Expect(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": "nope"}).StatusCode).To(Equal(404))
		Expect(do(admin, "POST", "/api/campaigns/nope/series", fiber.Map{"series_id": a.ID}).StatusCode).To(Equal(404))
		Expect(do(admin, "GET", "/api/campaigns/nope/series", nil).StatusCode).To(Equal(404))
		Expect(do(admin, "DELETE", "/api/campaigns/nope/series/"+a.ID, nil).StatusCode).To(Equal(404))

		// Rhythm on a series the campaign does not run.
		Expect(do(admin, "PUT", "/api/campaigns/"+camp+"/series/"+a.ID, fiber.Map{"rhythm": weekly(1)}).StatusCode).To(Equal(404))

		campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": a.ID}))
		Expect(do(admin, "PUT", "/api/campaigns/"+camp+"/series/"+a.ID, fiber.Map{}).StatusCode).To(Equal(400))
		Expect(do(admin, "PUT", "/api/campaigns/"+camp+"/series/"+a.ID, fiber.Map{"rhythm": weekly(0)}).StatusCode).To(Equal(400))
		Expect(do(admin, "PUT", "/api/campaigns/"+camp+"/series/"+a.ID, fiber.Map{"rhythm": weekly(1), "series_id": a.ID}).StatusCode).To(Equal(400))
	})

	It("refuses to attach a series local to another campaign", func() {
		home := createCampaign(admin, "Home")
		away := createCampaign(admin, "Away")
		local := create(admin, digest(fiber.Map{"campaign_id": home}))

		Expect(do(admin, "POST", "/api/campaigns/"+away+"/series", fiber.Map{"series_id": local.ID}).StatusCode).To(Equal(400))
		Expect(runsOf(admin, away).Runs).To(BeEmpty())
	})

	// ── Delete ───────────────────────────────────────────────────────────────

	It("soft-deletes a series, dropping its runs and keeping posts' series_id", func() {
		camp := createCampaign(admin, "Launch")
		a := create(admin, digest(nil))
		campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": a.ID}))
		seedPost("p1", camp, &a.ID, models.PostStatusDraft)

		Expect(do(admin, "DELETE", "/api/series/"+a.ID, nil).StatusCode).To(Equal(204))
		Expect(list(admin)).To(BeEmpty())
		Expect(runsOf(admin, camp).Runs).To(BeEmpty())
		Expect(do(admin, "DELETE", "/api/series/"+a.ID, nil).StatusCode).To(Equal(404))
		Expect(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": a.ID}).StatusCode).To(Equal(404))

		var p models.Post
		decodeInto(do(admin, "GET", "/api/posts/p1", nil), 200, &p)
		Expect(p.SeriesID).To(Equal(&a.ID))
	})

	It("removes a deleted campaign's local series and runs", func() {
		camp := createCampaign(admin, "Launch")
		lib := create(admin, digest(fiber.Map{"name": "Library"}))
		local := create(admin, digest(fiber.Map{"name": "Local", "campaign_id": camp}))
		campaignWrite(do(admin, "POST", "/api/campaigns/"+camp+"/series", fiber.Map{"series_id": lib.ID}))
		seedPost("p1", camp, &local.ID, models.PostStatusDraft)

		Expect(do(admin, "DELETE", "/api/campaigns/"+camp, nil).StatusCode).To(Equal(204))

		remaining := list(admin)
		Expect(remaining).To(HaveLen(1))
		Expect(remaining[0].ID).To(Equal(lib.ID))
		var runs int
		Expect(db.NewSelect().TableExpr("campaign_series").ColumnExpr("count(*)").Scan(context.Background(), &runs)).To(Succeed())
		Expect(runs).To(Equal(0))
		var kept *string
		Expect(db.NewSelect().TableExpr("posts").Column("series_id").Where("id = ?", "p1").Scan(context.Background(), &kept)).To(Succeed())
		Expect(kept).To(Equal(&local.ID))
	})

	// ── Concurrent deletes ───────────────────────────────────────────────────

	// holdDelete soft-deletes a row in an open transaction, the way the series
	// and campaign deletes do, and returns the commit. A spec that fails before
	// committing rolls back, so the held lock never blocks cleanup.
	holdDelete := func(table, id string) func() {
		GinkgoHelper()
		tx, err := db.BeginTx(context.Background(), nil)
		Expect(err).NotTo(HaveOccurred())
		held = append(held, tx)
		_, err = tx.NewUpdate().TableExpr(table).Set("deleted_at = now()").Where("id = ?", id).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		return func() { Expect(tx.Commit()).To(Succeed()) }
	}

	It("orders attach behind a series delete in flight, so no run outlives the series", func() {
		camp := createCampaign(admin, "Launch")
		a := create(admin, digest(nil))
		commit := holdDelete("brand_series", a.ID)

		done := make(chan error, 1)
		go func() {
			_, err := svc.Attach(tenantCtx(), camp, a.ID)
			done <- err
		}()
		Consistently(done, "300ms").ShouldNot(Receive())
		commit()
		Eventually(done, "5s").Should(Receive(MatchError(series.ErrNotFound)))
		Expect(runsOf(admin, camp).Runs).To(BeEmpty())
	})

	It("orders a campaign-local create behind a campaign delete in flight", func() {
		camp := createCampaign(admin, "Launch")
		commit := holdDelete("campaigns", camp)

		done := make(chan error, 1)
		go func() {
			_, err := svc.Create(tenantCtx(), series.Input{Name: "Local", Supply: models.SeriesSupplySelf, CampaignID: &camp})
			done <- err
		}()
		Consistently(done, "300ms").ShouldNot(Receive())
		commit()
		Eventually(done, "5s").Should(Receive(MatchError(series.ErrCampaignNotFound)))
		Expect(list(admin)).To(BeEmpty())
	})

	// ── Usage ────────────────────────────────────────────────────────────────

	It("counts usage from posts: published vs everything else", func() {
		camp := createCampaign(admin, "Launch")
		a := create(admin, digest(fiber.Map{"name": "A"}))
		b := create(admin, digest(fiber.Map{"name": "B"}))
		seedPost("p1", camp, &a.ID, models.PostStatusDraft)
		seedPost("p2", camp, &a.ID, models.PostStatusScheduled)
		seedPost("p3", camp, &a.ID, models.PostStatusFailed)
		seedPost("p4", camp, &a.ID, models.PostStatusPublished)
		seedPost("p5", camp, nil, models.PostStatusPublished)

		byID := map[string]models.SeriesUsage{}
		for _, s := range list(admin) {
			byID[s.ID] = s.Usage
		}
		Expect(byID[a.ID]).To(Equal(models.SeriesUsage{Drafts: 3, Published: 1}))
		Expect(byID[b.ID]).To(Equal(models.SeriesUsage{}))

		var got seriesWire
		decodeInto(do(admin, "PUT", "/api/series/"+a.ID, digest(fiber.Map{"name": "A"})), 200, &got)
		Expect(got.Usage).To(Equal(models.SeriesUsage{Drafts: 3, Published: 1}))
	})

	// ── series_id on the post ────────────────────────────────────────────────

	Describe("series_id on the post", func() {
		var camp, lib string

		BeforeEach(func() {
			camp = createCampaign(admin, "Launch")
			lib = create(admin, digest(nil)).ID
			seedPost("p1", camp, nil, models.PostStatusDraft)
		})

		put := func(extra fiber.Map) *http.Response {
			body := fiber.Map{"campaign_id": camp, "content": "hi"}
			maps.Copy(body, extra)
			return do(admin, "PUT", "/api/posts/p1", body)
		}

		It("round-trips through GET and the list, is null by default, and is presence-aware", func() {
			resp := do(admin, "GET", "/api/posts/p1", nil)
			var raw map[string]any
			decodeInto(resp, 200, &raw)
			Expect(raw).To(HaveKeyWithValue("series_id", BeNil()))

			var p models.Post
			decodeInto(put(fiber.Map{"series_id": lib}), 200, &p)
			Expect(p.SeriesID).To(Equal(&lib))

			var all []models.Post
			decodeInto(do(admin, "GET", "/api/posts", nil), 200, &all)
			Expect(all).To(HaveLen(1))
			Expect(all[0].SeriesID).To(Equal(&lib))

			decodeInto(put(nil), 200, &p)
			Expect(p.SeriesID).To(Equal(&lib))

			decodeInto(put(fiber.Map{"series_id": nil}), 200, &p)
			Expect(p.SeriesID).To(BeNil())
		})

		It("accepts a series on create", func() {
			var p models.Post
			decodeInto(do(admin, "POST", "/api/posts", fiber.Map{"campaign_id": camp, "content": "new", "series_id": lib}), 201, &p)
			Expect(p.SeriesID).To(Equal(&lib))
			Expect(do(admin, "POST", "/api/posts", fiber.Map{"campaign_id": camp, "content": "new", "series_id": "nope"}).StatusCode).To(Equal(400))
		})

		It("400s an unknown, foreign, deleted or out-of-scope series", func() {
			Expect(put(fiber.Map{"series_id": "nope"}).StatusCode).To(Equal(400))

			other := signupTenant("Other", "owner@other.example")
			theirs := create(other, digest(nil))
			Expect(put(fiber.Map{"series_id": theirs.ID}).StatusCode).To(Equal(400))

			gone := create(admin, digest(fiber.Map{"name": "Gone"}))
			Expect(do(admin, "DELETE", "/api/series/"+gone.ID, nil).StatusCode).To(Equal(204))
			Expect(put(fiber.Map{"series_id": gone.ID}).StatusCode).To(Equal(400))

			away := createCampaign(admin, "Away")
			awayLocal := create(admin, digest(fiber.Map{"campaign_id": away}))
			Expect(put(fiber.Map{"series_id": awayLocal.ID}).StatusCode).To(Equal(400))

			homeLocal := create(admin, digest(fiber.Map{"campaign_id": camp}))
			Expect(put(fiber.Map{"series_id": homeLocal.ID}).StatusCode).To(Equal(200))

			// Its campaign-local series cannot follow the post to another campaign.
			Expect(do(admin, "PUT", "/api/posts/p1", fiber.Map{"campaign_id": away, "content": "hi"}).StatusCode).To(Equal(400))
		})

		It("keeps saving a post whose series was deleted", func() {
			Expect(put(fiber.Map{"series_id": lib}).StatusCode).To(Equal(200))
			Expect(do(admin, "DELETE", "/api/series/"+lib, nil).StatusCode).To(Equal(204))

			Expect(put(fiber.Map{"content": "edited", "series_id": lib}).StatusCode).To(Equal(200))
			Expect(put(fiber.Map{"content": "edited again"}).StatusCode).To(Equal(200))
		})

		It("refuses a series change on a submitted post with 409", func() {
			seedPost("locked", camp, &lib, models.PostStatusScheduled)
			other := create(admin, digest(fiber.Map{"name": "Other"})).ID
			body := func(extra fiber.Map) fiber.Map {
				m := fiber.Map{
					"campaign_id": camp, "platform_id": "AXqWG7U2qnpt", "platform_post_type": "text-post",
					"content": "hi", "status": "scheduled",
				}
				maps.Copy(m, extra)
				return m
			}
			Expect(do(admin, "PUT", "/api/posts/locked", body(fiber.Map{"series_id": other})).StatusCode).To(Equal(409))
			Expect(do(admin, "PUT", "/api/posts/locked", body(fiber.Map{"series_id": nil})).StatusCode).To(Equal(409))
			Expect(do(admin, "PUT", "/api/posts/locked", body(fiber.Map{"series_id": lib})).StatusCode).To(Equal(200))
		})
	})
})
