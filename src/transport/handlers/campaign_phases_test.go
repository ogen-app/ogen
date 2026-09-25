package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// CON-166: campaign phase date plans, the campaign-type lock and post phase
// integrity (API checks + the DB trigger backstops).
var _ = Describe("Campaign phases (CON-166)", Ordered, func() {
	// Seeded system types: Uk = awareness (phases 98 → xh), Ef = conversion
	// (phases M2 → pr → so).
	const (
		awareness   = "Uk"
		conversion  = "Ef"
		awareness1  = "98"
		awareness2  = "xh"
		conversion1 = "M2"
	)

	var (
		db           *bun.DB
		app          *fiber.App
		ck           *http.Cookie
		campaignRepo repository.CampaignRepository
		postRepo     repository.PostRepository
		tctx         = tenantctx.With(context.Background(), models.DefaultTenantID)
	)

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
		ctRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo = repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), ctRepo)
		postRepo = repository.NewPostRepository(db)
		sRepo := repository.NewSessionRepository(db)
		uRepo := repository.NewUserRepository(db)
		auth := handlers.RequireAuth(sRepo, uRepo, testCookieName)
		handlers.NewSessionsHandler(uRepo, repository.NewAccountRepository(db), sRepo, testCookieName, false).Register(app)
		handlers.NewCampaignTypesHandler(ctRepo, auth).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, ctRepo, auth, nil, nil, nil, nil, nil).Register(app)
		handlers.NewCampaignPhasesHandler(campaignRepo, nil, auth).Register(app)
		ph := handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth)
		ph.SetCampaignRepo(campaignRepo)
		ph.Register(app)

		seedTenantUser(db, "Phase User", "phases@example.com", "phases-password")
		body, _ := json.Marshal(fiber.Map{"email": "phases@example.com", "password": "phases-password"})
		req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		ck = resp.Cookies()[0]
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, t := range []string{"campaigns", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(t).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
		_, err := db.NewDelete().TableExpr("campaigns_types").Where("is_system = false").Exec(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	do := func(method, path string, body any) (int, map[string]any) {
		GinkgoHelper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		out := map[string]any{}
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out
	}

	campaignBody := func(typeID, start, end string) fiber.Map {
		m := fiber.Map{"name": "Phased", "campaign_type_id": typeID}
		if start != "" {
			m["start_date"] = start + "T00:00:00Z"
			m["end_date"] = end + "T00:00:00Z"
		}
		return m
	}

	createCampaign := func(typeID, start, end string) string {
		GinkgoHelper()
		code, body := do("POST", "/api/campaigns", campaignBody(typeID, start, end))
		Expect(code).To(Equal(fiber.StatusCreated))
		return body["id"].(string)
	}

	// createPost creates a draft post; phaseID "" leaves it without a phase.
	createPost := func(campaignID, phaseID string) (int, map[string]any) {
		body := fiber.Map{"campaign_id": campaignID, "title": "t", "content": "c"}
		if phaseID != "" {
			body["campaign_type_phase_id"] = phaseID
		}
		return do("POST", "/api/posts", body)
	}

	// windows flattens a phase-plan response to [phase_id, start, end] rows.
	windows := func(plan map[string]any) [][3]any {
		var out [][3]any
		for _, p := range plan["phases"].([]any) {
			m := p.(map[string]any)
			out = append(out, [3]any{m["phase_id"], m["start_date"], m["end_date"]})
		}
		return out
	}

	Describe("GET /api/campaigns/:id/phases", func() {
		It("derives the windows by splitting the campaign dates across the phases", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			code, plan := do("GET", "/api/campaigns/"+id+"/phases", nil)
			Expect(code).To(Equal(200))
			Expect(plan["source"]).To(Equal("derived"))
			Expect(plan["type_locked"]).To(BeFalse())
			// 31 days / 2 phases → 16 + 15, remainder to the earliest.
			Expect(windows(plan)).To(Equal([][3]any{
				{awareness1, "2026-10-01", "2026-10-16"},
				{awareness2, "2026-10-17", "2026-10-31"},
			}))
		})

		It("reports an undated campaign as unscheduled with null dates", func() {
			id := createCampaign(awareness, "", "")
			code, plan := do("GET", "/api/campaigns/"+id+"/phases", nil)
			Expect(code).To(Equal(200))
			Expect(plan["source"]).To(Equal("unscheduled"))
			Expect(windows(plan)).To(Equal([][3]any{{awareness1, nil, nil}, {awareness2, nil, nil}}))
		})

		It("404s for an unknown campaign", func() {
			code, _ := do("GET", "/api/campaigns/nope/phases", nil)
			Expect(code).To(Equal(404))
		})
	})

	Describe("PUT/DELETE /api/campaigns/:id/phases", func() {
		plan := func(a1s, a1e, a2s, a2e string) fiber.Map {
			return fiber.Map{"phases": []fiber.Map{
				{"phase_id": awareness1, "start_date": a1s, "end_date": a1e},
				{"phase_id": awareness2, "start_date": a2s, "end_date": a2e},
			}}
		}

		It("stores a valid manual plan, then resets it to derived", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			code, body := do("PUT", "/api/campaigns/"+id+"/phases", plan("2026-10-01", "2026-10-05", "2026-10-06", "2026-10-31"))
			Expect(code).To(Equal(200))
			Expect(body["source"]).To(Equal("manual"))

			_, got := do("GET", "/api/campaigns/"+id+"/phases", nil)
			Expect(got["source"]).To(Equal("manual"))
			Expect(windows(got)).To(Equal([][3]any{
				{awareness1, "2026-10-01", "2026-10-05"},
				{awareness2, "2026-10-06", "2026-10-31"},
			}))

			code, reset := do("DELETE", "/api/campaigns/"+id+"/phases", nil)
			Expect(code).To(Equal(200))
			Expect(reset["source"]).To(Equal("derived"))
			Expect(windows(reset)[0]).To(Equal([3]any{awareness1, "2026-10-01", "2026-10-16"}))
		})

		It("rejects a plan with a gap, an overlap, a wrong bound or a missing phase", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			for _, bad := range []fiber.Map{
				plan("2026-10-01", "2026-10-05", "2026-10-07", "2026-10-31"), // gap
				plan("2026-10-01", "2026-10-06", "2026-10-06", "2026-10-31"), // overlap
				plan("2026-10-02", "2026-10-05", "2026-10-06", "2026-10-31"), // late start
				plan("2026-10-01", "2026-10-05", "2026-10-06", "2026-10-30"), // early end
				{"phases": []fiber.Map{{"phase_id": awareness1, "start_date": "2026-10-01", "end_date": "2026-10-31"}}},
				plan("2026-10-01", "not-a-date", "2026-10-06", "2026-10-31"),
			} {
				code, body := do("PUT", "/api/campaigns/"+id+"/phases", bad)
				Expect(code).To(Equal(400), "plan %v", bad)
				Expect(body["code"]).To(Equal("invalid_phase_plan"))
			}
			_, got := do("GET", "/api/campaigns/"+id+"/phases", nil)
			Expect(got["source"]).To(Equal("derived"))
		})

		It("409s on an undated campaign", func() {
			id := createCampaign(awareness, "", "")
			code, body := do("PUT", "/api/campaigns/"+id+"/phases", plan("2026-10-01", "2026-10-05", "2026-10-06", "2026-10-31"))
			Expect(code).To(Equal(409))
			Expect(body["code"]).To(Equal("campaign_unscheduled"))
		})

		It("re-anchors a manual plan when the campaign dates widen, and resets it when a window would empty", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			code, _ := do("PUT", "/api/campaigns/"+id+"/phases", plan("2026-10-01", "2026-10-05", "2026-10-06", "2026-10-31"))
			Expect(code).To(Equal(200))

			code, body := do("PUT", "/api/campaigns/"+id, campaignBody(awareness, "2026-09-25", "2026-11-10"))
			Expect(code).To(Equal(200))
			Expect(body).NotTo(HaveKey("phase_plan_reset"))
			_, got := do("GET", "/api/campaigns/"+id+"/phases", nil)
			Expect(got["source"]).To(Equal("manual"))
			Expect(windows(got)).To(Equal([][3]any{
				{awareness1, "2026-09-25", "2026-10-05"},
				{awareness2, "2026-10-06", "2026-11-10"},
			}))

			// Starting after the first phase's end would empty it → back to derived.
			code, body = do("PUT", "/api/campaigns/"+id, campaignBody(awareness, "2026-10-10", "2026-11-10"))
			Expect(code).To(Equal(200))
			Expect(body["phase_plan_reset"]).To(BeTrue())
			_, got = do("GET", "/api/campaigns/"+id+"/phases", nil)
			Expect(got["source"]).To(Equal("derived"))
		})
	})

	Describe("campaign-type lock", func() {
		It("locks the type once a post is planned against a phase", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			code, _ := createPost(id, awareness1)
			Expect(code).To(Equal(fiber.StatusCreated))

			_, got := do("GET", "/api/campaigns/"+id, nil)
			Expect(got["type_locked"]).To(BeTrue())

			code, body := do("PUT", "/api/campaigns/"+id, campaignBody(conversion, "2026-10-01", "2026-10-31"))
			Expect(code).To(Equal(409))
			Expect(body["code"]).To(Equal("campaign_type_locked"))
			Expect(body["phased_post_count"]).To(BeEquivalentTo(1))

			_, got = do("GET", "/api/campaigns/"+id, nil)
			Expect(got["campaign_type_id"]).To(Equal(awareness))

			// An unrelated edit that keeps the type still saves.
			code, _ = do("PUT", "/api/campaigns/"+id, campaignBody(awareness, "2026-10-01", "2026-11-30"))
			Expect(code).To(Equal(200))
		})

		It("allows a switch while no post has a phase, dropping any manual plan", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			code, _ := createPost(id, "") // a phase-less post doesn't lock
			Expect(code).To(Equal(fiber.StatusCreated))
			code, _ = do("PUT", "/api/campaigns/"+id+"/phases", fiber.Map{"phases": []fiber.Map{
				{"phase_id": awareness1, "start_date": "2026-10-01", "end_date": "2026-10-05"},
				{"phase_id": awareness2, "start_date": "2026-10-06", "end_date": "2026-10-31"},
			}})
			Expect(code).To(Equal(200))

			code, body := do("PUT", "/api/campaigns/"+id, campaignBody(conversion, "2026-10-01", "2026-10-31"))
			Expect(code).To(Equal(200))
			Expect(body["phase_plan_reset"]).To(BeTrue())
			_, got := do("GET", "/api/campaigns/"+id+"/phases", nil)
			Expect(got["source"]).To(Equal("derived"))
			Expect(got["phases"]).To(HaveLen(3))
		})

		It("is backstopped by the DB trigger when the API check is bypassed", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			code, _ := createPost(id, awareness1)
			Expect(code).To(Equal(fiber.StatusCreated))

			c, err := campaignRepo.GetByID(tctx, id)
			Expect(err).NotTo(HaveOccurred())
			c.CampaignTypeID = conversion
			err = campaignRepo.Update(tctx, c)
			Expect(repository.IsConstraintViolation(err, repository.ConstraintCampaignTypeLocked)).To(BeTrue(), "err = %v", err)
		})
	})

	Describe("post phase integrity", func() {
		It("rejects a phase from another campaign type on create and update", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			code, body := createPost(id, conversion1)
			Expect(code).To(Equal(400))
			Expect(body["code"]).To(Equal("invalid_campaign_type_phase_id"))

			code, post := createPost(id, awareness1)
			Expect(code).To(Equal(fiber.StatusCreated))
			code, body = do("PUT", "/api/posts/"+post["id"].(string), fiber.Map{
				"campaign_id": id, "title": "t", "content": "c", "campaign_type_phase_id": conversion1,
			})
			Expect(code).To(Equal(400))
			Expect(body["code"]).To(Equal("invalid_campaign_type_phase_id"))
		})

		It("rejects moving a post to a campaign of another type while keeping its phase", func() {
			from := createCampaign(awareness, "2026-10-01", "2026-10-31")
			to := createCampaign(conversion, "2026-10-01", "2026-10-31")
			code, post := createPost(from, awareness1)
			Expect(code).To(Equal(fiber.StatusCreated))
			code, body := do("PUT", "/api/posts/"+post["id"].(string), fiber.Map{
				"campaign_id": to, "title": "t", "content": "c", "campaign_type_phase_id": awareness1,
			})
			Expect(code).To(Equal(400))
			Expect(body["code"]).To(Equal("invalid_campaign_type_phase_id"))
		})

		It("is backstopped by the DB trigger on a direct repository write", func() {
			id := createCampaign(awareness, "2026-10-01", "2026-10-31")
			c, err := campaignRepo.GetByID(tctx, id)
			Expect(err).NotTo(HaveOccurred())
			phase := conversion1
			postID, _ := models.NewID()
			err = postRepo.Create(tctx, &models.Post{
				ID: postID, CampaignID: id, CampaignTypePhaseID: &phase, Title: "t", Content: "c",
				Status: models.PostStatusDraft, MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
				CTAType: models.CTATypeNone, CreatedBy: c.CreatedBy,
			})
			Expect(repository.IsConstraintViolation(err, repository.ConstraintPhaseMatchesCampaignType)).To(BeTrue(), "err = %v", err)
		})

		It("409s deleting a custom-type phase that posts still reference", func() {
			code, ct := do("POST", "/api/campaign_types/"+awareness+"/clone", fiber.Map{"name": "phase-guard-type", "label": "Phase guard"})
			Expect(code).To(Equal(fiber.StatusCreated))
			typeID := ct["id"].(string)
			phaseID := ct["phases"].([]any)[0].(map[string]any)["id"].(string)

			id := createCampaign(typeID, "2026-10-01", "2026-10-31")
			code, _ = createPost(id, phaseID)
			Expect(code).To(Equal(fiber.StatusCreated))

			code, body := do("DELETE", "/api/campaign_types/"+typeID+"/phases/"+phaseID, nil)
			Expect(code).To(Equal(409))
			Expect(body["code"]).To(Equal("phase_in_use"))
		})
	})
})
