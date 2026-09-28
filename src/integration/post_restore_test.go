//go:build integration

package integration_test

import (
	"bytes"
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
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
)

// restoreResp mirrors the JSON body returned by POST /api/posts/:id/restore.
type restoreResp struct {
	Post                models.Post `json:"post"`
	RestoredFromVersion int         `json:"restored_from_version"`
	NewVersionNumber    int         `json:"new_version_number"`
	AutoSnapshotCreated bool        `json:"auto_snapshot_created"`
	NoOp                bool        `json:"no_op"`
}

var _ = Describe("Post restore — CON-68", Ordered, func() {
	var (
		app         *fiber.App
		db          *bun.DB
		authCookie  *http.Cookie
		campaignID  string
		postRepo    repository.PostRepository
		versionRepo repository.PostVersionRepository
		logRepo     repository.PostLogRepository
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
		versionRepo = repository.NewPostVersionRepository(db)
		logRepo = repository.NewPostLogRepository(db)
		postAttRepo := repository.NewPostAttachmentRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, "test_session")

		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth, nil, nil).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, "test_session", false, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)

		postsHandler := handlers.NewPostsHandler(postRepo, versionRepo, platformRepo, postAttRepo, auth, handlers.PostsOptions{PostLogs: logRepo})
		postsHandler.Register(app)
		// The restore action now lives on the actions handler.
		handlers.NewPostActionsHandler(postRepo, nil, restore.New(db, postRepo, versionRepo, logRepo, nil), nil, auth).Register(app)

		// Seed user + session + campaign.
		seedTenantUser(db, "Admin", "restore@example.com", "restore-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "restore@example.com", "password": "restore-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		Expect(len(loginResp.Cookies())).To(BeNumerically(">", 0))
		authCookie = loginResp.Cookies()[0]

		cBody, _ := json.Marshal(fiber.Map{"name": "Restore Campaign", "campaign_type_id": "Uk"})
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
		for _, t := range []string{"post_versions", "post_logs", "post_assistant_messages", "posts", "campaigns", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(t).Where("1 = 1").Exec(ctx)
		}
	})

	// createDraft makes a platform-less draft with the given content.
	createDraft := func(content string) string {
		body, _ := json.Marshal(fiber.Map{
			"campaign_id": campaignID,
			"title":       "Restore Source",
			"content":     content,
			"cta_type":    "none",
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

	// updateContent replaces the draft's content (status stays draft).
	updateContent := func(id, content string) {
		body, _ := json.Marshal(fiber.Map{
			"campaign_id": campaignID,
			"title":       "Restore Source",
			"content":     content,
			"status":      "draft",
			"cta_type":    "none",
		})
		req := httptest.NewRequest("PUT", "/api/posts/"+id, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
	}

	// snapshot manually saves the current content as a new version.
	snapshot := func(id, note string) {
		body, _ := json.Marshal(fiber.Map{"note": note})
		req := httptest.NewRequest("POST", "/api/posts/"+id+"/versions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
	}

	restore := func(id string, versionNumber int) (*http.Response, restoreResp) {
		body, _ := json.Marshal(fiber.Map{"version_number": versionNumber})
		req := httptest.NewRequest("POST", "/api/posts/"+id+"/restore", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		var out restoreResp
		if resp.StatusCode == fiber.StatusOK {
			Expect(json.NewDecoder(resp.Body).Decode(&out)).To(Succeed())
		}
		return resp, out
	}

	// seedThreeVersions builds v1=A, v2=B, v3=C with the post left on C.
	seedThreeVersions := func() string {
		id := createDraft("A")
		snapshot(id, "v1")
		updateContent(id, "B")
		snapshot(id, "v2")
		updateContent(id, "C")
		snapshot(id, "v3")
		return id
	}

	It("#1 restores an earlier version by appending a new version (non-destructive)", func() {
		id := seedThreeVersions()

		resp, out := restore(id, 1)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(out.NoOp).To(BeFalse())
		Expect(out.AutoSnapshotCreated).To(BeFalse()) // HEAD (C) == v3, not dirty
		Expect(out.RestoredFromVersion).To(Equal(1))
		Expect(out.NewVersionNumber).To(Equal(4))
		Expect(out.Post.Content).To(Equal("A"))

		ctx := tenantCtx()
		versions, err := versionRepo.ListByPostID(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(versions).To(HaveLen(4))
		// Originals are untouched.
		Expect(versions[0].Content).To(Equal("A"))
		Expect(versions[1].Content).To(Equal("B"))
		Expect(versions[2].Content).To(Equal("C"))
		// The restore version is a fresh append.
		Expect(versions[3].VersionNumber).To(Equal(4))
		Expect(versions[3].Content).To(Equal("A"))
		Expect(versions[3].Note).To(Equal("Restored from v1"))
		Expect(versions[3].Creator).To(Equal("user"))

		// Live post reflects the restored content.
		live, err := postRepo.GetByID(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(live.Content).To(Equal("A"))

		// Audit entry recorded.
		logs, err := logRepo.ListByPostID(ctx, id, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).To(ContainElement(WithTransform(
			func(l models.PostLog) models.PostLogEventType { return l.EventType },
			Equal(models.PostLogEventPostRestored),
		)))
	})

	It("#2 auto-snapshots a dirty HEAD before restoring so nothing is lost", func() {
		id := seedThreeVersions()
		// Dirty the HEAD: change content without snapshotting (latest
		// version is v3=C, live content becomes D).
		updateContent(id, "D")

		resp, out := restore(id, 2)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(out.AutoSnapshotCreated).To(BeTrue())
		Expect(out.RestoredFromVersion).To(Equal(2))
		Expect(out.NewVersionNumber).To(Equal(5)) // v4 = auto-snapshot, v5 = restore
		Expect(out.Post.Content).To(Equal("B"))

		ctx := tenantCtx()
		versions, err := versionRepo.ListByPostID(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(versions).To(HaveLen(5))
		Expect(versions[3].Content).To(Equal("D")) // the rescued dirty HEAD
		Expect(versions[3].Note).To(Equal("Auto-saved before restore"))
		Expect(versions[4].Content).To(Equal("B"))
		Expect(versions[4].Note).To(Equal("Restored from v2"))
	})

	It("#3 is a no-op when the target already matches the current content", func() {
		id := seedThreeVersions() // HEAD == C == v3

		resp, out := restore(id, 3)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		Expect(out.NoOp).To(BeTrue())
		Expect(out.NewVersionNumber).To(Equal(0))

		ctx := tenantCtx()
		versions, err := versionRepo.ListByPostID(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(versions).To(HaveLen(3)) // nothing appended
	})

	It("#4 returns 404 for a nonexistent version number", func() {
		id := seedThreeVersions()
		resp, _ := restore(id, 99)
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("#5 returns 400 when version_number is missing or non-positive", func() {
		id := seedThreeVersions()
		body, _ := json.Marshal(fiber.Map{})
		req := httptest.NewRequest("POST", "/api/posts/"+id+"/restore", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("#6 returns 404 for an unknown post", func() {
		resp, _ := restore("doesnotexist", 1)
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("#7 refuses to restore a post that is not in an editable state", func() {
		id := seedThreeVersions()
		// Force a non-editable status directly (the publish pipeline is out
		// of scope here) — restore must not silently rewrite live content.
		ctx := tenantCtx()
		_, err := db.NewUpdate().
			Model((*models.Post)(nil)).
			Set("status = ?", string(models.PostStatusPublished)).
			Where("id = ?", id).
			Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		resp, _ := restore(id, 1)
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))

		// Content unchanged.
		live, err := postRepo.GetByID(ctx, id)
		Expect(err).NotTo(HaveOccurred())
		Expect(live.Content).To(Equal("C"))
	})
})
