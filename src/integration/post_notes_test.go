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
	"github.com/ogen-app/ogen/src/usecase/notes"
)

var _ = Describe("Post notes CRUD (CON-188)", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		campaignID string
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
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		postRepo := repository.NewPostRepository(db)
		postVersionRepo := repository.NewPostVersionRepository(db)
		postAttRepo := repository.NewPostAttachmentRepository(db)
		noteSvc := notes.New(repository.NewPostNoteRepository(db))
		auth := handlers.RequireAuth(sessionRepo, userRepo, "test_session")

		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth, nil, nil).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, "test_session", false, nil).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil, handlers.CampaignsOptions{}).Register(app)
		handlers.NewPostsHandler(postRepo, postVersionRepo, repository.NewPlatformRepository(db), postAttRepo, auth, handlers.PostsOptions{}).Register(app)
		handlers.NewPostNotesHandler(noteSvc, postRepo, auth, nil).Register(app)

		seedTenantUser(db, "Admin", "notes-it@example.com", "it-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "notes-it@example.com", "password": "it-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		authCookie = loginResp.Cookies()[0]

		cBody, _ := json.Marshal(fiber.Map{"name": "Notes Campaign", "campaign_type_id": "Uk"})
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
		_, _ = db.NewDelete().TableExpr("post_notes").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_attachments").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_versions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("post_assistant_messages").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("posts").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("campaigns").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(ctx)
		_, _ = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(ctx)
	})

	createPost := func() string {
		body, _ := json.Marshal(fiber.Map{
			"campaign_id":        campaignID,
			"platform_id":        linkedinPlatformID,
			"platform_post_type": "article",
			"title":              "Notes Post",
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

	createNote := func(postID, typ, title, body string) (*models.PostNote, int) {
		payload := fiber.Map{"type": typ, "body": body}
		if title != "" {
			payload["title"] = title
		}
		b, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/api/posts/"+postID+"/notes", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		if resp.StatusCode != fiber.StatusCreated {
			return nil, resp.StatusCode
		}
		var n models.PostNote
		Expect(json.NewDecoder(resp.Body).Decode(&n)).To(Succeed())
		return &n, resp.StatusCode
	}

	It("#1 creates a note and reads it back", func() {
		postID := createPost()
		n, code := createNote(postID, "note", "Fact-check", "Verify the figure.")
		Expect(code).To(Equal(fiber.StatusCreated))
		Expect(n.ID).NotTo(BeEmpty())
		Expect(n.PostID).To(Equal(postID))
		Expect(n.Type).To(Equal(models.PostNoteTypeNote))
		Expect(n.Title).To(Equal("Fact-check"))
		Expect(n.Body).To(Equal("Verify the figure."))
		Expect(n.Origin).To(Equal(models.PostNoteOriginManual))
		Expect(n.CreatedBy).NotTo(BeEmpty())

		req := httptest.NewRequest("GET", "/api/posts/"+postID+"/notes/"+n.ID, nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var got models.PostNote
		Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
		Expect(got.ID).To(Equal(n.ID))
	})

	It("#2 lists notes draft_thesis-first then oldest-first", func() {
		postID := createPost()
		// Created in an order that proves both rules: a plain note, then an
		// image prompt, then a draft_thesis created LAST — it must still sort
		// to the top; the other two keep created_at order.
		_, c1 := createNote(postID, "note", "", "first note")
		Expect(c1).To(Equal(fiber.StatusCreated))
		time.Sleep(5 * time.Millisecond)
		_, c2 := createNote(postID, "image_prompt", "", "an image prompt")
		Expect(c2).To(Equal(fiber.StatusCreated))
		time.Sleep(5 * time.Millisecond)
		_, c3 := createNote(postID, "draft_thesis", "Draft thesis", "- point 1\n- point 2")
		Expect(c3).To(Equal(fiber.StatusCreated))

		req := httptest.NewRequest("GET", "/api/posts/"+postID+"/notes", nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var list []models.PostNote
		Expect(json.NewDecoder(resp.Body).Decode(&list)).To(Succeed())
		Expect(list).To(HaveLen(3))
		Expect(list[0].Type).To(Equal(models.PostNoteTypeDraftThesis))
		Expect(list[1].Body).To(Equal("first note"))
		Expect(list[2].Body).To(Equal("an image prompt"))
	})

	It("#3 updates a note (body + type) via PATCH", func() {
		postID := createPost()
		n, _ := createNote(postID, "note", "", "original")

		patch, _ := json.Marshal(fiber.Map{"body": "updated body", "type": "image_prompt"})
		req := httptest.NewRequest("PATCH", "/api/posts/"+postID+"/notes/"+n.ID, bytes.NewReader(patch))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var got models.PostNote
		Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
		Expect(got.Body).To(Equal("updated body"))
		Expect(got.Type).To(Equal(models.PostNoteTypeImagePrompt))
	})

	It("#4 deletes a note; it is then gone", func() {
		postID := createPost()
		n, _ := createNote(postID, "note", "", "to delete")

		delReq := httptest.NewRequest("DELETE", "/api/posts/"+postID+"/notes/"+n.ID, nil)
		delReq.AddCookie(authCookie)
		delResp, err := app.Test(delReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(delResp.StatusCode).To(Equal(fiber.StatusNoContent))

		getReq := httptest.NewRequest("GET", "/api/posts/"+postID+"/notes/"+n.ID, nil)
		getReq.AddCookie(authCookie)
		getResp, err := app.Test(getReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(getResp.StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("#5 rejects an empty body and an invalid type with 400", func() {
		postID := createPost()
		_, code := createNote(postID, "note", "title only", "")
		Expect(code).To(Equal(fiber.StatusBadRequest))
		_, code = createNote(postID, "nonsense", "", "some body")
		Expect(code).To(Equal(fiber.StatusBadRequest))
	})

	It("#6 does not expose a note through a different post (404)", func() {
		postA := createPost()
		postB := createPost()
		n, _ := createNote(postA, "note", "", "belongs to A")

		req := httptest.NewRequest("GET", "/api/posts/"+postB+"/notes/"+n.ID, nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("#7 returns an empty array for a post with no notes", func() {
		postID := createPost()
		req := httptest.NewRequest("GET", "/api/posts/"+postID+"/notes", nil)
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var list []models.PostNote
		Expect(json.NewDecoder(resp.Body).Decode(&list)).To(Succeed())
		Expect(list).To(BeEmpty())
	})
})
