package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// Campaigns and posts can bind a specific brand voice + audience, and a
// post's own voice/audience is settable via the targeted PUT /:id/brand.
var _ = Describe("Brand bindings (CON-245)", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		brandRepo  repository.BrandRepository
		campaignID string
		postID     string
	)

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

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
		sessionRepo := repository.NewSessionRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		brandRepo = repository.NewBrandRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), campaignTypeRepo)
		postRepo := repository.NewPostRepository(db)

		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		ch := handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil)
		ch.SetBrandRepo(brandRepo)
		ch.Register(app)
		ph := handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth)
		ph.SetBrandRepo(brandRepo)
		ph.Register(app)

		user := seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]

		// Seed brand voice + audience, a campaign, and a post directly.
		ctx := tenantCtx()
		Expect(brandRepo.CreateVoice(ctx, &models.BrandVoice{
			ID: "v-1", Name: "Dry British",
			Samples: models.StringSlice{}, ChannelNotes: models.StringMap{},
			Origin: models.BrandOrigin{Kind: "blank"},
		})).To(Succeed())
		Expect(brandRepo.CreateAudience(ctx, &models.BrandAudience{
			ID: "a-1", Name: "Sceptics", Origin: models.BrandOrigin{Kind: "blank"},
		})).To(Succeed())

		campaignID = "camp-1"
		Expect(campaignRepo.Create(ctx, &models.Campaign{
			ID: campaignID, Name: "C", CampaignTypeID: seededCampaignTypeID,
			AssetIDs: models.StringSlice{}, TargetPlatforms: models.CampaignPlatforms{},
			TagIDs: models.StringSlice{}, PublishingDays: models.StringSlice{},
			Status: models.StatusDraft, PublishingTime: "09:00", SpreadMinutes: 15,
			GoalCadence: "month", CreatedBy: user.ID,
		})).To(Succeed())

		postID = "post-1"
		Expect(postRepo.Create(ctx, &models.Post{
			ID: postID, CampaignID: campaignID, Content: "hi",
			Status: models.PostStatusDraft, CTAType: models.CTATypeNone,
			MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
			CreatedBy: user.ID,
		})).To(Succeed())
	})

	AfterEach(func() {
		for _, tbl := range []string{"posts", "campaigns", "brand_voices", "brand_audiences", "sessions", "users", "accounts"} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	do := func(method, path string, body any) *http.Response {
		GinkgoHelper()
		var r *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		} else {
			r = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	strptr := func(s string) *string { return &s }

	Describe("PUT /api/posts/:id/brand", func() {
		It("sets a post's own voice + audience and round-trips", func() {
			resp := do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{
				"brand_voice_id": "v-1", "brand_audience_id": "a-1",
			})
			Expect(resp.StatusCode).To(Equal(200))
			var p models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
			Expect(p.BrandVoiceID).To(Equal(strptr("v-1")))
			Expect(p.BrandAudienceID).To(Equal(strptr("a-1")))

			// Persisted.
			resp = do("GET", "/api/posts/"+postID, nil)
			Expect(resp.StatusCode).To(Equal(200))
			var got models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
			Expect(got.BrandVoiceID).To(Equal(strptr("v-1")))
			Expect(got.BrandAudienceID).To(Equal(strptr("a-1")))

			// Usage is now computable (unblocks CON-228 FR7). The draft
			// post now counts against voice v-1 and audience a-1.
			brand, err := brandRepo.GetAll(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
			var vu, au models.BrandUsage
			for _, v := range brand.Voices {
				if v.ID == "v-1" {
					vu = v.Usage
				}
			}
			for _, a := range brand.Audiences {
				if a.ID == "a-1" {
					au = a.Usage
				}
			}
			Expect(vu).To(Equal(models.BrandUsage{Drafts: 1, Published: 0}))
			Expect(au).To(Equal(models.BrandUsage{Drafts: 1, Published: 0}))
		})

		It("clears the refs when set to null", func() {
			Expect(do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_voice_id": "v-1"}).StatusCode).To(Equal(200))
			resp := do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_voice_id": nil, "brand_audience_id": nil})
			Expect(resp.StatusCode).To(Equal(200))
			var p models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
			Expect(p.BrandVoiceID).To(BeNil())
			Expect(p.BrandAudienceID).To(BeNil())
		})

		// CON-245 fix: the two fields are independent. An omitted field must leave
		// the stored ref alone; only an explicit null clears it. Before the
		// presence-aware change, sending only brand_voice_id wiped the audience.
		It("leaves the other ref untouched when only one field is sent", func() {
			Expect(do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_voice_id": "v-1", "brand_audience_id": "a-1"}).StatusCode).To(Equal(200))

			// Send only the voice — the audience must survive.
			resp := do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_voice_id": "v-1"})
			Expect(resp.StatusCode).To(Equal(200))
			var p models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
			Expect(p.BrandVoiceID).To(Equal(strptr("v-1")))
			Expect(p.BrandAudienceID).To(Equal(strptr("a-1")))

			// An explicit null on just the audience clears it, keeping the voice.
			resp = do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_audience_id": nil})
			Expect(resp.StatusCode).To(Equal(200))
			var p2 models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&p2)).To(Succeed())
			Expect(p2.BrandVoiceID).To(Equal(strptr("v-1")))
			Expect(p2.BrandAudienceID).To(BeNil())
		})
	})

	// CON-245 fix: the whole-resource PUT is a full replace for its
	// client-authored fields, but the brand refs are server-stamped by
	// content_plan / draft_post — so an ordinary save that omits them (what the
	// UI sends today) must not null them.
	Describe("whole-resource PUT /api/posts/:id preserves brand refs", func() {
		It("keeps server-stamped refs when the body omits them", func() {
			Expect(do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_voice_id": "v-1", "brand_audience_id": "a-1"}).StatusCode).To(Equal(200))

			resp := do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "content": "edited"})
			Expect(resp.StatusCode).To(Equal(200))
			var p models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
			Expect(p.BrandVoiceID).To(Equal(strptr("v-1")))
			Expect(p.BrandAudienceID).To(Equal(strptr("a-1")))
		})

		It("clears a ref on an explicit null, leaving an omitted one alone", func() {
			Expect(do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_voice_id": "v-1", "brand_audience_id": "a-1"}).StatusCode).To(Equal(200))

			resp := do("PUT", "/api/posts/"+postID, fiber.Map{"campaign_id": campaignID, "brand_voice_id": nil})
			Expect(resp.StatusCode).To(Equal(200))
			var p models.Post
			Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
			Expect(p.BrandVoiceID).To(BeNil())
			Expect(p.BrandAudienceID).To(Equal(strptr("a-1")))
		})

		It("422s a voice/audience id not in the tenant", func() {
			Expect(do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_voice_id": "nope"}).StatusCode).To(Equal(422))
			Expect(do("PUT", "/api/posts/"+postID+"/brand", fiber.Map{"brand_audience_id": "nope"}).StatusCode).To(Equal(422))
		})

		It("404s an unknown post", func() {
			Expect(do("PUT", "/api/posts/ghost/brand", fiber.Map{"brand_voice_id": "v-1"}).StatusCode).To(Equal(404))
		})
	})

	Describe("campaign brand fields", func() {
		It("sets and round-trips brand_voice_id/brand_audience_id, 422 on a foreign id", func() {
			body := fiber.Map{"name": "C", "campaign_type_id": seededCampaignTypeID, "brand_voice_id": "v-1", "brand_audience_id": "a-1"}
			resp := do("PUT", "/api/campaigns/"+campaignID, body)
			Expect(resp.StatusCode).To(Equal(200))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			Expect(c.BrandVoiceID).To(Equal(strptr("v-1")))
			Expect(c.BrandAudienceID).To(Equal(strptr("a-1")))

			bad := fiber.Map{"name": "C", "campaign_type_id": seededCampaignTypeID, "brand_voice_id": "nope"}
			Expect(do("PUT", "/api/campaigns/"+campaignID, bad).StatusCode).To(Equal(422))
		})

		// CON-245 fix: an ordinary campaign save that omits the brand fields must
		// not null them; an explicit null still clears.
		It("preserves refs when the update omits them, clears on explicit null", func() {
			set := fiber.Map{"name": "C", "campaign_type_id": seededCampaignTypeID, "brand_voice_id": "v-1", "brand_audience_id": "a-1"}
			Expect(do("PUT", "/api/campaigns/"+campaignID, set).StatusCode).To(Equal(200))

			// Save with the brand fields omitted — refs survive.
			resp := do("PUT", "/api/campaigns/"+campaignID, fiber.Map{"name": "C2", "campaign_type_id": seededCampaignTypeID})
			Expect(resp.StatusCode).To(Equal(200))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			Expect(c.BrandVoiceID).To(Equal(strptr("v-1")))
			Expect(c.BrandAudienceID).To(Equal(strptr("a-1")))

			// Explicit null on the voice clears it; the omitted audience stays.
			resp = do("PUT", "/api/campaigns/"+campaignID, fiber.Map{"name": "C2", "campaign_type_id": seededCampaignTypeID, "brand_voice_id": nil})
			Expect(resp.StatusCode).To(Equal(200))
			var c2 models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c2)).To(Succeed())
			Expect(c2.BrandVoiceID).To(BeNil())
			Expect(c2.BrandAudienceID).To(Equal(strptr("a-1")))
		})
	})
})

// seededCampaignTypeID is a system campaign type seeded by the reference-data
// migration (same ids campaigns_test uses).
const seededCampaignTypeID = "gb"
