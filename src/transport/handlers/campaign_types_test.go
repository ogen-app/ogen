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

// Seeded campaign type Sqids (deterministic: Encode([1..4])).
const (
	ctAwareness  = "Uk"
	ctEngagement = "gb"
	ctConversion = "Ef"
	ctRetention  = "Vq"
)

var _ = Describe("CampaignTypesHandler", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
	)

	BeforeAll(func() {
		db = mustOpenTestDBWithMigrations()
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
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewUsersHandler(db, userRepo, repository.NewAccountRepository(db), settingRepo, auth).Register(app)
		handlers.NewSessionsHandler(userRepo, repository.NewAccountRepository(db), sessionRepo, testCookieName, false).Register(app)
		handlers.NewCampaignTypesHandler(campaignTypeRepo, auth).Register(app)

		seedTenantUser(db, "Admin", "admin@example.com", "admin-password")

		loginBody, _ := json.Marshal(fiber.Map{"email": "admin@example.com", "password": "admin-password"})
		loginReq := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(loginBody))
		loginReq.Header.Set("Content-Type", "application/json")
		loginResp, err := app.Test(loginReq)
		Expect(err).NotTo(HaveOccurred())
		Expect(loginResp.StatusCode).To(Equal(fiber.StatusCreated))
		cookies := loginResp.Cookies()
		Expect(cookies).To(HaveLen(1))
		authCookie = cookies[0]
	})

	AfterEach(func() {
		// Only delete user-created campaign types; seeded system types must survive across tests.
		_, err := db.NewDelete().TableExpr("campaigns_types").Where("is_system = FALSE").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("accounts").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
	})

	// helper: create a user-defined campaign type via the API
	createCampaignType := func(name, label string) models.CampaignType {
		body, _ := json.Marshal(fiber.Map{"name": name, "label": label})
		req := httptest.NewRequest("POST", "/api/campaign_types", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(authCookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var ct models.CampaignType
		Expect(json.NewDecoder(resp.Body).Decode(&ct)).To(Succeed())
		return ct
	}

	// ── List ─────────────────────────────────────────────────────────────────

	Describe("GET /api/campaign_types", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaign_types", nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns all 4 seeded campaign types", func() {
				req := httptest.NewRequest("GET", "/api/campaign_types", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var types []models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&types)).To(Succeed())
				Expect(types).To(HaveLen(5))

				names := make([]string, len(types))
				for i, ct := range types {
					names[i] = ct.Name
				}
				Expect(names).To(ConsistOf("awareness", "engagement", "conversion", "retention", "evergreen"))
			})

			It("returns phases for each campaign type", func() {
				req := httptest.NewRequest("GET", "/api/campaign_types", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())

				var types []models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&types)).To(Succeed())

				byName := make(map[string]models.CampaignType, len(types))
				for _, ct := range types {
					byName[ct.Name] = ct
				}

				Expect(byName["awareness"].Phases).To(HaveLen(2))
				Expect(byName["engagement"].Phases).To(HaveLen(3))
				Expect(byName["conversion"].Phases).To(HaveLen(3))
				Expect(byName["retention"].Phases).To(HaveLen(3))
				Expect(byName["evergreen"].Phases).To(HaveLen(3))
			})

			It("returns phases ordered by sequence", func() {
				req := httptest.NewRequest("GET", "/api/campaign_types", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())

				var types []models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&types)).To(Succeed())

				byName := make(map[string]models.CampaignType, len(types))
				for _, ct := range types {
					byName[ct.Name] = ct
				}

				phases := byName["engagement"].Phases
				Expect(phases[0].Sequence).To(Equal(1))
				Expect(phases[1].Sequence).To(Equal(2))
				Expect(phases[2].Sequence).To(Equal(3))
				Expect(phases[0].Name).To(Equal("Activate"))
			})
		})
	})

	// ── Get ──────────────────────────────────────────────────────────────────

	Describe("GET /api/campaign_types/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("GET", "/api/campaign_types/"+ctAwareness, nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("returns the campaign type with phases", func() {
				req := httptest.NewRequest("GET", "/api/campaign_types/"+ctAwareness, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var ct models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&ct)).To(Succeed())
				Expect(ct.ID).To(Equal(ctAwareness))
				Expect(ct.Name).To(Equal("awareness"))
				Expect(ct.Label).To(Equal("Awareness"))
				Expect(ct.IsSystem).To(BeTrue())
				Expect(ct.Phases).To(HaveLen(2))
				Expect(ct.Phases[0].Name).To(Equal("Launch & Distribution"))
				Expect(ct.Phases[1].Name).To(Equal("Sustain & Optimize"))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("GET", "/api/campaign_types/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Create ───────────────────────────────────────────────────────────────

	Describe("POST /api/campaign_types", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"name": "custom", "label": "Custom"})
				req := httptest.NewRequest("POST", "/api/campaign_types", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("creates a user-defined campaign type and returns 201", func() {
				body, _ := json.Marshal(fiber.Map{
					"name":        "product-launch",
					"label":       "Product Launch",
					"description": "A specialized type for product launches.",
				})
				req := httptest.NewRequest("POST", "/api/campaign_types", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var ct models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&ct)).To(Succeed())
				Expect(ct.ID).NotTo(BeEmpty())
				Expect(ct.Name).To(Equal("product-launch"))
				Expect(ct.Label).To(Equal("Product Launch"))
				Expect(ct.Description).To(Equal("A specialized type for product launches."))
				Expect(ct.IsSystem).To(BeFalse())
				Expect(ct.Phases).To(BeEmpty())
			})

			It("returns 400 when name is missing", func() {
				body, _ := json.Marshal(fiber.Map{"label": "No Name"})
				req := httptest.NewRequest("POST", "/api/campaign_types", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})

			It("returns 400 when label is missing", func() {
				body, _ := json.Marshal(fiber.Map{"name": "no-label"})
				req := httptest.NewRequest("POST", "/api/campaign_types", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Update ───────────────────────────────────────────────────────────────

	Describe("PUT /api/campaign_types/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"name": "updated", "label": "Updated"})
				req := httptest.NewRequest("PUT", "/api/campaign_types/"+ctAwareness, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("updates a user-created campaign type", func() {
				ct := createCampaignType("original", "Original")

				body, _ := json.Marshal(fiber.Map{"name": "updated", "label": "Updated", "description": "New desc"})
				req := httptest.NewRequest("PUT", "/api/campaign_types/"+ct.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Name).To(Equal("updated"))
				Expect(got.Label).To(Equal("Updated"))
				Expect(got.Description).To(Equal("New desc"))
			})

			It("returns 403 when attempting to update a system type", func() {
				body, _ := json.Marshal(fiber.Map{"name": "hacked", "label": "Hacked"})
				req := httptest.NewRequest("PUT", "/api/campaign_types/"+ctAwareness, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(403))
			})

			It("returns 404 for an unknown id", func() {
				body, _ := json.Marshal(fiber.Map{"name": "ghost", "label": "Ghost"})
				req := httptest.NewRequest("PUT", "/api/campaign_types/nonexistent", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Delete ───────────────────────────────────────────────────────────────

	Describe("DELETE /api/campaign_types/:id", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				req := httptest.NewRequest("DELETE", "/api/campaign_types/"+ctAwareness, nil)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("deletes a user-created campaign type and returns 204", func() {
				ct := createCampaignType("to-delete", "To Delete")

				req := httptest.NewRequest("DELETE", "/api/campaign_types/"+ct.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(204))
			})

			It("returns 403 when attempting to delete a system type", func() {
				req := httptest.NewRequest("DELETE", "/api/campaign_types/"+ctEngagement, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(403))
			})

			It("returns 404 for an unknown id", func() {
				req := httptest.NewRequest("DELETE", "/api/campaign_types/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})

	// ── Clone ─────────────────────────────────────────────────────────────────

	Describe("POST /api/campaign_types/:id/clone", func() {
		Context("when not authenticated", func() {
			It("returns 401", func() {
				body, _ := json.Marshal(fiber.Map{"name": "clone", "label": "Clone"})
				req := httptest.NewRequest("POST", "/api/campaign_types/"+ctAwareness+"/clone", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(401))
			})
		})

		Context("when authenticated", func() {
			It("clones a system type with all its phases", func() {
				body, _ := json.Marshal(fiber.Map{"name": "my-awareness", "label": "My Awareness"})
				req := httptest.NewRequest("POST", "/api/campaign_types/"+ctAwareness+"/clone", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var clone models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&clone)).To(Succeed())
				Expect(clone.ID).NotTo(Equal(ctAwareness))
				Expect(clone.Name).To(Equal("my-awareness"))
				Expect(clone.Label).To(Equal("My Awareness"))
				Expect(clone.IsSystem).To(BeFalse())
				Expect(clone.Phases).To(HaveLen(2))
				Expect(clone.Phases[0].Name).To(Equal("Launch & Distribution"))
				Expect(clone.Phases[1].Name).To(Equal("Sustain & Optimize"))
				// Phase IDs must differ from the originals.
				Expect(clone.Phases[0].ID).NotTo(Equal("98"))
				Expect(clone.Phases[0].CampaignTypeID).To(Equal(clone.ID))
			})

			It("clones a user-created type", func() {
				src := createCampaignType("base-type", "Base Type")

				body, _ := json.Marshal(fiber.Map{"name": "derived-type", "label": "Derived Type"})
				req := httptest.NewRequest("POST", "/api/campaign_types/"+src.ID+"/clone", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(201))

				var clone models.CampaignType
				Expect(json.NewDecoder(resp.Body).Decode(&clone)).To(Succeed())
				Expect(clone.Name).To(Equal("derived-type"))
				Expect(clone.IsSystem).To(BeFalse())
				Expect(clone.Phases).To(BeEmpty())
			})

			It("returns 404 for an unknown source id", func() {
				body, _ := json.Marshal(fiber.Map{"name": "ghost", "label": "Ghost"})
				req := httptest.NewRequest("POST", "/api/campaign_types/nonexistent/clone", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})

			It("returns 400 when name is missing", func() {
				body, _ := json.Marshal(fiber.Map{"label": "No Name"})
				req := httptest.NewRequest("POST", "/api/campaign_types/"+ctAwareness+"/clone", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(400))
			})
		})
	})

	// ── Phases ────────────────────────────────────────────────────────────────

	Describe("phase management", func() {
		var customTypeID string

		BeforeEach(func() {
			ct := createCampaignType("custom-type", "Custom Type")
			customTypeID = ct.ID
		})

		Describe("POST /api/campaign_types/:id/phases", func() {
			Context("when not authenticated", func() {
				It("returns 401", func() {
					body, _ := json.Marshal(fiber.Map{"name": "Phase 1", "sequence": 1})
					req := httptest.NewRequest("POST", "/api/campaign_types/"+customTypeID+"/phases", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(401))
				})
			})

			Context("when authenticated", func() {
				It("adds a phase and returns 201", func() {
					body, _ := json.Marshal(fiber.Map{
						"name":     "Kickoff",
						"purpose":  "Get things started.",
						"sequence": 1,
					})
					req := httptest.NewRequest("POST", "/api/campaign_types/"+customTypeID+"/phases", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.AddCookie(authCookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(201))

					var phase models.CampaignTypePhase
					Expect(json.NewDecoder(resp.Body).Decode(&phase)).To(Succeed())
					Expect(phase.ID).NotTo(BeEmpty())
					Expect(phase.Name).To(Equal("Kickoff"))
					Expect(phase.Sequence).To(Equal(1))
					Expect(phase.CampaignTypeID).To(Equal(customTypeID))
				})

				It("returns 400 when name is missing", func() {
					body, _ := json.Marshal(fiber.Map{"sequence": 1})
					req := httptest.NewRequest("POST", "/api/campaign_types/"+customTypeID+"/phases", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.AddCookie(authCookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(400))
				})

				It("returns 404 for an unknown campaign type id", func() {
					body, _ := json.Marshal(fiber.Map{"name": "Phase", "sequence": 1})
					req := httptest.NewRequest("POST", "/api/campaign_types/nonexistent/phases", bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.AddCookie(authCookie)
					resp, err := app.Test(req)
					Expect(err).NotTo(HaveOccurred())
					Expect(resp.StatusCode).To(Equal(404))
				})
			})
		})

		Describe("PUT /api/campaign_types/:id/phases/:phase_id", func() {
			It("updates the phase", func() {
				// Add a phase first.
				addBody, _ := json.Marshal(fiber.Map{"name": "Old Name", "sequence": 1})
				addReq := httptest.NewRequest("POST", "/api/campaign_types/"+customTypeID+"/phases", bytes.NewReader(addBody))
				addReq.Header.Set("Content-Type", "application/json")
				addReq.AddCookie(authCookie)
				addResp, err := app.Test(addReq)
				Expect(err).NotTo(HaveOccurred())
				var phase models.CampaignTypePhase
				Expect(json.NewDecoder(addResp.Body).Decode(&phase)).To(Succeed())

				body, _ := json.Marshal(fiber.Map{"name": "New Name", "purpose": "Updated purpose.", "sequence": 2})
				req := httptest.NewRequest("PUT", "/api/campaign_types/"+customTypeID+"/phases/"+phase.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(200))

				var got models.CampaignTypePhase
				Expect(json.NewDecoder(resp.Body).Decode(&got)).To(Succeed())
				Expect(got.Name).To(Equal("New Name"))
				Expect(got.Purpose).To(Equal("Updated purpose."))
				Expect(got.Sequence).To(Equal(2))
			})

			It("returns 404 when phase belongs to a different campaign type", func() {
				addBody, _ := json.Marshal(fiber.Map{"name": "Phase", "sequence": 1})
				addReq := httptest.NewRequest("POST", "/api/campaign_types/"+customTypeID+"/phases", bytes.NewReader(addBody))
				addReq.Header.Set("Content-Type", "application/json")
				addReq.AddCookie(authCookie)
				addResp, err := app.Test(addReq)
				Expect(err).NotTo(HaveOccurred())
				var phase models.CampaignTypePhase
				Expect(json.NewDecoder(addResp.Body).Decode(&phase)).To(Succeed())

				body, _ := json.Marshal(fiber.Map{"name": "Hijack", "sequence": 1})
				req := httptest.NewRequest("PUT", "/api/campaign_types/"+ctAwareness+"/phases/"+phase.ID, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})

		Describe("DELETE /api/campaign_types/:id/phases/:phase_id", func() {
			It("deletes the phase and returns 204", func() {
				addBody, _ := json.Marshal(fiber.Map{"name": "Temporary", "sequence": 1})
				addReq := httptest.NewRequest("POST", "/api/campaign_types/"+customTypeID+"/phases", bytes.NewReader(addBody))
				addReq.Header.Set("Content-Type", "application/json")
				addReq.AddCookie(authCookie)
				addResp, err := app.Test(addReq)
				Expect(err).NotTo(HaveOccurred())
				var phase models.CampaignTypePhase
				Expect(json.NewDecoder(addResp.Body).Decode(&phase)).To(Succeed())

				req := httptest.NewRequest("DELETE", "/api/campaign_types/"+customTypeID+"/phases/"+phase.ID, nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(204))
			})

			It("returns 404 for an unknown phase id", func() {
				req := httptest.NewRequest("DELETE", "/api/campaign_types/"+customTypeID+"/phases/nonexistent", nil)
				req.AddCookie(authCookie)
				resp, err := app.Test(req)
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.StatusCode).To(Equal(404))
			})
		})
	})
})
