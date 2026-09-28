//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// scheduleResp mirrors the JSON body returned by POST /api/posts/:id/schedule.
type scheduleResp struct {
	Post        models.Post `json:"post"`
	Status      string      `json:"status"`
	AutoPublish bool        `json:"auto_publish"`
	Promoted    bool        `json:"promoted"`
}

var _ = Describe("Post schedule — CON-78", Ordered, func() {
	const (
		linkedinSqid  = "AXqWG7U2qnpt" // seeded LinkedIn (Zernio: linkedin)
		instagramSqid = "rzgpTkARLH0L" // seeded Instagram (Zernio: instagram)
	)

	var (
		app           *fiber.App
		db            *bun.DB
		authCookie    *http.Cookie
		campaignID    string
		postRepo      repository.PostRepository
		logRepo       repository.PostLogRepository
		allowlistRepo repository.AutoPublishAllowlistRepository
	)

	BeforeAll(func() {
		db = mustOpenIntegrationDB()
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
		platformRepo := repository.NewPlatformRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo)
		postRepo = repository.NewPostRepository(db)
		versionRepo := repository.NewPostVersionRepository(db)
		logRepo = repository.NewPostLogRepository(db)
		postAttRepo := repository.NewPostAttachmentRepository(db)
		allowlistRepo = repository.NewAutoPublishAllowlistRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, "test_session")

		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, "test_session", false).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(app)

		postsHandler := handlers.NewPostsHandler(postRepo, versionRepo, platformRepo, postAttRepo, auth)
		postsHandler.SetPostLogRepo(logRepo)
		// Real schedule service over a real allowlist. No jobs
		// client wired — the auto-publish routing decision is still
		// exercised; the Zernio submit enqueue is out of scope here
		// (consistent with the rest of the suite, which doesn't run the River worker pool).
		postsHandler.SetScheduleService(schedule.New(db, postRepo, platformRepo, postAttRepo, allowlistRepo, logRepo, nil, nil))
		postsHandler.Register(app)

		// Seed user + session + campaign.
		seedTenantUser(db, "Admin", "schedule@example.com", "schedule-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "schedule@example.com", "password": "schedule-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]

		cBody, _ := json.Marshal(fiber.Map{"name": "Schedule Campaign", "campaign_type_id": "Uk"})
		cReq := httptest.NewRequest("POST", "/api/campaigns", bytes.NewReader(cBody))
		cReq.Header.Set("Content-Type", "application/json")
		cReq.AddCookie(authCookie)
		cResp, err := app.Test(cReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(cResp.StatusCode).To(Equal(fiber.StatusCreated))
		var camp models.Campaign
		Expect(json.NewDecoder(cResp.Body).Decode(&camp)).To(Succeed())
		campaignID = camp.ID
	})

	AfterEach(func() {
		ctx := tenantCtx()
		for _, t := range []string{"auto_publish_allowlist", "post_versions", "post_logs", "post_assistant_messages", "posts", "campaigns", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(t).Where("1 = 1").Exec(ctx)
		}
	})

	// createPost makes a post with the given platform/type/status.
	createPost := func(platformID, postType, status string) string {
		body, _ := json.Marshal(fiber.Map{
			"campaign_id":        campaignID,
			"platform_id":        platformID,
			"platform_post_type": postType,
			"title":              "Schedule Source",
			"content":            "Ready to ship.",
			"status":             status,
			"cta_type":           "none",
		})
		req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var p models.Post
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())
		return p.ID
	}

	schedule := func(id string, scheduledAt *time.Time, allowPromote bool) (*http.Response, scheduleResp) {
		payload := fiber.Map{"allow_promote": allowPromote}
		if scheduledAt != nil {
			payload["scheduled_at"] = scheduledAt.Format(time.RFC3339)
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/posts/"+id+"/schedule", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		var out scheduleResp
		if resp.StatusCode == fiber.StatusOK {
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		}
		return resp, out
	}

	future := func() *time.Time {
		t := time.Now().Add(48 * time.Hour).UTC()
		return &t
	}

	It("#1 schedules a ready post on a non-allowlisted platform for manual publishing", func() {
		id := createPost(linkedinSqid, "text-post", "ready_for_publish")
		when := future()

		resp, out := schedule(id, when, false)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(out.Status).To(Equal(string(models.PostStatusScheduledForManualPublish)))
		Expect(out.AutoPublish).To(BeFalse())
		Expect(out.Promoted).To(BeFalse())
		Expect(out.Post.ScheduledAt).NotTo(BeNil())
		Expect(out.Post.ScheduledAt.UTC().Unix()).To(Equal(when.Unix()))

		// user_schedule action-log recorded.
		logs, err := logRepo.ListByPostID(tenantCtx(), id, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainElement(WithTransform(
			func(l models.PostLog) models.PostLogEventType { return l.EventType },
			Equal(models.PostLogEventUserSchedule),
		)))
	})

	It("#2 routes an allowlisted platform to auto-publish (scheduled)", func() {
		Expect(allowlistRepo.Set(tenantCtx(), []string{"linkedin"})).To(Succeed())
		id := createPost(linkedinSqid, "text-post", "ready_for_publish")

		resp, out := schedule(id, future(), false)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(out.Status).To(Equal(string(models.PostStatusScheduled)))
		Expect(out.AutoPublish).To(BeTrue())
	})

	It("#3 auto-promotes a valid draft before scheduling", func() {
		id := createPost(linkedinSqid, "text-post", "draft")

		resp, out := schedule(id, future(), true)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(out.Promoted).To(BeTrue())
		Expect(out.Status).To(Equal(string(models.PostStatusScheduledForManualPublish)))

		live, err := postRepo.GetByID(tenantCtx(), id)
		Expect(err).NotTo(HaveOccurred())
		Expect(live.Status).To(Equal(models.PostStatusScheduledForManualPublish))
	})

	It("#4 returns 422 when promoting a draft that fails pre-publish validation", func() {
		// image-post requires at least one image attachment; this draft has none.
		id := createPost(instagramSqid, "image-post", "draft")

		resp, _ := schedule(id, future(), true)
		Expect(resp.StatusCode).To(Equal(fiber.StatusUnprocessableEntity))

		// No state change — still a draft.
		live, err := postRepo.GetByID(tenantCtx(), id)
		Expect(err).NotTo(HaveOccurred())
		Expect(live.Status).To(Equal(models.PostStatusDraft))
	})

	It("#5 returns 400 when scheduling a draft without allow_promote", func() {
		id := createPost(linkedinSqid, "text-post", "draft")
		resp, _ := schedule(id, future(), false)
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("#6 rejects a scheduled_at in the past", func() {
		id := createPost(linkedinSqid, "text-post", "ready_for_publish")
		past := time.Now().Add(-1 * time.Hour).UTC()
		resp, _ := schedule(id, &past, false)
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("#7 returns 400 when scheduled_at is missing", func() {
		id := createPost(linkedinSqid, "text-post", "ready_for_publish")
		resp, _ := schedule(id, nil, false)
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("#8 returns 404 for an unknown post", func() {
		resp, _ := schedule("doesnotexist", future(), false)
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("#9 refuses to reschedule an already-scheduled post", func() {
		id := createPost(linkedinSqid, "text-post", "ready_for_publish")
		resp, _ := schedule(id, future(), false)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))

		// Second schedule attempt — the post is no longer ready_for_publish.
		resp2, _ := schedule(id, future(), false)
		Expect(resp2.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("#10 returns 400 for a post with no platform set", func() {
		// A platform-less draft can't be scheduled — no allowlist/validation.
		body, _ := json.Marshal(fiber.Map{
			"campaign_id": campaignID,
			"title":       "No Platform",
			"content":     "draft",
			"cta_type":    "none",
		})
		req := httptest.NewRequest("POST", "/api/posts", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		var p models.Post
		Expect(json.NewDecoder(resp.Body).Decode(&p)).To(Succeed())

		schedResp, _ := schedule(p.ID, future(), true)
		Expect(schedResp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})
})
