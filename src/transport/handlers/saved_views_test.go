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
	"github.com/ogen-app/ogen/src/usecase/assets"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/savedviews"
)

// savedViewWire decodes a SavedView as the UI sees it.
type savedViewWire struct {
	ID        string         `json:"id"`
	Surface   string         `json:"surface"`
	Name      string         `json:"name"`
	Query     string         `json:"query"`
	Shared    bool           `json:"shared"`
	CreatedBy *string        `json:"created_by"`
	Valid     bool           `json:"valid"`
	CanEdit   bool           `json:"can_edit"`
	Counts    map[string]int `json:"counts"`
}

var _ = Describe("SavedViewsHandler", Ordered, func() {
	var (
		app      *fiber.App
		db       *bun.DB
		hub      *quietHub
		userRepo repository.UserRepository
		annUser  *models.User
		owner    *http.Cookie
		ann      *http.Cookie
		bob      *http.Cookie
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
		hub = &quietHub{}
		userRepo = repository.NewUserRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		ideaRepo := repository.NewIdeaRepository(db)
		assetRepo := repository.NewAssetRepository(db, repository.NewTagRepository(db), repository.NewAssetFileRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewIdeasHandler(ideas.New(ideaRepo, userRepo), auth, nil).Register(app)
		svc := savedviews.New(repository.NewSavedViewRepository(db), hub, ideas.NewSurface(ideaRepo), assets.NewSurface(assetRepo))
		handlers.NewSavedViewsHandler(svc, userRepo, auth).Register(app)

		seedTenantUserWithRole(db, "Olga", "owner@example.com", "owner-password", models.RoleOwner)
		annUser = seedTenantUser(db, "Ann", "ann@example.com", "ann-password")
		seedTenantUser(db, "Bob", "bob@example.com", "bob-password")
		owner = login("owner@example.com", "owner-password")
		ann = login("ann@example.com", "ann-password")
		bob = login("bob@example.com", "bob-password")
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{"saved_views", "ideas", "assets", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
		}
	})

	do := func(cookie *http.Cookie, method, path string, body any) *http.Response {
		GinkgoHelper()
		b := ""
		if body != nil {
			raw, _ := json.Marshal(body)
			b = string(raw)
		}
		req := httptest.NewRequest(method, path, strings.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	decode := func(resp *http.Response, status int, into any) {
		GinkgoHelper()
		Expect(resp.StatusCode).To(Equal(status))
		Expect(json.NewDecoder(resp.Body).Decode(into)).To(Succeed())
	}

	create := func(cookie *http.Cookie, name, query string, shared bool) savedViewWire {
		GinkgoHelper()
		var v savedViewWire
		decode(do(cookie, "POST", "/api/saved-views", fiber.Map{
			"surface": "ideas", "name": name, "query": query, "shared": shared,
		}), fiber.StatusCreated, &v)
		return v
	}

	list := func(cookie *http.Cookie, extra string) []savedViewWire {
		GinkgoHelper()
		var body struct {
			Views []savedViewWire `json:"views"`
		}
		decode(do(cookie, "GET", "/api/saved-views?surface=ideas"+extra, nil), 200, &body)
		return body.Views
	}

	names := func(views []savedViewWire) []string {
		out := make([]string, len(views))
		for i, v := range views {
			out[i] = v.Name
		}
		return out
	}

	expectCode := func(resp *http.Response, status int, code, param string) {
		GinkgoHelper()
		var body map[string]string
		decode(resp, status, &body)
		Expect(body["code"]).To(Equal(code))
		Expect(body["param"]).To(Equal(param))
	}

	It("requires auth and a registered surface", func() {
		Expect(do(nil, "GET", "/api/saved-views?surface=ideas", nil).StatusCode).To(Equal(401))
		expectCode(do(ann, "GET", "/api/saved-views?surface=posts", nil), 400, "unknown_surface", "")
		expectCode(do(ann, "POST", "/api/saved-views", fiber.Map{"surface": "posts", "name": "x"}), 400, "unknown_surface", "")
	})

	It("stores the query canonical, without paging, and validates it with the list's parser", func() {
		v := create(ann, "  Strong PDFs  ", "kind=web,pdf&fit=high&pile=waiting&limit=50&cursor=abc&text=+agents+&sort=fit", false)
		Expect(v.Name).To(Equal("Strong PDFs"))
		Expect(v.Query).To(Equal("fit=high&kind=pdf,web&sort=fit&text=agents"))
		Expect(v.Surface).To(Equal("ideas"))
		Expect(v.Shared).To(BeFalse())
		Expect(v.Valid).To(BeTrue())
		Expect(v.CanEdit).To(BeTrue())
		Expect(*v.CreatedBy).To(Equal(annUser.ID))

		body := func(query string) fiber.Map { return fiber.Map{"surface": "ideas", "name": "x", "query": query} }
		expectCode(do(ann, "POST", "/api/saved-views", body("kind=film")), 400, "invalid_query", "kind")
		expectCode(do(ann, "POST", "/api/saved-views", body("colour=red")), 400, "invalid_query", "colour")
		expectCode(do(ann, "POST", "/api/saved-views", body("group=tag")), 400, "invalid_query", "group")
		Expect(do(ann, "POST", "/api/saved-views", fiber.Map{"surface": "ideas", "name": "  "}).StatusCode).To(Equal(400))
		Expect(do(ann, "POST", "/api/saved-views", fiber.Map{"surface": "ideas", "name": strings.Repeat("é", 81)}).StatusCode).To(Equal(400))
	})

	It("shows a private view to its creator only, and a shared one to everyone", func() {
		create(ann, "Mine", "origin=manual", false)
		create(ann, "Ours", "origin=asset_analysis", true)

		Expect(names(list(ann, ""))).To(Equal([]string{"Ours", "Mine"}))
		bobs := list(bob, "")
		Expect(names(bobs)).To(Equal([]string{"Ours"}))
		Expect(bobs[0].CanEdit).To(BeFalse())
		owners := list(owner, "")
		Expect(names(owners)).To(Equal([]string{"Ours"}))
		Expect(owners[0].CanEdit).To(BeTrue())
	})

	It("lets the creator, or an owner for a shared view, change it", func() {
		mine := create(ann, "Mine", "origin=manual", false)
		ours := create(ann, "Ours", "origin=manual", true)

		Expect(do(bob, "PATCH", "/api/saved-views/"+ours.ID, fiber.Map{"name": "Bob's"}).StatusCode).To(Equal(403))
		Expect(do(bob, "DELETE", "/api/saved-views/"+ours.ID, nil).StatusCode).To(Equal(403))
		Expect(do(bob, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"name": "Bob's"}).StatusCode).To(Equal(404))
		Expect(do(owner, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"name": "Olga's"}).StatusCode).To(Equal(404))

		var renamed savedViewWire
		decode(do(owner, "PATCH", "/api/saved-views/"+ours.ID, fiber.Map{"name": "Team view"}), 200, &renamed)
		Expect(renamed.Name).To(Equal("Team view"))
		Expect(renamed.Query).To(Equal("origin=manual"))
		Expect(renamed.Shared).To(BeTrue())

		var edited savedViewWire
		decode(do(ann, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"query": "fit=low,high", "shared": true}), 200, &edited)
		Expect(edited.Query).To(Equal("fit=high,low"))
		Expect(edited.Shared).To(BeTrue())
		Expect(edited.Name).To(Equal("Mine"))

		Expect(do(ann, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"surface": "assets"}).StatusCode).To(Equal(400))
		Expect(do(ann, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"name": nil}).StatusCode).To(Equal(400))
		expectCode(do(ann, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"query": "fit=great"}), 400, "invalid_query", "fit")

		Expect(do(owner, "DELETE", "/api/saved-views/"+ours.ID, nil).StatusCode).To(Equal(204))
		Expect(names(list(bob, ""))).To(Equal([]string{"Mine"}))
	})

	It("counts every view in one list call, and flags a stored query that no longer parses", func() {
		for _, title := range []string{"one", "two"} {
			Expect(do(ann, "POST", "/api/ideas", fiber.Map{"title": title}).StatusCode).To(Equal(201))
		}
		var idea struct {
			ID string `json:"id"`
		}
		decode(do(bob, "POST", "/api/ideas", fiber.Map{"title": "three"}), 201, &idea)
		Expect(do(bob, "PUT", "/api/ideas/"+idea.ID+"/verdict", fiber.Map{"verdict": "yes", "remind_at": nil}).StatusCode).To(Equal(200))

		all := create(ann, "All", "", false)
		create(ann, "Written", "origin=manual&author="+annUser.ID, false)
		broken := create(ann, "Broken", "fit=high", false)
		_, err := db.NewUpdate().TableExpr("saved_views").Set("query = 'fit=great'").Where("id = ?", broken.ID).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())

		views := list(ann, "&counts=total,undecided")
		Expect(names(views)).To(Equal([]string{"Broken", "Written", "All"}))
		Expect(views[0].Valid).To(BeFalse())
		Expect(views[0].Counts).To(BeNil())
		Expect(views[1].Counts).To(Equal(map[string]int{"total": 2, "undecided": 2}))
		Expect(views[2].Counts).To(Equal(map[string]int{"total": 3, "undecided": 2}))
		Expect(views[2].ID).To(Equal(all.ID))

		Expect(list(ann, "")[1].Counts).To(BeNil())
		Expect(do(ann, "GET", "/api/saved-views?surface=ideas&counts=total,stars", nil).StatusCode).To(Equal(400))
	})

	It("deletes a removed member's private views and leaves their shared ones to the owners", func() {
		create(ann, "Mine", "origin=manual", false)
		ours := create(ann, "Ours", "origin=manual", true)

		Expect(userRepo.RemoveMemberGuarded(context.Background(), annUser.ID, annUser.TenantID)).To(Succeed())

		count, err := db.NewSelect().TableExpr("saved_views").Count(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(1))
		views := list(owner, "")
		Expect(names(views)).To(Equal([]string{"Ours"}))
		Expect(views[0].CreatedBy).To(BeNil())
		Expect(views[0].CanEdit).To(BeTrue())
		Expect(list(bob, "")[0].CanEdit).To(BeFalse())

		Expect(do(owner, "PATCH", "/api/saved-views/"+ours.ID, fiber.Map{"shared": false}).StatusCode).To(Equal(400))
		Expect(do(owner, "PATCH", "/api/saved-views/"+ours.ID, fiber.Map{"name": "Adopted"}).StatusCode).To(Equal(200))
	})

	It("announces changes to shared views only", func() {
		mine := create(ann, "Mine", "", false)
		Expect(hub.events).To(BeEmpty())

		ours := create(ann, "Ours", "", true)
		Expect(do(ann, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"shared": true}).StatusCode).To(Equal(200))
		Expect(do(ann, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"shared": false}).StatusCode).To(Equal(200))
		Expect(do(ann, "PATCH", "/api/saved-views/"+mine.ID, fiber.Map{"name": "Still mine"}).StatusCode).To(Equal(200))
		Expect(do(ann, "DELETE", "/api/saved-views/"+ours.ID, nil).StatusCode).To(Equal(204))

		var types []string
		for _, ev := range hub.events {
			Expect(ev.Topic).To(Equal("entity:saved_views:ideas"))
			Expect(ev.TenantID).To(Equal(annUser.TenantID))
			Expect(ev.UserID).To(BeEmpty())
			types = append(types, ev.Type)
		}
		Expect(types).To(Equal([]string{"saved_view.created", "saved_view.updated", "saved_view.updated", "saved_view.deleted"}))
		Expect(hub.events[0].Payload).To(Equal(map[string]string{"id": ours.ID, "surface": "ideas", "action": "created"}))
	})

	Describe("the assets surface", func() {
		createAsset := func(cookie *http.Cookie, name, query string, shared bool) savedViewWire {
			GinkgoHelper()
			var v savedViewWire
			decode(do(cookie, "POST", "/api/saved-views", fiber.Map{
				"surface": "assets", "name": name, "query": query, "shared": shared,
			}), fiber.StatusCreated, &v)
			return v
		}
		listAssets := func(cookie *http.Cookie, extra string) []savedViewWire {
			GinkgoHelper()
			var body struct {
				Views []savedViewWire `json:"views"`
			}
			decode(do(cookie, "GET", "/api/saved-views?surface=assets"+extra, nil), 200, &body)
			return body.Views
		}

		It("stores the query canonical, without volatile parameters, and validates it with the list's parser", func() {
			v := createAsset(ann, "Fresh PDFs",
				"kind=web,pdf&kind=pdf&updated=7d&limit=50&cursor=abc&facets=kind&tz=Europe/Kyiv&ids_only=true"+
					"&scope_campaign=c1&checksum="+strings.Repeat("a", 64)+"&text=+agents+&sort=title&group=tag", false)
			Expect(v.Surface).To(Equal("assets"))
			Expect(v.Query).To(Equal("group=tag&kind=pdf,web&sort=title&text=agents&updated=7d"))
			Expect(v.Valid).To(BeTrue())

			body := func(query string) fiber.Map { return fiber.Map{"surface": "assets", "name": "x", "query": query} }
			expectCode(do(ann, "POST", "/api/saved-views", body("kind=film")), 400, "invalid_query", "kind")
			expectCode(do(ann, "POST", "/api/saved-views", body("colour=red")), 400, "invalid_query", "colour")
			expectCode(do(ann, "POST", "/api/saved-views", body("updated_after=soon")), 400, "invalid_query", "updated_after")
			expectCode(do(ann, "POST", "/api/saved-views", body("updated=7d&updated_after=2026-10-01")), 400, "invalid_query", "updated")
			expectCode(do(ann, "POST", "/api/saved-views", body("text=a")), 400, "invalid_query", "text")
			expectCode(do(ann, "POST", "/api/saved-views", body("sort=relevance")), 400, "invalid_query", "sort")
		})

		It("counts each view in the viewer's zone, and flags a stored query that no longer parses", func() {
			kyiv, err := time.LoadLocation("Europe/Kyiv")
			Expect(err).NotTo(HaveOccurred())
			ctx := tenantctx.With(context.Background(), annUser.TenantID)
			for id, at := range map[string]time.Time{
				"sv-a1": time.Date(2026, 10, 5, 10, 0, 0, 0, kyiv),
				// The 8th in Kyiv, still the 7th in UTC.
				"sv-a2": time.Date(2026, 10, 8, 0, 30, 0, 0, kyiv),
				"sv-a3": time.Date(2026, 9, 20, 10, 0, 0, 0, kyiv),
			} {
				_, err := db.NewInsert().Model(&models.Asset{ID: id, Title: id, Status: models.AssetStatusReady,
					CreatedBy: annUser.ID, CreatedAt: at, UpdatedAt: at}).Exec(ctx)
				Expect(err).NotTo(HaveOccurred())
			}

			all := createAsset(ann, "All", "", false)
			week := createAsset(ann, "First week", "created_after=2026-10-01&created_before=2026-10-08", true)
			broken := createAsset(ann, "Broken", "kind=pdf", false)
			_, err = db.NewUpdate().TableExpr("saved_views").Set("query = 'kind=film'").Where("id = ?", broken.ID).Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
			createAsset(bob, "Bob's private", "", false)

			views := listAssets(ann, "&counts=total&tz=Europe/Kyiv")
			Expect(names(views)).To(Equal([]string{"Broken", "First week", "All"}))
			Expect(views[0].Valid).To(BeFalse())
			Expect(views[0].Counts).To(BeNil())
			Expect(views[1].ID).To(Equal(week.ID))
			Expect(views[1].Counts).To(Equal(map[string]int{"total": 1}))
			Expect(views[2].ID).To(Equal(all.ID))
			Expect(views[2].Counts).To(Equal(map[string]int{"total": 3}))

			utc := listAssets(ann, "&counts=total&tz=UTC")
			Expect(utc[1].Counts).To(Equal(map[string]int{"total": 2}))

			Expect(names(listAssets(bob, ""))).To(Equal([]string{"Bob's private", "First week"}))
			Expect(do(ann, "GET", "/api/saved-views?surface=assets&counts=undecided", nil).StatusCode).To(Equal(400))
		})

		It("announces shared asset views on the assets topic", func() {
			ours := createAsset(ann, "Ours", "kind=pdf", true)
			Expect(do(ann, "DELETE", "/api/saved-views/"+ours.ID, nil).StatusCode).To(Equal(204))
			Expect(hub.events).To(HaveLen(2))
			for _, ev := range hub.events {
				Expect(ev.Topic).To(Equal("entity:saved_views:assets"))
			}
			Expect(hub.events[0].Payload).To(Equal(map[string]string{"id": ours.ID, "surface": "assets", "action": "created"}))
		})
	})
})
