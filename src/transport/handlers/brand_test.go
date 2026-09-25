package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/gofiber/fiber/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

var _ = Describe("BrandHandler", Ordered, func() {
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
		accountRepo := repository.NewAccountRepository(db)
		auth := handlers.RequireAuth(sessionRepo, userRepo, testCookieName)
		handlers.NewSessionsHandler(userRepo, accountRepo, sessionRepo, testCookieName, false).Register(app)
		// nil storage: uploads are not exercised here (they'd need object storage).
		handlers.NewBrandHandler(repository.NewBrandRepository(db), nil, auth).Register(app)

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
		for _, tbl := range []string{
			"brand_voices", "brand_audiences", "brand_guardrails", "brand_facts", "brand_guardrails_stance",
			"brand_look", "brand_templates", "sessions", "users", "accounts",
		} {
			_, err := db.NewDelete().TableExpr(tbl).Where("1 = 1").Exec(context.Background())
			Expect(err).NotTo(HaveOccurred())
		}
	})

	// ── request helpers ──────────────────────────────────────────────────────

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

	getBrand := func() models.BrandData {
		GinkgoHelper()
		resp := do("GET", "/api/brand", nil)
		Expect(resp.StatusCode).To(Equal(200))
		var data models.BrandData
		Expect(json.NewDecoder(resp.Body).Decode(&data)).To(Succeed())
		return data
	}

	voiceBody := func(name string, isDefault bool) fiber.Map {
		return fiber.Map{
			"name":      name,
			"whenToUse": "when " + name,
			"isDefault": isDefault,
			"samples":   []string{"one", "two", "three"},
			"rules": fiber.Map{
				"emoji": "never", "hashtags": "few", "formality": "formal",
				"person": "we", "length": "short", "opening": "op", "closing": "cl",
			},
			"channelNotes": fiber.Map{"linkedin": "dialled down"},
			"origin":       fiber.Map{"kind": "blank"},
		}
	}

	createVoice := func(name string, isDefault bool) models.BrandVoice {
		GinkgoHelper()
		resp := do("POST", "/api/brand/voices", voiceBody(name, isDefault))
		Expect(resp.StatusCode).To(Equal(201))
		var v models.BrandVoice
		Expect(json.NewDecoder(resp.Body).Decode(&v)).To(Succeed())
		return v
	}

	// ── AC1: aggregate empties ───────────────────────────────────────────────

	Describe("GET /api/brand", func() {
		It("requires auth", func() {
			req := httptest.NewRequest("GET", "/api/brand", nil)
			resp, err := app.Test(req)
			Expect(err).NotTo(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(401))
		})

		It("returns every slot with empty lists and null singletons for a fresh tenant", func() {
			resp := do("GET", "/api/brand", nil)
			Expect(resp.StatusCode).To(Equal(200))
			// Raw shape: [] not null for lists, null for singletons (empty != absent).
			body := map[string]json.RawMessage{}
			Expect(json.NewDecoder(resp.Body).Decode(&body)).To(Succeed())
			Expect(string(body["voices"])).To(Equal("[]"))
			Expect(string(body["audiences"])).To(Equal("[]"))
			Expect(string(body["templates"])).To(Equal("[]"))
			Expect(string(body["guardrails"])).To(Equal("null"))
			Expect(string(body["look"])).To(Equal("null"))
		})
	})

	// ── AC2 + AC4: voice round-trip, camelCase, server-owned fields ───────────

	Describe("voices", func() {
		It("creates a voice and echoes camelCase, derived usage, stamped updatedAt", func() {
			resp := do("POST", "/api/brand/voices", voiceBody("Dry British", true))
			Expect(resp.StatusCode).To(Equal(201))
			var v models.BrandVoice
			Expect(json.NewDecoder(resp.Body).Decode(&v)).To(Succeed())
			Expect(v.ID).NotTo(BeEmpty())
			Expect(v.Name).To(Equal("Dry British"))
			Expect(v.IsDefault).To(BeTrue())
			Expect(v.Usage).To(Equal(models.BrandUsage{Drafts: 0, Published: 0})) // derived, zero (FR7)
			Expect(v.UpdatedAt.IsZero()).To(BeFalse())

			// camelCase keys on the wire — the ui types are the contract this
			// endpoint must match verbatim, so assert the raw key names.
			resp = do("GET", "/api/brand", nil)
			raw := struct {
				Voices []map[string]json.RawMessage `json:"voices"`
			}{}
			Expect(json.NewDecoder(resp.Body).Decode(&raw)).To(Succeed())
			Expect(raw.Voices).To(HaveLen(1))
			Expect(raw.Voices[0]).To(HaveKey("whenToUse"))
			Expect(raw.Voices[0]).To(HaveKey("isDefault"))
			Expect(raw.Voices[0]).To(HaveKey("channelNotes"))
			Expect(raw.Voices[0]).To(HaveKey("updatedAt"))
		})

		It("ignores client-supplied summary and derived fields on write (FR3)", func() {
			body := voiceBody("Sneaky", false)
			body["summary"] = "client-set summary"
			body["usage"] = fiber.Map{"drafts": 99, "published": 99}
			resp := do("POST", "/api/brand/voices", body)
			Expect(resp.StatusCode).To(Equal(201))
			var v models.BrandVoice
			Expect(json.NewDecoder(resp.Body).Decode(&v)).To(Succeed())
			Expect(v.Summary).To(Equal("")) // server-owned, withdrawn
			Expect(v.Usage).To(Equal(models.BrandUsage{}))
		})

		It("requires a name (422)", func() {
			body := voiceBody("", false)
			resp := do("POST", "/api/brand/voices", body)
			Expect(resp.StatusCode).To(Equal(422))
		})

		It("rejects an invalid rules enum (422)", func() {
			body := voiceBody("Bad", false)
			body["rules"] = fiber.Map{"emoji": "banana"}
			resp := do("POST", "/api/brand/voices", body)
			Expect(resp.StatusCode).To(Equal(422))
		})

		It("caps oversized whenToUse and too many channel notes (422)", func() {
			big := voiceBody("Big", false)
			big["whenToUse"] = strings.Repeat("x", (1<<10)+1)
			Expect(do("POST", "/api/brand/voices", big).StatusCode).To(Equal(422))

			notes := fiber.Map{}
			for i := range 51 {
				notes[fmt.Sprintf("p%d", i)] = "note"
			}
			many := voiceBody("Many", false)
			many["channelNotes"] = notes
			Expect(do("POST", "/api/brand/voices", many).StatusCode).To(Equal(422))
		})

		It("returns an updatedAt that matches the stored value (no ns/µs drift)", func() {
			resp := do("POST", "/api/brand/voices", voiceBody("Precise", false))
			Expect(resp.StatusCode).To(Equal(201))
			var created models.BrandVoice
			Expect(json.NewDecoder(resp.Body).Decode(&created)).To(Succeed())
			stored := getBrand().Voices
			Expect(stored).To(HaveLen(1))
			Expect(created.UpdatedAt).To(BeTemporally("==", stored[0].UpdatedAt))
		})

		It("replaces a voice and preserves write-once origin (FR6)", func() {
			body := voiceBody("Corp", false)
			body["origin"] = fiber.Map{"kind": "website", "url": "quantwealth.example.com/about"}
			resp := do("POST", "/api/brand/voices", body)
			Expect(resp.StatusCode).To(Equal(201))
			var created models.BrandVoice
			Expect(json.NewDecoder(resp.Body).Decode(&created)).To(Succeed())

			// PUT with a different origin — must not move.
			upd := voiceBody("Corp renamed", false)
			upd["origin"] = fiber.Map{"kind": "blank"}
			resp = do("PUT", "/api/brand/voices/"+created.ID, upd)
			Expect(resp.StatusCode).To(Equal(200))

			data := getBrand()
			Expect(data.Voices).To(HaveLen(1))
			Expect(data.Voices[0].Name).To(Equal("Corp renamed"))
			Expect(data.Voices[0].Origin.Kind).To(Equal("website"))
			Expect(data.Voices[0].Origin.URL).To(Equal("quantwealth.example.com/about"))
		})

		It("404s on replacing/deleting an unknown id", func() {
			resp := do("PUT", "/api/brand/voices/nope", voiceBody("X", false))
			Expect(resp.StatusCode).To(Equal(404))
			resp = do("DELETE", "/api/brand/voices/nope", nil)
			Expect(resp.StatusCode).To(Equal(404))
		})
	})

	// ── AC3: one-default invariant ───────────────────────────────────────────

	Describe("one-default invariant", func() {
		It("demotes the previous default when a new default is saved", func() {
			a := createVoice("A", true)
			b := createVoice("B", true)

			data := getBrand()
			byID := map[string]models.BrandVoice{}
			for _, v := range data.Voices {
				byID[v.ID] = v
			}
			Expect(byID[a.ID].IsDefault).To(BeFalse())
			Expect(byID[b.ID].IsDefault).To(BeTrue())
		})

		It("hands the default to a survivor when the default is deleted", func() {
			a := createVoice("A", true)
			b := createVoice("B", false)

			resp := do("DELETE", "/api/brand/voices/"+a.ID, nil)
			Expect(resp.StatusCode).To(Equal(204))

			data := getBrand()
			Expect(data.Voices).To(HaveLen(1))
			Expect(data.Voices[0].ID).To(Equal(b.ID))
			Expect(data.Voices[0].IsDefault).To(BeTrue())
		})
	})

	// ── AC5: guardrails singleton ────────────────────────────────────────────

	Describe("guardrails", func() {
		validBody := fiber.Map{
			"facts":       []string{"Authorised and regulated by the FCA."},
			"mayClaim":    []string{},
			"neverClaim":  []string{"Any future return."},
			"bannedWords": []string{"guaranteed"},
			"disclaimer":  "Capital at risk.",
		}

		It("rejects an entirely empty save (422) and clears only via DELETE", func() {
			resp := do("PUT", "/api/brand/guardrails", fiber.Map{
				"facts": []string{}, "mayClaim": []string{}, "neverClaim": []string{},
				"bannedWords": []string{}, "disclaimer": "",
			})
			Expect(resp.StatusCode).To(Equal(422))
		})

		It("upserts, does not restamp updatedAt on an unchanged save, and deletes to null", func() {
			resp := do("PUT", "/api/brand/guardrails", validBody)
			Expect(resp.StatusCode).To(Equal(200))
			var first models.BrandGuardrails
			Expect(json.NewDecoder(resp.Body).Decode(&first)).To(Succeed())
			Expect(first.UpdatedAt.IsZero()).To(BeFalse())

			// Read the stored timestamp back. Postgres truncates timestamptz to
			// microseconds, so comparing the reloaded value (not the ns-precision
			// in-memory response) keeps both sides at DB precision — and is the
			// stronger claim anyway: the unchanged save must return what's stored.
			storedBefore := getBrand().Guardrails
			Expect(storedBefore).NotTo(BeNil())

			// Identical save → returns the same stored timestamp, not a new one (FR8).
			resp = do("PUT", "/api/brand/guardrails", validBody)
			Expect(resp.StatusCode).To(Equal(200))
			var second models.BrandGuardrails
			Expect(json.NewDecoder(resp.Body).Decode(&second)).To(Succeed())
			Expect(second.UpdatedAt).To(BeTemporally("==", storedBefore.UpdatedAt))

			// DELETE → null.
			resp = do("DELETE", "/api/brand/guardrails", nil)
			Expect(resp.StatusCode).To(Equal(204))
			Expect(getBrand().Guardrails).To(BeNil())
		})
	})

	// ── AC2: audiences + templates round-trip ────────────────────────────────

	Describe("audiences and templates", func() {
		It("round-trips an audience", func() {
			resp := do("POST", "/api/brand/audiences", fiber.Map{
				"name": "Sceptics", "who": "retail investors",
				"readsOn": "phone", "scrollsPastWhen": "a percentage",
				"believesWhen": "a citation", "origin": fiber.Map{"kind": "blank"},
			})
			Expect(resp.StatusCode).To(Equal(201))
			var a models.BrandAudience
			Expect(json.NewDecoder(resp.Body).Decode(&a)).To(Succeed())
			Expect(a.ID).NotTo(BeEmpty())
			Expect(a.Usage).To(Equal(models.BrandUsage{}))

			resp = do("DELETE", "/api/brand/audiences/"+a.ID, nil)
			Expect(resp.StatusCode).To(Equal(204))
			Expect(getBrand().Audiences).To(BeEmpty())
		})

		It("round-trips a template with the one-default rule and validates role", func() {
			bad := fiber.Map{"name": "T", "role": "sideways"}
			resp := do("POST", "/api/brand/templates", bad)
			Expect(resp.StatusCode).To(Equal(422))

			resp = do("POST", "/api/brand/templates", fiber.Map{
				"name": "Corner lockup", "role": "foreground", "isDefault": true,
				"ratios":    []fiber.Map{{"ratio": "1:1", "url": "/favicon.svg"}},
				"platforms": []string{"Instagram"},
				"origin":    fiber.Map{"kind": "blank"},
			})
			Expect(resp.StatusCode).To(Equal(201))
			var t models.BrandTemplate
			Expect(json.NewDecoder(resp.Body).Decode(&t)).To(Succeed())
			Expect(t.IsDefault).To(BeTrue())
			Expect(t.Ratios).To(HaveLen(1))
		})

		It("hands the default to a survivor when the default template is deleted", func() {
			mk := func(name string, def bool) models.BrandTemplate {
				resp := do("POST", "/api/brand/templates", fiber.Map{
					"name": name, "role": "foreground", "isDefault": def,
					"ratios": []fiber.Map{{"ratio": "1:1", "url": "/x.png"}},
				})
				Expect(resp.StatusCode).To(Equal(201))
				var t models.BrandTemplate
				Expect(json.NewDecoder(resp.Body).Decode(&t)).To(Succeed())
				return t
			}
			a := mk("A", true)
			b := mk("B", false)

			Expect(do("DELETE", "/api/brand/templates/"+a.ID, nil).StatusCode).To(Equal(204))

			data := getBrand()
			Expect(data.Templates).To(HaveLen(1))
			Expect(data.Templates[0].ID).To(Equal(b.ID))
			Expect(data.Templates[0].IsDefault).To(BeTrue())
		})

		It("round-trips look via the ON CONFLICT upsert, then deletes to null", func() {
			resp := do("PUT", "/api/brand/look", fiber.Map{
				"logos":           []fiber.Map{{"job": "profile", "url": "/logo.png"}},
				"palette":         []fiber.Map{{"role": "Primary", "hex": "#10243E"}},
				"typefaces":       []string{"Inter"},
				"referenceImages": []string{},
			})
			Expect(resp.StatusCode).To(Equal(200))
			Expect(getBrand().Look).NotTo(BeNil())

			// A second PUT exercises the ON CONFLICT DO UPDATE branch.
			resp = do("PUT", "/api/brand/look", fiber.Map{
				"logos":           []fiber.Map{{"job": "mark", "url": "/logo2.png"}},
				"palette":         []fiber.Map{},
				"typefaces":       []string{},
				"referenceImages": []string{},
			})
			Expect(resp.StatusCode).To(Equal(200))
			Expect(getBrand().Look.Logos).To(HaveLen(1))

			Expect(do("DELETE", "/api/brand/look", nil).StatusCode).To(Equal(204))
			Expect(getBrand().Look).To(BeNil())
		})
	})
})
