package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// CON-316: the facts ledger (/api/brand/facts), the guardrails stance and the
// guardrails.facts compatibility path.
var _ = Describe("Brand facts ledger (CON-316)", Ordered, func() {
	var (
		app        *fiber.App
		db         *bun.DB
		authCookie *http.Cookie
		admin      *models.User
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
		accountRepo := repository.NewAccountRepository(db)
		tenantRepo := repository.NewTenantRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, accountRepo, sessionRepo, testCookieName, false).Register(app)
		handlers.NewTenantsHandler(signup.New(db, accountRepo, tenantRepo, nil), tenantRepo, testCookieName, false, auth).Register(app)
		handlers.NewBrandHandler(repository.NewBrandRepository(db), nil, auth).Register(app)

		admin = seedTenantUser(db, "Ada Admin", "admin@example.com", "admin-password")
		authCookie = loginCookie(app, "admin@example.com", "admin-password")
	})

	AfterEach(func() {
		ctx := context.Background()
		for _, tbl := range []string{
			"brand_facts", "brand_guardrails_stance", "brand_guardrails", "sessions", "users", "accounts",
		} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(ctx)
			Expect(err).NotTo(HaveOccurred())
		}
		_, _ = db.NewDelete().Model((*models.Tenant)(nil)).Where("id <> ?", models.DefaultTenantID).Exec(tenantCtx())
	})

	doAs := func(cookie *http.Cookie, method, path string, body any) *http.Response {
		GinkgoHelper()
		var r *bytes.Reader
		switch b := body.(type) {
		case nil:
			r = bytes.NewReader(nil)
		case string:
			r = bytes.NewReader([]byte(b))
		default:
			raw, _ := json.Marshal(b)
			r = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, r)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, err := app.Test(req)
		Expect(err).NotTo(HaveOccurred())
		return resp
	}
	do := func(method, path string, body any) *http.Response {
		GinkgoHelper()
		return doAs(authCookie, method, path, body)
	}

	decode := func(resp *http.Response, v any) {
		GinkgoHelper()
		Expect(json.NewDecoder(resp.Body).Decode(v)).To(Succeed())
	}

	getBrand := func() models.BrandData {
		GinkgoHelper()
		resp := do("GET", "/api/brand", nil)
		Expect(resp.StatusCode).To(Equal(200))
		var data models.BrandData
		decode(resp, &data)
		return data
	}

	createFact := func(body fiber.Map) models.BrandFact {
		GinkgoHelper()
		resp := do("POST", "/api/brand/facts", body)
		Expect(resp.StatusCode).To(Equal(201))
		var f models.BrandFact
		decode(resp, &f)
		return f
	}

	today := models.CalendarDateOf(time.Now()).String()

	Describe("aggregate", func() {
		It("returns facts [] and an undecided stance for a fresh tenant", func() {
			resp := do("GET", "/api/brand", nil)
			body := map[string]json.RawMessage{}
			decode(resp, &body)
			Expect(string(body["facts"])).To(Equal("[]"))
			Expect(string(body["guardrailsStance"])).To(MatchJSON(
				`{"none":false,"decidedAt":null,"decidedBy":null,"decidedByName":null}`))
		})
	})

	Describe("CRUD", func() {
		It("creates with defaults, stamps the author and today's addedAt", func() {
			resp := do("POST", "/api/brand/facts", fiber.Map{"statement": "  We ship weekly.  "})
			Expect(resp.StatusCode).To(Equal(201))
			raw := map[string]any{}
			decode(resp, &raw)
			Expect(raw["statement"]).To(Equal("We ship weekly."))
			Expect(raw["subject"]).To(Equal("us"))
			Expect(raw["kind"]).To(Equal("documented"))
			Expect(raw["source"]).To(Equal(""))
			Expect(raw["addedAt"]).To(Equal(today))
			Expect(raw["checkedAt"]).To(BeNil())
			Expect(raw["expiresAt"]).To(BeNil())
			Expect(raw["createdBy"]).To(Equal(admin.ID))
			Expect(raw["createdByName"]).To(Equal("Ada Admin"))
			Expect(raw["id"]).NotTo(BeEmpty())
			Expect(raw).NotTo(HaveKey("status"))

			data := getBrand()
			Expect(data.Facts).To(HaveLen(1))
			Expect(data.Facts[0].ID).To(Equal(raw["id"]))
		})

		It("round-trips every field and keeps id + metadata when the statement changes", func() {
			f := createFact(fiber.Map{
				"statement": "Support answered 94% of tickets within one working day.",
				"subject":   "us", "kind": "measured", "source": "Helpdesk export, Q4",
				"addedAt": "2026-09-01", "checkedAt": "2026-09-20", "expiresAt": "2027-01-31",
			})
			Expect(f.ExpiresAt.String()).To(Equal("2027-01-31"))

			resp := do("PUT", "/api/brand/facts/"+f.ID, fiber.Map{
				"statement": "Support answered 95% of tickets within one working day.",
				"subject":   "us", "kind": "measured", "source": "Helpdesk export, Q4",
				"addedAt": "2026-09-01", "checkedAt": "2026-09-24", "expiresAt": "2027-01-31",
			})
			Expect(resp.StatusCode).To(Equal(200))
			var got models.BrandFact
			decode(resp, &got)
			Expect(got.ID).To(Equal(f.ID))
			Expect(got.Statement).To(HavePrefix("Support answered 95%"))
			Expect(got.CheckedAt.String()).To(Equal("2026-09-24"))
			Expect(got.CreatedByName).To(Equal("Ada Admin"))
			Expect(got.UpdatedAt).To(BeTemporally(">", f.UpdatedAt))

			stored := getBrand().Facts[0]
			Expect(stored.Statement).To(Equal(got.Statement))
			Expect(stored.Source).To(Equal("Helpdesk export, Q4"))
			Expect(stored.AddedAt.String()).To(Equal("2026-09-01"))
		})

		It("does not restamp updatedAt on an unchanged PUT", func() {
			body := fiber.Map{"statement": "Stable.", "subject": "problem", "kind": "judgement", "addedAt": "2026-09-01"}
			f := createFact(body)
			resp := do("PUT", "/api/brand/facts/"+f.ID, body)
			Expect(resp.StatusCode).To(Equal(200))
			var got models.BrandFact
			decode(resp, &got)
			Expect(got.UpdatedAt).To(BeTemporally("==", f.UpdatedAt))
		})

		It("PUT is a full replace: an omitted date clears it", func() {
			f := createFact(fiber.Map{"statement": "Dated.", "expiresAt": "2027-01-01"})
			resp := do("PUT", "/api/brand/facts/"+f.ID, fiber.Map{"statement": "Dated.", "subject": "us", "kind": "documented"})
			Expect(resp.StatusCode).To(Equal(200))
			var got models.BrandFact
			decode(resp, &got)
			Expect(got.ExpiresAt).To(BeNil())
			Expect(got.AddedAt).To(BeNil())
		})

		It("deletes (204) and 404s on an unknown id", func() {
			f := createFact(fiber.Map{"statement": "Temporary."})
			Expect(do("DELETE", "/api/brand/facts/"+f.ID, nil).StatusCode).To(Equal(204))
			Expect(getBrand().Facts).To(BeEmpty())
			Expect(do("DELETE", "/api/brand/facts/"+f.ID, nil).StatusCode).To(Equal(404))
			Expect(do("PUT", "/api/brand/facts/nope", fiber.Map{"statement": "x", "subject": "us", "kind": "measured"}).StatusCode).To(Equal(404))
		})

		It("orders the ledger by creation", func() {
			a := createFact(fiber.Map{"statement": "First."})
			b := createFact(fiber.Map{"statement": "Second."})
			facts := getBrand().Facts
			Expect(facts).To(HaveLen(2))
			Expect([]string{facts[0].ID, facts[1].ID}).To(Equal([]string{a.ID, b.ID}))
		})
	})

	Describe("validation", func() {
		It("409s on a duplicate statement, on create and on rename", func() {
			createFact(fiber.Map{"statement": "Unique."})
			resp := do("POST", "/api/brand/facts", fiber.Map{"statement": " Unique. "})
			Expect(resp.StatusCode).To(Equal(409))
			other := createFact(fiber.Map{"statement": "Other."})
			resp = do("PUT", "/api/brand/facts/"+other.ID, fiber.Map{"statement": "Unique.", "subject": "us", "kind": "documented"})
			Expect(resp.StatusCode).To(Equal(409))
		})

		DescribeTable("422s on bad input",
			func(body fiber.Map) {
				Expect(do("POST", "/api/brand/facts", body).StatusCode).To(Equal(422))
			},
			Entry("blank statement", fiber.Map{"statement": "   "}),
			Entry("oversized statement", fiber.Map{"statement": strings.Repeat("x", 1025)}),
			Entry("bad subject", fiber.Map{"statement": "s", "subject": "them"}),
			Entry("bad kind", fiber.Map{"statement": "s", "kind": "rumour"}),
			Entry("oversized source", fiber.Map{"statement": "s", "source": strings.Repeat("é", 501)}),
			Entry("malformed date", fiber.Map{"statement": "s", "expiresAt": "31/01/2027"}),
			Entry("impossible date", fiber.Map{"statement": "s", "checkedAt": "2026-02-30"}),
		)

		It("400s on malformed JSON", func() {
			Expect(do("POST", "/api/brand/facts", "{nope").StatusCode).To(Equal(400))
		})

		It("accepts dates in any order (a fact may be recorded already expired)", func() {
			f := createFact(fiber.Map{"statement": "Old.", "addedAt": "2026-09-01", "checkedAt": "2026-08-01", "expiresAt": "2026-01-01"})
			Expect(f.ExpiresAt.String()).To(Equal("2026-01-01"))
		})

		It("422s on the 201st fact", func() {
			rows := make([]models.BrandFact, repository.MaxBrandFacts)
			for i := range rows {
				rows[i] = models.BrandFact{
					ID: fmt.Sprintf("seed-%d", i), Statement: fmt.Sprintf("Fact %d", i),
					Subject: models.FactSubjectUs, Kind: models.FactKindDocumented,
				}
			}
			_, err := db.NewInsert().Model(&rows).Exec(tenantCtx())
			Expect(err).NotTo(HaveOccurred())
			Expect(do("POST", "/api/brand/facts", fiber.Map{"statement": "One too many."}).StatusCode).To(Equal(422))
		})
	})

	Describe("guardrails.facts compatibility (FR6)", func() {
		rules := fiber.Map{"mayClaim": []string{"fast"}, "neverClaim": []string{}, "bannedWords": []string{}, "disclaimer": ""}

		It("projects the ledger into guardrails.facts and leaves it alone when facts is omitted", func() {
			createFact(fiber.Map{"statement": "Kept.", "kind": "measured"})
			resp := do("PUT", "/api/brand/guardrails", rules)
			Expect(resp.StatusCode).To(Equal(200))
			var g models.BrandGuardrails
			decode(resp, &g)
			Expect(g.Facts).To(Equal(models.StringSlice{"Kept."}))

			data := getBrand()
			Expect(data.Facts).To(HaveLen(1))
			Expect(data.Facts[0].Kind).To(Equal(models.FactKindMeasured))
			Expect(data.Guardrails.Facts).To(Equal(models.StringSlice{"Kept."}))
		})

		It("reconciles by statement when facts is present", func() {
			kept := createFact(fiber.Map{"statement": "Kept.", "kind": "measured", "source": "audit", "expiresAt": "2027-01-01"})
			createFact(fiber.Map{"statement": "Dropped."})

			body := fiber.Map{"facts": []string{" Kept. ", "New one.", "New one.", ""}, "mayClaim": []string{"fast"}}
			resp := do("PUT", "/api/brand/guardrails", body)
			Expect(resp.StatusCode).To(Equal(200))
			var g models.BrandGuardrails
			decode(resp, &g)
			Expect(g.Facts).To(Equal(models.StringSlice{"Kept.", "New one."}))

			facts := getBrand().Facts
			Expect(facts).To(HaveLen(2))
			Expect(facts[0].ID).To(Equal(kept.ID))
			Expect(facts[0].Kind).To(Equal(models.FactKindMeasured))
			Expect(facts[0].Source).To(Equal("audit"))
			Expect(facts[0].ExpiresAt.String()).To(Equal("2027-01-01"))
			Expect(facts[1].Statement).To(Equal("New one."))
			Expect(facts[1].Subject).To(Equal(models.FactSubjectUs))
			Expect(facts[1].Kind).To(Equal(models.FactKindDocumented))
			Expect(facts[1].AddedAt).To(BeNil())
			Expect(facts[1].CreatedByName).To(Equal("Ada Admin"))
		})

		It("422s when rules are empty and facts is omitted, even with facts in the ledger", func() {
			createFact(fiber.Map{"statement": "Ledger fact."})
			resp := do("PUT", "/api/brand/guardrails", fiber.Map{"mayClaim": []string{}})
			Expect(resp.StatusCode).To(Equal(422))
		})

		It("accepts a facts-only body (the flag-off editor)", func() {
			resp := do("PUT", "/api/brand/guardrails", fiber.Map{"facts": []string{"Only facts."}})
			Expect(resp.StatusCode).To(Equal(200))
			Expect(getBrand().Facts).To(HaveLen(1))
		})

		It("does not restamp updatedAt when rules and the statement set are unchanged", func() {
			body := fiber.Map{"facts": []string{"A.", "B."}, "mayClaim": []string{"fast"}}
			resp := do("PUT", "/api/brand/guardrails", body)
			var first models.BrandGuardrails
			decode(resp, &first)
			resp = do("PUT", "/api/brand/guardrails", fiber.Map{"facts": []string{"B.", "A."}, "mayClaim": []string{"fast"}})
			var second models.BrandGuardrails
			decode(resp, &second)
			Expect(second.UpdatedAt).To(BeTemporally("==", first.UpdatedAt))
		})

		It("DELETE /guardrails leaves the ledger alone", func() {
			do("PUT", "/api/brand/guardrails", rules)
			createFact(fiber.Map{"statement": "Survives."})
			Expect(do("DELETE", "/api/brand/guardrails", nil).StatusCode).To(Equal(204))
			data := getBrand()
			Expect(data.Guardrails).To(BeNil())
			Expect(data.Facts).To(HaveLen(1))
		})
	})

	Describe("guardrails stance (FR7)", func() {
		It("records none=true with the author while there are no guardrails", func() {
			resp := do("PUT", "/api/brand/guardrails/stance", fiber.Map{"none": true})
			Expect(resp.StatusCode).To(Equal(200))
			var s models.GuardrailsStance
			decode(resp, &s)
			Expect(s.None).To(BeTrue())
			Expect(s.DecidedAt).NotTo(BeNil())
			Expect(*s.DecidedBy).To(Equal(admin.ID))
			Expect(*s.DecidedByName).To(Equal("Ada Admin"))

			Expect(getBrand().GuardrailsStance.None).To(BeTrue())
		})

		It("is not affected by facts", func() {
			createFact(fiber.Map{"statement": "A fact."})
			Expect(do("PUT", "/api/brand/guardrails/stance", fiber.Map{"none": true}).StatusCode).To(Equal(200))
		})

		It("409s while guardrails exist", func() {
			do("PUT", "/api/brand/guardrails", fiber.Map{"mayClaim": []string{"fast"}})
			Expect(do("PUT", "/api/brand/guardrails/stance", fiber.Map{"none": true}).StatusCode).To(Equal(409))
		})

		It("is cleared by a guardrails save and not restored by DELETE", func() {
			do("PUT", "/api/brand/guardrails/stance", fiber.Map{"none": true})
			Expect(do("PUT", "/api/brand/guardrails", fiber.Map{"mayClaim": []string{"fast"}}).StatusCode).To(Equal(200))
			Expect(getBrand().GuardrailsStance.None).To(BeFalse())
			do("DELETE", "/api/brand/guardrails", nil)
			Expect(getBrand().GuardrailsStance.None).To(BeFalse())
		})

		It("none=false returns to undecided, idempotently", func() {
			do("PUT", "/api/brand/guardrails/stance", fiber.Map{"none": true})
			for range 2 {
				resp := do("PUT", "/api/brand/guardrails/stance", fiber.Map{"none": false})
				Expect(resp.StatusCode).To(Equal(200))
				var s models.GuardrailsStance
				decode(resp, &s)
				Expect(s).To(Equal(models.GuardrailsStance{}))
			}
			Expect(getBrand().GuardrailsStance.None).To(BeFalse())
		})

		It("422s without none and 400s on malformed JSON", func() {
			Expect(do("PUT", "/api/brand/guardrails/stance", fiber.Map{}).StatusCode).To(Equal(422))
			Expect(do("PUT", "/api/brand/guardrails/stance", "nope").StatusCode).To(Equal(400))
		})
	})

	It("isolates tenants: facts and stance are invisible and 404 elsewhere", func() {
		f := createFact(fiber.Map{"statement": "Tenant A only."})
		do("PUT", "/api/brand/guardrails/stance", fiber.Map{"none": true})

		other := signupCookie(app, "Other", "owner@other.example")
		resp := doAs(other, "GET", "/api/brand", nil)
		var data models.BrandData
		decode(resp, &data)
		Expect(data.Facts).To(BeEmpty())
		Expect(data.GuardrailsStance.None).To(BeFalse())

		Expect(doAs(other, "PUT", "/api/brand/facts/"+f.ID, fiber.Map{"statement": "x", "subject": "us", "kind": "measured"}).StatusCode).To(Equal(404))
		Expect(doAs(other, "DELETE", "/api/brand/facts/"+f.ID, nil).StatusCode).To(Equal(404))
		// The same statement is free in another workspace.
		Expect(doAs(other, "POST", "/api/brand/facts", fiber.Map{"statement": "Tenant A only."}).StatusCode).To(Equal(201))
		Expect(getBrand().Facts).To(HaveLen(1))
	})

	It("keeps createdByName after the author is removed", func() {
		f := createFact(fiber.Map{"statement": "Authored."})
		_, err := db.NewDelete().TableExpr("sessions").Where("1 = 1").Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		_, err = db.NewDelete().TableExpr("users").Where("id = ?", admin.ID).Exec(context.Background())
		Expect(err).NotTo(HaveOccurred())
		var row models.BrandFact
		Expect(db.NewSelect().Model(&row).Where("bf.id = ?", f.ID).Scan(tenantCtx())).To(Succeed())
		Expect(row.CreatedBy).To(BeNil())
		Expect(row.CreatedByName).To(Equal("Ada Admin"))
	})

	It("backfills legacy guardrails.facts with the sidecar defaults (FR2)", func() {
		ctx := context.Background()
		_, err := db.NewRaw(`INSERT INTO brand_guardrails (id, tenant_id, facts) VALUES ('g-legacy', ?, ?::jsonb)`,
			models.DefaultTenantID, `[" Beta. ", "Alpha.", "", "Beta.", "Gamma."]`).Exec(ctx)
		Expect(err).NotTo(HaveOccurred())

		up, err := os.ReadFile("../../infra/database/migrations/20260925000316_brand_facts.up.sql")
		Expect(err).NotTo(HaveOccurred())
		backfill := string(up)[strings.Index(string(up), "INSERT INTO brand_facts"):]
		_, err = db.ExecContext(ctx, backfill)
		Expect(err).NotTo(HaveOccurred())

		data := getBrand()
		Expect(data.Guardrails.Facts).To(Equal(models.StringSlice{"Beta.", "Alpha.", "Gamma."}))
		for _, f := range data.Facts {
			Expect(f.Subject).To(Equal(models.FactSubjectUs))
			Expect(f.Kind).To(Equal(models.FactKindDocumented))
			Expect(f.Source).To(BeEmpty())
			Expect(f.AddedAt).To(BeNil())
			Expect(f.CheckedAt).To(BeNil())
			Expect(f.ExpiresAt).To(BeNil())
			Expect(f.CreatedBy).To(BeNil())
			Expect(f.CreatedByName).To(BeEmpty())
		}
	})
})

// loginCookie signs in and returns the session cookie.
func loginCookie(app *fiber.App, email, password string) *http.Cookie {
	GinkgoHelper()
	body, _ := json.Marshal(fiber.Map{"email": email, "password": password})
	req := httptest.NewRequest("POST", "/api/sessions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(fiber.StatusCreated))
	cookies := resp.Cookies()
	Expect(cookies).To(HaveLen(1))
	return cookies[0]
}

// signupCookie creates a fresh workspace via public signup and returns its
// owner's session cookie.
func signupCookie(app *fiber.App, tenantName, email string) *http.Cookie {
	GinkgoHelper()
	body, _ := json.Marshal(fiber.Map{
		"tenant": fiber.Map{"name": tenantName},
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
