package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/brandlib"
)

// BrandHandler is the REST surface for the Brand module: one aggregate
// read plus per-section whole-resource writes, all tenant-scoped behind h.auth.
// The wire shapes match the ui repo's components/brand/types.ts exactly, so the
// prototype's stubbed services (services/api/brand.ts) each collapse to one
// apiJson call against these endpoints with nothing above them changing.
type BrandHandler struct {
	repo     repository.BrandRepository
	lib      *brandlib.Service
	storage  storage.Storage
	auth     fiber.Handler
	activity *activity.Recorder
}

// NewBrandHandler constructs the handler. storage may be nil (uploads then 503);
// a nil rec records nothing.
func NewBrandHandler(repo repository.BrandRepository, store storage.Storage, auth fiber.Handler, rec *activity.Recorder) *BrandHandler {
	return &BrandHandler{repo: repo, lib: brandlib.New(repo), storage: store, auth: auth, activity: rec}
}

func (h *BrandHandler) recordActivity(c *fiber.Ctx, typ string, opts ...activity.Option) {
	if h.activity == nil {
		return
	}
	h.activity.Record(reqCtx(c), activity.CategoryBrand, typ,
		append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

// brandNow is time.Now normalised to the microsecond resolution Postgres stores
// timestamptz at, so a value we persist and echo on a write matches exactly what
// a later GET returns — no ns-vs-µs drift between a POST/PUT response and a read.
func brandNow() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func (h *BrandHandler) Register(app *fiber.App) {
	g := app.Group("/api/brand")
	g.Get("/", h.auth, h.GetAll)
	// Binary upload for logos / template PNGs / reference images.
	g.Post("/uploads", h.auth, h.Upload)
	// Singletons — literal segments, no :id.
	g.Put("/guardrails", h.auth, h.PutGuardrails)
	g.Delete("/guardrails", h.auth, h.DeleteGuardrails)
	g.Put("/guardrails/stance", h.auth, h.PutGuardrailsStance)
	g.Put("/look", h.auth, h.PutLook)
	g.Delete("/look", h.auth, h.DeleteLook)
	// Libraries.
	g.Post("/voices", h.auth, h.CreateVoice)
	g.Put("/voices/:id", h.auth, h.UpdateVoice)
	g.Delete("/voices/:id", h.auth, h.DeleteVoice)
	g.Post("/audiences", h.auth, h.CreateAudience)
	g.Put("/audiences/:id", h.auth, h.UpdateAudience)
	g.Delete("/audiences/:id", h.auth, h.DeleteAudience)
	g.Post("/templates", h.auth, h.CreateTemplate)
	g.Put("/templates/:id", h.auth, h.UpdateTemplate)
	g.Delete("/templates/:id", h.auth, h.DeleteTemplate)
	// Facts ledger.
	g.Post("/facts", h.auth, h.CreateFact)
	g.Put("/facts/:id", h.auth, h.UpdateFact)
	g.Delete("/facts/:id", h.auth, h.DeleteFact)
}

// GetAll returns the whole aggregate — every slot present (FR1).
func (h *BrandHandler) GetAll(c *fiber.Ctx) error {
	data, err := h.repo.GetAll(reqCtx(c))
	if err != nil {
		return err
	}
	return c.JSON(data)
}

// ── Voices ──────────────────────────────────────────────────────────────────

func (h *BrandHandler) CreateVoice(c *fiber.Ctx) error {
	var v models.BrandVoice
	if err := c.BodyParser(&v); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	created, err := h.lib.CreateVoice(reqCtx(c), &v)
	if err != nil {
		return libError(err, "voice not found")
	}
	h.recordActivity(c, "brand_voice_created", activity.WithEntity("brand_voice", created.ID))
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *BrandHandler) UpdateVoice(c *fiber.Ctx) error {
	var v models.BrandVoice
	if err := c.BodyParser(&v); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	v.ID = c.Params("id")
	updated, err := h.lib.UpdateVoice(reqCtx(c), &v)
	if err != nil {
		return libError(err, "voice not found")
	}
	h.recordActivity(c, "brand_voice_updated", activity.WithEntity("brand_voice", updated.ID))
	return c.JSON(updated)
}

func (h *BrandHandler) DeleteVoice(c *fiber.Ctx) error {
	deleted, err := h.lib.DeleteVoice(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "voice not found")
	}
	h.recordActivity(c, "brand_voice_deleted", activity.WithEntity("brand_voice", c.Params("id")))
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Audiences ───────────────────────────────────────────────────────────────

func (h *BrandHandler) CreateAudience(c *fiber.Ctx) error {
	var a models.BrandAudience
	if err := c.BodyParser(&a); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	created, err := h.lib.CreateAudience(reqCtx(c), &a)
	if err != nil {
		return libError(err, "audience not found")
	}
	h.recordActivity(c, "brand_audience_created", activity.WithEntity("brand_audience", created.ID))
	return c.Status(fiber.StatusCreated).JSON(created)
}

func (h *BrandHandler) UpdateAudience(c *fiber.Ctx) error {
	var a models.BrandAudience
	if err := c.BodyParser(&a); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	a.ID = c.Params("id")
	updated, err := h.lib.UpdateAudience(reqCtx(c), &a)
	if err != nil {
		return libError(err, "audience not found")
	}
	h.recordActivity(c, "brand_audience_updated", activity.WithEntity("brand_audience", updated.ID))
	return c.JSON(updated)
}

func (h *BrandHandler) DeleteAudience(c *fiber.Ctx) error {
	deleted, err := h.lib.DeleteAudience(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "audience not found")
	}
	h.recordActivity(c, "brand_audience_deleted", activity.WithEntity("brand_audience", c.Params("id")))
	return c.SendStatus(fiber.StatusNoContent)
}

// libError maps a brand library error: a broken rule is a 422, a missing
// item a 404 with missing as its message.
func libError(err error, missing string) error {
	switch {
	case brandlib.IsRule(err):
		return ruleError(err)
	case errors.Is(err, sql.ErrNoRows):
		return fiber.NewError(fiber.StatusNotFound, missing)
	}
	return err
}

// ── Guardrails (singleton) ──────────────────────────────────────────────────

// guardrailsRequest is the PUT /api/brand/guardrails body. facts is
// presence-aware: omitted leaves the ledger alone, present
// reconciles the ledger to it by statement.
type guardrailsRequest struct {
	Facts       Optional[models.StringSlice] `json:"facts"       swaggertype:"array,string"`
	MayClaim    models.StringSlice           `json:"mayClaim"`
	NeverClaim  models.StringSlice           `json:"neverClaim"`
	BannedWords models.StringSlice           `json:"bannedWords"`
	Disclaimer  string                       `json:"disclaimer"`
}

func (h *BrandHandler) PutGuardrails(c *fiber.Ctx) error {
	var req guardrailsRequest
	if err := json.Unmarshal(c.Body(), &req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	g := models.BrandGuardrails{
		MayClaim:    req.MayClaim,
		NeverClaim:  req.NeverClaim,
		BannedWords: req.BannedWords,
		Disclaimer:  req.Disclaimer,
	}
	// nil facts = key omitted: the ledger is not touched.
	var facts []string
	if req.Facts.Present {
		facts = req.Facts.orZero()
		if facts == nil {
			facts = []string{}
		}
	}
	saved, err := h.lib.SaveGuardrails(reqCtx(c), &g, facts, sessionUserID(c))
	if err != nil {
		if brandlib.IsRule(err) {
			return ruleError(err)
		}
		return factError(err)
	}
	if saved.Changed {
		var opts []activity.Option
		if saved.Facts != nil {
			opts = append(opts, activity.WithPayload(map[string]any{"facts_reconciled": *saved.Facts}))
		}
		h.recordActivity(c, "brand_guardrails_updated", opts...)
	}
	return c.JSON(saved.Guardrails)
}

// DeleteGuardrails removes the rules. The facts ledger is its own section now
// and is not touched, nor is a previous stance brought back.
func (h *BrandHandler) DeleteGuardrails(c *fiber.Ctx) error {
	deleted, err := h.lib.DeleteGuardrails(reqCtx(c))
	if err != nil {
		return err
	}
	if deleted {
		h.recordActivity(c, "brand_guardrails_deleted")
	}
	// Idempotent: absent → 204 too. The section is now empty either way.
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Look (singleton) ────────────────────────────────────────────────────────

func (h *BrandHandler) PutLook(c *fiber.Ctx) error {
	var l models.BrandLook
	if err := c.BodyParser(&l); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	normalizeLook(&l)
	if err := validateLook(&l); err != nil {
		return err
	}
	existing, err := h.repo.GetLook(reqCtx(c))
	if err != nil {
		return err
	}
	now := brandNow()
	l.UpdatedAt = now
	if existing == nil {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		l.ID = id
		l.CreatedAt = now
	} else {
		// See PutGuardrails: ON CONFLICT upsert leaves id / created_at untouched.
		l.ID = existing.ID
		l.CreatedAt = existing.CreatedAt
	}
	if err := h.repo.UpsertLook(reqCtx(c), &l); err != nil {
		return err
	}
	h.recordActivity(c, "brand_look_updated")
	return c.JSON(&l)
}

func (h *BrandHandler) DeleteLook(c *fiber.Ctx) error {
	deleted, err := h.repo.DeleteLook(reqCtx(c))
	if err != nil {
		return err
	}
	if deleted {
		h.recordActivity(c, "brand_look_deleted")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Templates ───────────────────────────────────────────────────────────────

func (h *BrandHandler) CreateTemplate(c *fiber.Ctx) error {
	var t models.BrandTemplate
	if err := c.BodyParser(&t); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	normalizeTemplate(&t)
	if err := validateTemplate(&t); err != nil {
		return err
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	t.ID = id
	now := brandNow()
	t.CreatedAt, t.UpdatedAt = now, now
	if err := h.repo.CreateTemplate(reqCtx(c), &t); err != nil {
		return err
	}
	h.recordActivity(c, "brand_template_created", activity.WithEntity("brand_template", t.ID))
	return c.Status(fiber.StatusCreated).JSON(&t)
}

func (h *BrandHandler) UpdateTemplate(c *fiber.Ctx) error {
	var t models.BrandTemplate
	if err := c.BodyParser(&t); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	t.ID = c.Params("id")
	normalizeTemplate(&t)
	if err := validateTemplate(&t); err != nil {
		return err
	}
	t.UpdatedAt = brandNow()
	if err := h.repo.UpdateTemplate(reqCtx(c), &t); err != nil {
		return notFound(err, "template not found")
	}
	h.recordActivity(c, "brand_template_updated", activity.WithEntity("brand_template", t.ID))
	return c.JSON(&t)
}

func (h *BrandHandler) DeleteTemplate(c *fiber.Ctx) error {
	deleted, err := h.repo.DeleteTemplate(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "template not found")
	}
	h.recordActivity(c, "brand_template_deleted", activity.WithEntity("brand_template", c.Params("id")))
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Upload ──────────────────────────────────────────────────────────────────

const maxBrandUploadSize = 10 << 20 // 10 MB

// Upload stores one image to object storage and returns its public URL. Mirrors
// the assets/images path (MIME sniffed from the bytes, client Content-Type
// ignored). SVG is not in allowedImageMIMEs and sniffs as text, so it is
// rejected here — the v1 answer to the SVG question (revisited in CON-132).
func (h *BrandHandler) Upload(c *fiber.Ctx) error {
	if h.storage == nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "storage not configured")
	}
	fh, err := c.FormFile("file")
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "file is required")
	}
	if fh.Size > maxBrandUploadSize {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge,
			fmt.Sprintf("file exceeds maximum size of %d MB", maxBrandUploadSize>>20))
	}
	f, err := fh.Open()
	if err != nil {
		return fmt.Errorf("brand: open upload: %w", err)
	}
	defer f.Close()

	sniff := make([]byte, 512)
	n, err := f.Read(sniff)
	if err != nil {
		return fmt.Errorf("brand: read upload: %w", err)
	}
	mimeType := http.DetectContentType(sniff[:n])
	ext, ok := allowedImageMIMEs[mimeType]
	if !ok {
		return fiber.NewError(fiber.StatusUnsupportedMediaType,
			fmt.Sprintf("unsupported media type: %s", mimeType))
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("brand: seek upload: %w", err)
	}

	key := storage.TenantKey(reqCtx(c), "brand/"+uuid.NewString()+ext)
	url, err := h.storage.Upload(reqCtx(c), key, f, fh.Size, mimeType)
	if err != nil {
		return fmt.Errorf("brand: storage upload: %w", err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"url": url, "s3Key": key})
}

// ── validation + normalization ──────────────────────────────────────────────

var (
	templateRoles = map[string]bool{"foreground": true, "background": true}
	logoJobs      = map[string]bool{"profile": true, "watermark": true, "mark": true}

	hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

func unprocessable(format string, args ...any) error {
	return fiber.NewError(fiber.StatusUnprocessableEntity, fmt.Sprintf(format, args...))
}

// ruleError turns a broken brand rule into a 422.
func ruleError(err error) error {
	if err == nil {
		return nil
	}
	return fiber.NewError(fiber.StatusUnprocessableEntity, err.Error())
}

func normalizeLook(l *models.BrandLook) {
	if l.Logos == nil {
		l.Logos = models.BrandLogos{}
	}
	if l.Palette == nil {
		l.Palette = models.BrandPalette{}
	}
	if l.Typefaces == nil {
		l.Typefaces = models.StringSlice{}
	}
	if l.ReferenceImages == nil {
		l.ReferenceImages = models.StringSlice{}
	}
	for i := range l.Logos {
		if l.Logos[i].ID == "" {
			l.Logos[i].ID, _ = models.NewID()
		}
	}
	for i := range l.Palette {
		if l.Palette[i].ID == "" {
			l.Palette[i].ID, _ = models.NewID()
		}
	}
}

func validateLook(l *models.BrandLook) error {
	for _, lg := range l.Logos {
		if !logoJobs[lg.Job] {
			return unprocessable("invalid logo job: %q", lg.Job)
		}
	}
	for _, col := range l.Palette {
		if !hexColorRe.MatchString(col.Hex) {
			return unprocessable("invalid colour hex: %q", col.Hex)
		}
	}
	return nil
}

func normalizeTemplate(t *models.BrandTemplate) {
	if t.Ratios == nil {
		t.Ratios = models.TemplateRatios{}
	}
	if t.Platforms == nil {
		t.Platforms = models.StringSlice{}
	}
	if t.Role == "" {
		t.Role = "foreground"
	}
	if t.Origin.Kind == "" {
		t.Origin = models.BrandOrigin{Kind: "blank"}
	}
}

func validateTemplate(t *models.BrandTemplate) error {
	if strings.TrimSpace(t.Name) == "" {
		return unprocessable("name is required")
	}
	if !templateRoles[t.Role] {
		return unprocessable("invalid role: %q", t.Role)
	}
	return nil
}
