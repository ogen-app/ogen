package handlers_test

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
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// CON-314: a custom campaign type belongs to the workspace that created it.
// Another workspace can't see it, touch it or its phases, or build a campaign
// on it; system types stay shared and read-only.
var _ = Describe("Campaign type workspace isolation (CON-314)", Ordered, func() {
	var (
		app *fiber.App
		db  *bun.DB
	)

	BeforeAll(func() { db = mustOpenTestDBWithMigrations() })

	BeforeEach(func() {
		app = fiber.New(fiber.Config{ErrorHandler: func(c *fiber.Ctx, err error) error {
			code := fiber.StatusInternalServerError
			if e, ok := err.(*fiber.Error); ok {
				code = e.Code
			}
			return c.Status(code).JSON(fiber.Map{"error": err.Error()})
		}})
		userRepo := repository.NewUserRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		sessionRepo := repository.NewSessionRepository(db)
		tagRepo := repository.NewTagRepository(db)
		campaignTypeRepo := repository.NewCampaignTypeRepository(db)
		campaignRepo := repository.NewCampaignRepository(db, tagRepo, repository.NewPlatformRepository(db), campaignTypeRepo)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewTenantsHandler(signup.New(db, repository.NewAccountRepository(db), tenantRepo, nil), tenantRepo, testCookieName, false, auth).Register(app)
		handlers.NewCampaignsHandler(campaignRepo, campaignTypeRepo, auth, nil, nil, nil, nil, nil).Register(app)
		handlers.NewCampaignTypesHandler(campaignTypeRepo, auth).Register(app)
	})

	AfterEach(func() {
		ctx := tenantCtx() // TableExpr deletes bypass hooks; tenant value is ignored
		for _, t := range []string{"campaigns", "sessions", "users", "accounts"} {
			_, _ = db.NewDelete().TableExpr(t).Where("1 = 1").Exec(ctx)
		}
		_, _ = db.NewDelete().TableExpr("campaigns_types").Where("is_system = FALSE").Exec(ctx)
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(ctx)
	})

	signupWorkspace := func(name, email string) *http.Cookie {
		GinkgoHelper()
		body, _ := json.Marshal(fiber.Map{
			"tenant": fiber.Map{"name": name},
			"user":   fiber.Map{"name": "Owner", "email": email, "password": "password-owner"},
		})
		req := httptest.NewRequest("POST", "/api/tenants", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		for _, ck := range resp.Cookies() {
			if ck.Name == testCookieName {
				return ck
			}
		}
		Fail("signup did not set a session cookie")
		return nil
	}

	call := func(method, path string, cookie *http.Cookie, payload fiber.Map) *http.Response {
		GinkgoHelper()
		var body []byte
		if payload != nil {
			body, _ = json.Marshal(payload)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}

	createType := func(cookie *http.Cookie, name string) models.CampaignType {
		GinkgoHelper()
		resp := call("POST", "/api/campaign_types", cookie, fiber.Map{"name": name, "label": "Launch"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var ct models.CampaignType
		Expect(json.NewDecoder(resp.Body).Decode(&ct)).To(Succeed())
		return ct
	}

	addPhase := func(cookie *http.Cookie, typeID string) models.CampaignTypePhase {
		GinkgoHelper()
		resp := call("POST", "/api/campaign_types/"+typeID+"/phases", cookie, fiber.Map{"name": "Tease", "sequence": 1})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var ph models.CampaignTypePhase
		Expect(json.NewDecoder(resp.Body).Decode(&ph)).To(Succeed())
		return ph
	}

	listTypeIDs := func(cookie *http.Cookie) []string {
		GinkgoHelper()
		resp := call("GET", "/api/campaign_types", cookie, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var types []models.CampaignType
		Expect(json.NewDecoder(resp.Body).Decode(&types)).To(Succeed())
		ids := make([]string, len(types))
		for i, t := range types {
			ids[i] = t.ID
		}
		return ids
	}

	It("hides a workspace's custom type from every other workspace's reads", func() {
		a := signupWorkspace("Acme", "a@acme.test")
		b := signupWorkspace("Beta", "b@beta.test")
		own := createType(a, "launch")

		Expect(listTypeIDs(a)).To(ContainElements(own.ID, ctAwareness))
		Expect(listTypeIDs(b)).NotTo(ContainElement(own.ID))
		Expect(listTypeIDs(b)).To(ContainElement(ctAwareness), "system types stay visible to every workspace")

		Expect(call("GET", "/api/campaign_types/"+own.ID, a, nil).StatusCode).To(Equal(fiber.StatusOK))
		Expect(call("GET", "/api/campaign_types/"+own.ID, b, nil).StatusCode).To(Equal(fiber.StatusNotFound))
	})

	It("404s every write another workspace aims at a custom type or its phases", func() {
		a := signupWorkspace("Acme", "a@acme.test")
		b := signupWorkspace("Beta", "b@beta.test")
		own := createType(a, "launch")
		phase := addPhase(a, own.ID)
		base := "/api/campaign_types/" + own.ID

		Expect(call("PUT", base, b, fiber.Map{"name": "stolen", "label": "Stolen"}).StatusCode).To(Equal(fiber.StatusNotFound))
		Expect(call("POST", base+"/clone", b, fiber.Map{"name": "copy", "label": "Copy"}).StatusCode).To(Equal(fiber.StatusNotFound))
		Expect(call("POST", base+"/phases", b, fiber.Map{"name": "Extra", "sequence": 2}).StatusCode).To(Equal(fiber.StatusNotFound))
		Expect(call("PUT", base+"/phases/"+phase.ID, b, fiber.Map{"name": "Hijack", "sequence": 1}).StatusCode).To(Equal(fiber.StatusNotFound))
		Expect(call("DELETE", base+"/phases/"+phase.ID, b, nil).StatusCode).To(Equal(fiber.StatusNotFound))
		Expect(call("DELETE", base, b, nil).StatusCode).To(Equal(fiber.StatusNotFound))

		// Nothing changed for the owner.
		resp := call("GET", base, a, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var ct models.CampaignType
		Expect(json.NewDecoder(resp.Body).Decode(&ct)).To(Succeed())
		Expect(ct.Name).To(Equal("launch"))
		Expect(ct.Phases).To(HaveLen(1))
		Expect(ct.Phases[0].Name).To(Equal("Tease"))
	})

	It("rejects a campaign built on another workspace's custom type", func() {
		a := signupWorkspace("Acme", "a@acme.test")
		b := signupWorkspace("Beta", "b@beta.test")
		own := createType(a, "launch")

		resp := call("POST", "/api/campaigns", b, fiber.Map{"name": "Beta launch", "campaign_type_id": own.ID})
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))

		resp = call("POST", "/api/campaigns", b, fiber.Map{"name": "Beta campaign", "campaign_type_id": ctAwareness})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var campaign models.Campaign
		Expect(json.NewDecoder(resp.Body).Decode(&campaign)).To(Succeed())

		resp = call("PUT", "/api/campaigns/"+campaign.ID, b, fiber.Map{"name": "Beta campaign", "campaign_type_id": own.ID})
		Expect(resp.StatusCode).To(Equal(fiber.StatusBadRequest))
	})

	It("lets two workspaces own a custom type of the same name", func() {
		a := signupWorkspace("Acme", "a@acme.test")
		b := signupWorkspace("Beta", "b@beta.test")
		createType(a, "launch")
		createType(b, "launch")
	})

	It("409s a custom type named like a system type or the workspace's own", func() {
		a := signupWorkspace("Acme", "a@acme.test")
		own := createType(a, "launch")
		other := createType(a, "relaunch")

		for _, name := range []string{"awareness", "Awareness", "launch", "LAUNCH"} {
			resp := call("POST", "/api/campaign_types", a, fiber.Map{"name": name, "label": "X"})
			Expect(resp.StatusCode).To(Equal(fiber.StatusConflict), name)
			var body map[string]any
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(body["code"]).To(Equal("campaign_type_name_taken"))
		}
		Expect(call("POST", "/api/campaign_types/"+ctAwareness+"/clone", a, fiber.Map{"name": "awareness", "label": "X"}).StatusCode).
			To(Equal(fiber.StatusConflict))
		Expect(call("PUT", "/api/campaign_types/"+other.ID, a, fiber.Map{"name": "launch", "label": "X"}).StatusCode).
			To(Equal(fiber.StatusConflict))

		// Renaming a type to its own name is not a clash.
		Expect(call("PUT", "/api/campaign_types/"+own.ID, a, fiber.Map{"name": "launch", "label": "Launch 2"}).StatusCode).
			To(Equal(fiber.StatusOK))
	})

	It("keeps system types and their phases read-only", func() {
		a := signupWorkspace("Acme", "a@acme.test")
		resp := call("GET", "/api/campaign_types/"+ctAwareness, a, nil)
		Expect(resp.StatusCode).To(Equal(fiber.StatusOK))
		var system models.CampaignType
		Expect(json.NewDecoder(resp.Body).Decode(&system)).To(Succeed())
		Expect(system.Phases).NotTo(BeEmpty())
		base := "/api/campaign_types/" + ctAwareness

		Expect(call("PUT", base, a, fiber.Map{"name": "awareness", "label": "Mine"}).StatusCode).To(Equal(fiber.StatusForbidden))
		Expect(call("DELETE", base, a, nil).StatusCode).To(Equal(fiber.StatusForbidden))
		Expect(call("POST", base+"/phases", a, fiber.Map{"name": "Extra", "sequence": 9}).StatusCode).To(Equal(fiber.StatusForbidden))
		Expect(call("PUT", base+"/phases/"+system.Phases[0].ID, a, fiber.Map{"name": "Renamed", "sequence": 1}).StatusCode).
			To(Equal(fiber.StatusForbidden))
		Expect(call("DELETE", base+"/phases/"+system.Phases[0].ID, a, nil).StatusCode).To(Equal(fiber.StatusForbidden))
	})

	It("gives a clone of a system type to the cloning workspace only", func() {
		a := signupWorkspace("Acme", "a@acme.test")
		b := signupWorkspace("Beta", "b@beta.test")
		resp := call("POST", "/api/campaign_types/"+ctAwareness+"/clone", a, fiber.Map{"name": "my-awareness", "label": "Mine"})
		Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
		var clone models.CampaignType
		Expect(json.NewDecoder(resp.Body).Decode(&clone)).To(Succeed())
		Expect(clone.IsSystem).To(BeFalse())

		Expect(listTypeIDs(a)).To(ContainElement(clone.ID))
		Expect(listTypeIDs(b)).NotTo(ContainElement(clone.ID))
	})
})
