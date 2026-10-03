package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/database"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("Analytics endpoints", Ordered, func() {
	var (
		app               *fiber.App
		db                *bun.DB
		authCookie        *http.Cookie
		campaignID        string
		userID            string
		postRepo          repository.PostRepository
		analyticsRepo     repository.PostAnalyticsRepository
		socialAccountRepo repository.SocialAccountRepository
		auth              fiber.Handler
	)

	const linkedinSqid = "AXqWG7U2qnpt"

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
		// CON-125 Track B: post_analytics_snapshots lives in the analytics
		// migration set. In tests one physical DB holds both sets (the table
		// names don't collide), so the analytics repo is pointed at the same
		// pool after applying the analytics migrations.
		Expect(database.MigrateAnalytics(context.Background(), db)).To(Succeed())
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
		sessionRepo := repository.NewSessionRepository(db)
		settingRepo := repository.NewSettingRepository(db)
		tagRepo := repository.NewTagRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		postRepo = repository.NewPostRepository(db)
		analyticsRepo = repository.NewPostAnalyticsRepository(db)
		socialAccountRepo = repository.NewSocialAccountRepository(db)
		auth = handlers.RequireAuth(sessionRepo, userRepo, testCookieName)

		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth, nil, nil).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)

		ph := handlers.NewPostsHandler(postRepo, repository.NewPostVersionRepository(db), repository.NewPlatformRepository(db), repository.NewPostAttachmentRepository(db), auth, handlers.PostsOptions{})

		ph.Register(app)
		// GET /:id/analytics now lives on the insights handler.
		handlers.NewPostInsightsHandler(postRepo, nil, nil, analyticsRepo, nil, nil, auth).Register(app)
		handlers.NewAnalyticsHandler(analyticsRepo, nil, postRepo, repository.NewPlatformRepository(db), socialAccountRepo, campaignRepo, nil, nil, auth).Register(app)

		// Auth user + login.
		createdUser := seedTenantUser(db, "Admin", "admin@example.com", "admin-password")
		userID = createdUser.ID

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]

		// Campaign for the seeded posts.
		cBody, _ := json.Marshal(fiber.Map{"name": "Analytics Campaign", "campaign_type_id": "Uk"})
		cReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(cBody))
		cReq.Header.Set("Content-Type", "application/json")
		cReq.AddCookie(authCookie)
		cResp, err := app.Test(cReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(cResp.StatusCode).To(Equal(fiber.StatusCreated))
		var c models.Campaign
		Expect(json.NewDecoder(cResp.Body).Decode(&c)).To(Succeed())
		campaignID = c.ID
	})

	AfterEach(func() {
		ctx := tenantCtx()
		_, _ = db.NewDelete().TableExpr("post_analytics_current").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_analytics_snapshots").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("social_accounts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("posts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
	})

	seedPost := func(id, publisher, publisherPostID string, published time.Time) {
		p := &models.Post{
			ID:              id,
			CampaignID:      campaignID,
			PlatformID:      linkedinSqid,
			Title:           "Post " + id,
			Content:         "content " + id,
			MediaURLs:       models.StringSlice{},
			UsedAssetIDs:    models.StringSlice{},
			Status:          models.PostStatusPublished,
			Publisher:       publisher,
			PublisherPostID: publisherPostID,
			CTAType:         models.CTATypeNone,
			CreatedBy:       userID,
			PublishedAt:     &published,
		}
		Expect(postRepo.Create(tenantCtx(), p)).To(Succeed())
	}

	seedSnapshot := func(postID, pubPostID string, impressions, likes int, rate float64) {
		now := time.Now().UTC()
		Expect(analyticsRepo.Upsert(tenantCtx(), &models.PostAnalytics{
			PostID:          postID,
			PublisherPostID: pubPostID,
			Publisher:       models.PublisherZernio,
			// Denormalised platform name (was joined from platforms.name).
			Platform:       "LinkedIn",
			Impressions:    impressions,
			Likes:          likes,
			EngagementRate: rate,
			PlatformAnalytics: models.PlatformAnalyticsList{
				{Platform: "linkedin", SyncStatus: "synced", Analytics: models.PostAnalyticsMetrics{Impressions: impressions, Likes: likes}},
			},
			SyncStatus:    "synced",
			FirstSeenAt:   now,
			LastChangedAt: now,
			LastCheckedAt: now,
		})).To(Succeed())
	}

	get := func(path string) *http.Response {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	Describe("GET /api/posts/:id/analytics", func() {
		It("returns 404 for a missing post", func() {
			Expect(get("/api/posts/does-not-exist/analytics").StatusCode).To(Equal(404))
		})

		It("returns 409 not_published_via_publisher for a post without a publisher post id", func() {
			seedPost("p-draft", "", "", time.Now().UTC())
			resp := get("/api/posts/p-draft/analytics")
			Expect(resp.StatusCode).To(Equal(409))
			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["code"]).To(Equal("not_published_via_publisher"))
		})

		It("returns 200 pending when the post has no snapshot yet", func() {
			seedPost("p-pending", models.PublisherZernio, "z-pending", time.Now().UTC())
			resp := get("/api/posts/p-pending/analytics")
			Expect(resp.StatusCode).To(Equal(200))
			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["status"]).To(Equal("pending"))
			Expect(body["post_id"]).To(Equal("p-pending"))
		})

		It("returns 200 with the snapshot when covered", func() {
			seedPost("p1", models.PublisherZernio, "z-1", time.Now().UTC())
			seedSnapshot("p1", "z-1", 1234, 56, 0.07)
			resp := get("/api/posts/p1/analytics")
			Expect(resp.StatusCode).To(Equal(200))
			var body struct {
				PostID    string `json:"post_id"`
				Publisher string `json:"publisher"`
				Analytics struct {
					Impressions int `json:"impressions"`
					Likes       int `json:"likes"`
				} `json:"analytics"`
				PlatformAnalytics []struct {
					Platform   string `json:"platform"`
					SyncStatus string `json:"sync_status"`
				} `json:"platform_analytics"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body.PostID).To(Equal("p1"))
			Expect(body.Publisher).To(Equal("zernio"))
			Expect(body.Analytics.Impressions).To(Equal(1234))
			Expect(body.Analytics.Likes).To(Equal(56))
			Expect(body.PlatformAnalytics).To(HaveLen(1))
			Expect(body.PlatformAnalytics[0].Platform).To(Equal("linkedin"))
		})
	})

	Describe("GET /api/analytics/posts", func() {
		It("returns paged, sorted items plus overview, Zernio-only", func() {
			seedPost("p1", models.PublisherZernio, "z-1", time.Now().UTC().Add(-2*time.Hour))
			seedPost("p2", models.PublisherZernio, "z-2", time.Now().UTC().Add(-1*time.Hour))
			seedPost("p3", "", "", time.Now().UTC()) // non-publisher post, no snapshot
			seedSnapshot("p1", "z-1", 100, 10, 0.10)
			seedSnapshot("p2", "z-2", 300, 30, 0.20)

			resp := get("/api/analytics/posts?sort_by=impressions&order=desc&limit=50")
			Expect(resp.StatusCode).To(Equal(200))
			var body struct {
				Items []struct {
					PostID    string `json:"post_id"`
					Platform  string `json:"platform"`
					Analytics struct {
						Impressions int `json:"impressions"`
					} `json:"analytics"`
				} `json:"items"`
				Pagination struct {
					Page, Limit, Total, Pages int
				} `json:"pagination"`
				Overview struct {
					PostCount         int     `json:"post_count"`
					Impressions       int     `json:"impressions"`
					EngagementRateAvg float64 `json:"engagement_rate_avg"`
				} `json:"overview"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body.Items).To(HaveLen(2))
			Expect(body.Items[0].PostID).To(Equal("p2")) // impressions desc
			Expect(body.Items[0].Platform).To(Equal("LinkedIn"))
			Expect(body.Items[0].Analytics.Impressions).To(Equal(300))
			Expect(body.Pagination.Total).To(Equal(2))
			Expect(body.Overview.PostCount).To(Equal(2))
			Expect(body.Overview.Impressions).To(Equal(400))
		})

		It("rejects an unknown sort_by with 400", func() {
			Expect(get("/api/analytics/posts?sort_by=bogus").StatusCode).To(Equal(400))
		})

		It("rejects an unknown order with 400", func() {
			Expect(get("/api/analytics/posts?order=sideways").StatusCode).To(Equal(400))
		})
	})

	Describe("platform filter on /overview, /performers, /learnings", func() {
		const (
			instagramSqid = "rzgpTkARLH0L"
			xSqid         = "81mUCmc2xsKd"
		)

		// seedOn publishes one Zernio post on the given platform and writes its
		// current analytics row under that platform's display name.
		seedOn := func(id, platformID, displayName string, published time.Time, reach int) {
			p := &models.Post{
				ID: id, CampaignID: campaignID, PlatformID: platformID,
				Title: "Post " + id, Content: "content " + id,
				MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
				Status: models.PostStatusPublished, Publisher: models.PublisherZernio,
				PublisherPostID: "z-" + id, CTAType: models.CTATypeNone,
				CreatedBy: userID, PublishedAt: &published,
			}
			Expect(postRepo.Create(tenantCtx(), p)).To(Succeed())
			now := time.Now().UTC()
			Expect(analyticsRepo.Upsert(tenantCtx(), &models.PostAnalytics{
				PostID: id, PublisherPostID: "z-" + id, Publisher: models.PublisherZernio,
				Platform: displayName, PublishedAt: &published, Reach: reach, Impressions: reach,
				SyncStatus: "synced", FirstSeenAt: now, LastChangedAt: now, LastCheckedAt: now,
			})).To(Succeed())
		}

		seedThree := func() {
			at := time.Now().UTC().Add(-48 * time.Hour)
			seedOn("li-1", linkedinSqid, "LinkedIn", at, 100)
			seedOn("ig-1", instagramSqid, "Instagram", at, 200)
			seedOn("x-1", xSqid, "X (Twitter)", at, 400)
		}

		decode := func(resp *http.Response, into any) {
			Expect(resp.StatusCode).To(Equal(200))
			Expect(json.NewDecoder(resp.Body).Decode(into)).To(Succeed())
		}

		type performersBody struct {
			Data struct {
				TotalPosts int `json:"total_posts"`
			} `json:"data"`
		}

		It("rejects an unknown slug with 400 on every endpoint", func() {
			for _, path := range []string{"/api/analytics/overview", "/api/analytics/performers", "/api/analytics/learnings"} {
				resp := get(path + "?platform=linkedin&platform=linkdin")
				Expect(resp.StatusCode).To(Equal(400), path)
				var body map[string]any
				Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
				Expect(body["error"]).To(Equal("invalid_platform"))
			}
		})

		It("narrows /performers to the union of the repeated slugs", func() {
			seedThree()

			var all, union, csv performersBody
			decode(get("/api/analytics/performers"), &all)
			decode(get("/api/analytics/performers?platform=linkedin&platform=instagram"), &union)
			decode(get("/api/analytics/performers?platform=LinkedIn,instagram"), &csv)
			Expect(all.Data.TotalPosts).To(Equal(3))
			Expect(union.Data.TotalPosts).To(Equal(2))
			Expect(csv.Data.TotalPosts).To(Equal(2))
		})

		It("matches by platform id, not display name", func() {
			seedThree()
			var body performersBody
			decode(get("/api/analytics/performers?platform=twitter"), &body)
			Expect(body.Data.TotalPosts).To(Equal(1))
		})

		It("narrows /overview reach and posts published", func() {
			seedThree()
			type card struct {
				Metric string  `json:"metric"`
				Value  float64 `json:"value"`
			}
			var body struct {
				Data struct {
					Cards []card `json:"cards"`
				} `json:"data"`
			}
			decode(get("/api/analytics/overview?platform=instagram&platform=twitter"), &body)
			values := map[string]float64{}
			for _, c := range body.Data.Cards {
				values[c.Metric] = c.Value
			}
			Expect(values["reach"]).To(Equal(600.0))
			Expect(values["posts_published"]).To(Equal(2.0))
		})

		It("applies the /learnings minimum-support floor after narrowing", func() {
			base := time.Now().UTC().Add(-30 * 24 * time.Hour)
			for i := range 6 {
				seedOn("li-"+strconv.Itoa(i), linkedinSqid, "LinkedIn", base.Add(time.Duration(i)*time.Hour), 100+i)
			}
			seedOn("ig-0", instagramSqid, "Instagram", base, 50)

			type learningsBody struct {
				Data struct {
					Scope struct {
						MeasuredPosts int `json:"measured_posts"`
					} `json:"scope"`
					Heatmap struct {
						InsufficientHistory bool `json:"insufficient_history"`
					} `json:"heatmap"`
				} `json:"data"`
			}
			var all, ig learningsBody
			decode(get("/api/analytics/learnings"), &all)
			decode(get("/api/analytics/learnings?platform=instagram"), &ig)
			Expect(all.Data.Scope.MeasuredPosts).To(Equal(7))
			Expect(all.Data.Heatmap.InsufficientHistory).To(BeFalse())
			Expect(ig.Data.Scope.MeasuredPosts).To(Equal(1))
			Expect(ig.Data.Heatmap.InsufficientHistory).To(BeTrue())
		})
	})

	Describe("account labels on /performers and /analytics/posts/:id", func() {
		seedLabelledAccount := func(id, username, displayName, avatar string, deleted bool) {
			now := time.Now().UTC()
			a := &models.SocialAccount{
				ID: id, Platform: "linkedin", ProfileID: "prof-1",
				Username: username, DisplayName: displayName, AvatarURL: avatar,
				IsActive: true, RawJSON: "{}", ConnectedAt: now, LastSyncedAt: now,
			}
			if deleted {
				a.DeletedAt = &now
			}
			_, err := db.NewInsert().Model(a).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
		}

		seedOwnedSnapshot := func(postID, pubPostID, accountID, username string, published time.Time) {
			now := time.Now().UTC()
			Expect(analyticsRepo.Upsert(tenantCtx(), &models.PostAnalytics{
				PostID:          postID,
				PublisherPostID: pubPostID,
				Publisher:       models.PublisherZernio,
				Platform:        "LinkedIn",
				Impressions:     500,
				Reach:           400,
				Likes:           20,
				PublishedAt:     &published,
				PlatformAnalytics: models.PlatformAnalyticsList{{
					Platform: "linkedin", SyncStatus: "synced",
					AccountID: accountID, AccountUsername: username,
				}},
				SyncStatus:    "synced",
				FirstSeenAt:   now,
				LastChangedAt: now,
				LastCheckedAt: now,
			})).To(Succeed())
		}

		type account struct {
			ID          string `json:"id"`
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			AvatarURL   string `json:"avatar_url"`
		}

		It("labels performers rows from social_accounts, keeping disconnected accounts and falling back on a missing row", func() {
			published := time.Now().UTC().Add(-48 * time.Hour)
			seedLabelledAccount("acc-live", "acme", "Acme Inc", "https://cdn/acme.png", false)
			seedLabelledAccount("acc-gone", "oldco", "Old Co", "https://cdn/old.png", true)
			for _, p := range []struct{ id, acc, user string }{
				{"p-live", "acc-live", "acme"},
				{"p-gone", "acc-gone", "oldco"},
				{"p-unknown", "acc-unknown", "ghost"},
			} {
				seedPost(p.id, models.PublisherZernio, "z-"+p.id, published)
				seedOwnedSnapshot(p.id, "z-"+p.id, p.acc, p.user, published)
			}

			resp := get("/api/analytics/performers?window=7d&by=reach&limit=10")
			Expect(resp.StatusCode).To(Equal(200))
			var body struct {
				Available bool `json:"available"`
				Data      struct {
					Best []struct {
						PostID  string  `json:"post_id"`
						Account account `json:"account"`
					} `json:"best"`
				} `json:"data"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body.Available).To(BeTrue())
			got := map[string]account{}
			for _, r := range body.Data.Best {
				got[r.PostID] = r.Account
			}
			Expect(got).To(HaveKeyWithValue("p-live", account{ID: "acc-live", Username: "acme", DisplayName: "Acme Inc", AvatarURL: "https://cdn/acme.png"}))
			Expect(got).To(HaveKeyWithValue("p-gone", account{ID: "acc-gone", Username: "oldco", DisplayName: "Old Co", AvatarURL: "https://cdn/old.png"}))
			Expect(got).To(HaveKeyWithValue("p-unknown", account{ID: "acc-unknown", Username: "ghost", DisplayName: "ghost"}))
		})

		It("labels the post-statistics header from social_accounts", func() {
			published := time.Now().UTC().Add(-48 * time.Hour)
			seedLabelledAccount("acc-live", "acme", "Acme Inc", "https://cdn/acme.png", false)
			seedPost("p-stat", models.PublisherZernio, "z-stat", published)
			seedOwnedSnapshot("p-stat", "z-stat", "acc-live", "acme", published)

			resp := get("/api/analytics/posts/p-stat")
			Expect(resp.StatusCode).To(Equal(200))
			var body struct {
				Data struct {
					Post struct {
						Account account `json:"account"`
					} `json:"post"`
				} `json:"data"`
			}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body.Data.Post.Account).To(Equal(account{ID: "acc-live", Username: "acme", DisplayName: "Acme Inc", AvatarURL: "https://cdn/acme.png"}))
		})
	})

	Describe("campaign filter on /overview, /performers, /learnings", func() {
		const instagramSqid = "rzgpTkARLH0L"

		createCampaign := func(name string) string {
			body, _ := json.Marshal(fiber.Map{"name": name, "campaign_type_id": "Uk"})
			req := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
			var c models.Campaign
			Expect(json.NewDecoder(resp.Body).Decode(&c)).To(Succeed())
			return c.ID
		}

		// seedIn publishes one Zernio post in the given campaign and platform and
		// writes its current analytics row.
		seedIn := func(id, campaign, platformID, displayName string, reach int) {
			published := time.Now().UTC().Add(-48 * time.Hour)
			p := &models.Post{
				ID: id, CampaignID: campaign, PlatformID: platformID,
				Title: "Post " + id, Content: "content " + id,
				MediaURLs: models.StringSlice{}, UsedAssetIDs: models.StringSlice{},
				Status: models.PostStatusPublished, Publisher: models.PublisherZernio,
				PublisherPostID: "z-" + id, CTAType: models.CTATypeNone,
				CreatedBy: userID, PublishedAt: &published,
			}
			Expect(postRepo.Create(tenantCtx(), p)).To(Succeed())
			now := time.Now().UTC()
			Expect(analyticsRepo.Upsert(tenantCtx(), &models.PostAnalytics{
				PostID: id, PublisherPostID: "z-" + id, Publisher: models.PublisherZernio,
				Platform: displayName, PublishedAt: &published, Reach: reach, Impressions: reach,
				SyncStatus: "synced", FirstSeenAt: now, LastChangedAt: now, LastCheckedAt: now,
			})).To(Succeed())
		}

		decode := func(resp *http.Response, into any) {
			Expect(resp.StatusCode).To(Equal(200))
			Expect(json.NewDecoder(resp.Body).Decode(into)).To(Succeed())
		}

		type performersBody struct {
			Data struct {
				TotalPosts int `json:"total_posts"`
			} `json:"data"`
		}
		type overviewBody struct {
			Data struct {
				Cards []struct {
					Metric string  `json:"metric"`
					Value  float64 `json:"value"`
				} `json:"cards"`
				FollowersScope *string `json:"followers_scope"`
			} `json:"data"`
		}
		cardValues := func(b overviewBody) map[string]float64 {
			out := map[string]float64{}
			for _, c := range b.Data.Cards {
				out[c.Metric] = c.Value
			}
			return out
		}

		var other string
		BeforeEach(func() {
			other = createCampaign("Other Campaign")
			seedIn("a-li", campaignID, linkedinSqid, "LinkedIn", 100)
			seedIn("a-ig", campaignID, instagramSqid, "Instagram", 200)
			seedIn("b-li", other, linkedinSqid, "LinkedIn", 400)
		})

		It("narrows /performers to the campaign's posts", func() {
			var all, a, b performersBody
			decode(get("/api/analytics/performers"), &all)
			decode(get("/api/analytics/performers?campaign_id="+campaignID), &a)
			decode(get("/api/analytics/performers?campaign_id="+other), &b)
			Expect(all.Data.TotalPosts).To(Equal(3))
			Expect(a.Data.TotalPosts).To(Equal(2))
			Expect(b.Data.TotalPosts).To(Equal(1))
		})

		It("narrows /overview reach and posts published, and marks followers as workspace-wide", func() {
			var all, a overviewBody
			decode(get("/api/analytics/overview"), &all)
			decode(get("/api/analytics/overview?campaign_id="+campaignID), &a)
			Expect(cardValues(all)["reach"]).To(Equal(700.0))
			Expect(all.Data.FollowersScope).To(BeNil())
			Expect(cardValues(a)["reach"]).To(Equal(300.0))
			Expect(cardValues(a)["posts_published"]).To(Equal(2.0))
			Expect(a.Data.FollowersScope).To(HaveValue(Equal("workspace")))
		})

		It("combines with the platform filter", func() {
			var body performersBody
			decode(get("/api/analytics/performers?platform=linkedin&campaign_id="+campaignID), &body)
			Expect(body.Data.TotalPosts).To(Equal(1))
		})

		It("reports no_data for a campaign with nothing measured", func() {
			empty := createCampaign("Empty Campaign")
			var body struct {
				Available bool   `json:"available"`
				Reason    string `json:"reason"`
			}
			decode(get("/api/analytics/performers?campaign_id="+empty), &body)
			Expect(body.Available).To(BeFalse())
			Expect(body.Reason).To(Equal("no_data"))
		})

		It("404s an unknown or foreign campaign", func() {
			foreignTenant := "tn-con288-foreign"
			ctx := context.Background()
			_, err := db.NewInsert().Model(&models.Tenant{
				ID: foreignTenant, Name: foreignTenant, Slug: foreignTenant, TierID: models.DefaultTierID,
			}).On("CONFLICT (id) DO NOTHING").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
			foreignID, err := models.NewID()
			Expect(err).NotTo(HaveOccurred())
			_, err = db.NewInsert().Model(&models.Campaign{
				ID: foreignID, TenantScoped: models.TenantScoped{TenantID: foreignTenant},
				Name: "Theirs", CampaignTypeID: "Uk", CreatedBy: userID,
			}).Exec(tenantctx.With(ctx, foreignTenant))
			Expect(err).NotTo(HaveOccurred())

			for _, path := range []string{"/api/analytics/overview", "/api/analytics/performers"} {
				Expect(get(path+"?campaign_id=nope").StatusCode).To(Equal(404), path)
				Expect(get(path+"?campaign_id="+foreignID).StatusCode).To(Equal(404), path)
			}
		})

		It("rejects campaign_id on /learnings", func() {
			resp := get("/api/analytics/learnings?campaign_id=" + campaignID)
			Expect(resp.StatusCode).To(Equal(400))
			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["error"]).To(Equal("campaign_scope_unsupported"))
		})

		It("treats a blank campaign_id as no filter on every endpoint", func() {
			for _, path := range []string{"/api/analytics/overview", "/api/analytics/performers", "/api/analytics/learnings"} {
				Expect(get(path+"?campaign_id=%20").StatusCode).To(Equal(200), path)
			}
		})
	})

	Describe("POST /api/posts/:id/verify-external", func() {
		var (
			stub      *httptest.Server
			responder http.HandlerFunc
		)

		seedAccount := func(id, platform, profileID string) {
			_, err := db.NewInsert().Model(&models.SocialAccount{
				ID: id, Platform: platform, ProfileID: profileID,
				Username: "acme", DisplayName: "Acme", AvatarURL: "",
				IsActive: true, RawJSON: "{}",
				ConnectedAt: time.Now().UTC(), LastSyncedAt: time.Now().UTC(),
			}).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
		}

		post := func(path, body string) *http.Response {
			req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
			req.Header.Set("Content-Type", "application/json")
			req.AddCookie(authCookie)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			return resp
		}

		BeforeEach(func() {
			// Default responder: a found post. Individual specs may override.
			responder = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"synced":{"postsFound":1,"postsSynced":1,"skipped":false},"found":true,"post":{"platform":"linkedin","platformPostId":"LI-999","platformPostUrl":"https://li/999","content":"hi","publishedAt":"2026-07-30T10:00:00Z","analytics":{"likes":12,"comments":3,"reach":0,"impressions":0,"engagementRate":0.0,"lastUpdated":"2026-07-30T11:00:00Z"}}}`))
			}
			stub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/posts/sync-external" {
					responder(w, r)
					return
				}
				http.NotFound(w, r)
			}))
			client := zernio.NewClient(zernio.StaticKey("k"), stub.URL, zernio.ClientOpts{Timeout: 5 * time.Second})
			handlers.NewPostVerificationHandler(postRepo, client, socialAccountRepo,
				func(ctx context.Context) (string, error) { return "prof-1", nil },
				analyticsRepo, repository.NewPostVersionRepository(db), nil, auth).Register(app)
		})

		AfterEach(func() { stub.Close() })

		It("confirms a manual post, back-fills publisher_post_id, and writes a snapshot", func() {
			seedAccount("acc-li", "linkedin", "prof-1")
			seedPost("p-manual", "", "", time.Now().UTC())

			resp := post("/api/posts/p-manual/verify-external", `{"url":"https://li/999"}`)
			Expect(resp.StatusCode).To(Equal(200))
			var out map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			Expect(out["found"]).To(Equal(true))
			p := out["post"].(map[string]any)
			Expect(p["publisher_post_id"]).To(Equal("LI-999"))
			Expect(p["published_url"]).To(Equal("https://li/999"))

			// The canonical permalink is persisted as a first-class
			// field on the post, readable without an analytics round-trip.
			pResp := get("/api/posts/p-manual")
			Expect(pResp.StatusCode).To(Equal(200))
			var pj map[string]any
			Expect(json.NewDecoder(pResp.Body).Decode(&pj)).To(Succeed())
			Expect(pj["published_url"]).To(Equal("https://li/999"))

			// The per-post analytics endpoint now returns the back-filled snapshot.
			aResp := get("/api/posts/p-manual/analytics")
			Expect(aResp.StatusCode).To(Equal(200))
			var a map[string]any
			Expect(json.NewDecoder(aResp.Body).Decode(&a)).To(Succeed())
			Expect(a["publisher_post_id"]).To(Equal("LI-999"))
		})

		It("returns found:false when the platform has no such post (404 upstream)", func() {
			seedAccount("acc-li", "linkedin", "prof-1")
			seedPost("p-missing", "", "", time.Now().UTC())
			responder = func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"Post not found"}`))
			}
			resp := post("/api/posts/p-missing/verify-external", `{"url":"https://li/nope"}`)
			Expect(resp.StatusCode).To(Equal(200))
			var out map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			Expect(out["found"]).To(Equal(false))
		})

		It("returns 409 no_account_connected when the platform has no connected account", func() {
			seedPost("p-noacct", "", "", time.Now().UTC()) // no account seeded
			resp := post("/api/posts/p-noacct/verify-external", `{"url":"https://li/999"}`)
			Expect(resp.StatusCode).To(Equal(409))
			var out map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
			Expect(out["error"]).To(Equal("no_account_connected"))
		})

		It("rejects a body with neither url nor post_id (400)", func() {
			seedPost("p-empty", "", "", time.Now().UTC())
			Expect(post("/api/posts/p-empty/verify-external", `{}`).StatusCode).To(Equal(400))
		})
	})
})
