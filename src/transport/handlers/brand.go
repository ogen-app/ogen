package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/activity"
)

// BrandHandler is the REST surface for the Brand module (CON-228): one aggregate
// read plus per-section whole-resource writes, all tenant-scoped behind h.auth.
// The wire shapes match the ui repo's components/brand/types.ts exactly, so the
// prototype's stubbed services (services/api/brand.ts) each collapse to one
// apiJson call against these endpoints with nothing above them changing.
type BrandHandler struct {
	repo     repository.BrandRepository
	storage  storage.Storage
	auth     fiber.Handler
	activity *activity.Recorder
}

// NewBrandHandler constructs the handler. storage may be nil (uploads then 503).
func NewBrandHandler(repo repository.BrandRepository, store storage.Storage, auth fiber.Handler) *BrandHandler {
	return &BrandHandler{repo: repo, storage: store, auth: auth}
}

// SetActivityRecorder wires the CON-125 activity recorder. nil is a no-op.
func (h *BrandHandler) SetActivityRecorder(r *activity.Recorder) { h.activity = r }

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
	// Facts ledger (CON-316).
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
	normalizeVoice(&v)
	if err := validateVoice(&v); err != nil {
		return err
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	v.ID = id
	// FR5: summary is server-owned, generated off the samples. Generation (a
	// genkit River job) is a CON-228 follow-up; until then it is withdrawn to ""
	// — the honest rendering of a voice with nothing read back off it yet.
	v.Summary = ""
	now := brandNow()
	v.CreatedAt, v.UpdatedAt = now, now
	if err := h.repo.CreateVoice(reqCtx(c), &v); err != nil {
		return err
	}
	h.recordActivity(c, "brand_voice_created", activity.WithEntity("brand_voice", v.ID))
	v.Usage = models.BrandUsage{} // derived; zero until CON-245
	return c.Status(fiber.StatusCreated).JSON(&v)
}

func (h *BrandHandler) UpdateVoice(c *fiber.Ctx) error {
	var v models.BrandVoice
	if err := c.BodyParser(&v); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	v.ID = c.Params("id")
	normalizeVoice(&v)
	if err := validateVoice(&v); err != nil {
		return err
	}
	v.Summary = "" // FR5: withdrawn; regeneration is a follow-up.
	v.UpdatedAt = brandNow()
	if err := h.repo.UpdateVoice(reqCtx(c), &v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "voice not found")
		}
		return err
	}
	h.recordActivity(c, "brand_voice_updated", activity.WithEntity("brand_voice", v.ID))
	v.Usage = models.BrandUsage{}
	return c.JSON(&v)
}

func (h *BrandHandler) DeleteVoice(c *fiber.Ctx) error {
	deleted, err := h.repo.DeleteVoice(reqCtx(c), c.Params("id"))
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
	normalizeAudience(&a)
	if err := validateAudience(&a); err != nil {
		return err
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	a.ID = id
	a.Summary = "" // FR5
	now := brandNow()
	a.CreatedAt, a.UpdatedAt = now, now
	if err := h.repo.CreateAudience(reqCtx(c), &a); err != nil {
		return err
	}
	h.recordActivity(c, "brand_audience_created", activity.WithEntity("brand_audience", a.ID))
	a.Usage = models.BrandUsage{}
	return c.Status(fiber.StatusCreated).JSON(&a)
}

func (h *BrandHandler) UpdateAudience(c *fiber.Ctx) error {
	var a models.BrandAudience
	if err := c.BodyParser(&a); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	a.ID = c.Params("id")
	normalizeAudience(&a)
	if err := validateAudience(&a); err != nil {
		return err
	}
	a.Summary = "" // FR5
	a.UpdatedAt = brandNow()
	if err := h.repo.UpdateAudience(reqCtx(c), &a); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "audience not found")
		}
		return err
	}
	h.recordActivity(c, "brand_audience_updated", activity.WithEntity("brand_audience", a.ID))
	a.Usage = models.BrandUsage{}
	return c.JSON(&a)
}

func (h *BrandHandler) DeleteAudience(c *fiber.Ctx) error {
	deleted, err := h.repo.DeleteAudience(reqCtx(c), c.Params("id"))
	if err != nil {
		return err
	}
	if !deleted {
		return fiber.NewError(fiber.StatusNotFound, "audience not found")
	}
	h.recordActivity(c, "brand_audience_deleted", activity.WithEntity("brand_audience", c.Params("id")))
	return c.SendStatus(fiber.StatusNoContent)
}

// ── Guardrails (singleton) ──────────────────────────────────────────────────

// guardrailsRequest is the PUT /api/brand/guardrails body. facts is
// presence-aware (CON-316 FR6): omitted leaves the ledger alone, present
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
		facts = normalizeFactStatements(req.Facts.orZero())
	}
	normalizeGuardrails(&g)
	if err := validateGuardrails(&g, facts); err != nil {
		return err
	}
	existing, err := h.repo.GetGuardrails(reqCtx(c))
	if err != nil {
		return err
	}
	// FR8: an unchanged save must not restamp updated_at — this is the one
	// section where "when was this last checked" is a real question. With
	// facts present, unchanged also means the ledger already holds exactly
	// those statements.
	if existing != nil && guardrailsEqual(existing, &g) &&
		(facts == nil || sameStatementSet(existing.Facts, facts)) {
		return c.JSON(existing)
	}
	now := brandNow()
	g.UpdatedAt = now
	if existing == nil {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		g.ID = id
		g.CreatedAt = now
	} else {
		// The upsert is ON CONFLICT DO UPDATE and does not touch id /
		// created_at, so carry the stored ones through.
		g.ID = existing.ID
		g.CreatedAt = existing.CreatedAt
	}
	rec, err := h.repo.SaveGuardrails(reqCtx(c), &g, facts, sessionUserID(c))
	if err != nil {
		return factError(err)
	}
	var opts []activity.Option
	if facts != nil {
		opts = append(opts, activity.WithPayload(map[string]any{"facts_reconciled": rec}))
	}
	h.recordActivity(c, "brand_guardrails_updated", opts...)
	saved, err := h.repo.GetGuardrails(reqCtx(c))
	if err != nil {
		return err
	}
	return c.JSON(saved)
}

// DeleteGuardrails removes the rules. The facts ledger is its own section now
// and is not touched (CON-316 FR6), nor is a previous stance brought back.
func (h *BrandHandler) DeleteGuardrails(c *fiber.Ctx) error {
	deleted, err := h.repo.DeleteGuardrails(reqCtx(c))
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
		if errors.Is(err, sql.ErrNoRows) {
			return fiber.NewError(fiber.StatusNotFound, "template not found")
		}
		return err
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

// Size caps (§9): prose slots invite a pasted brand book, and every one is read
// on each generation turn once CON-245 lands. Tune there.
const (
	maxBrandSampleBytes     = 2 << 10 // 2 KB
	maxBrandSamples         = 20
	maxGuardrailStmtBytes   = 1 << 10 // 1 KB
	maxGuardrailStmts       = 50
	maxBrandBannedWords     = 100
	maxBrandDisclaimerBytes = 2 << 10 // 2 KB
	maxBrandLineBytes       = 1 << 10 // 1 KB
	maxBrandChannelNotes    = 50
)

var (
	voiceEmoji     = map[string]bool{"never": true, "sparingly": true, "freely": true}
	voiceHashtags  = map[string]bool{"never": true, "few": true, "many": true}
	voiceFormality = map[string]bool{"casual": true, "neutral": true, "formal": true}
	voicePerson    = map[string]bool{"i": true, "we": true, "third": true}
	voiceLength    = map[string]bool{"short": true, "medium": true, "long": true}
	templateRoles  = map[string]bool{"foreground": true, "background": true}
	logoJobs       = map[string]bool{"profile": true, "watermark": true, "mark": true}

	hexColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

func unprocessable(format string, args ...any) error {
	return fiber.NewError(fiber.StatusUnprocessableEntity, fmt.Sprintf(format, args...))
}

// checkEnum accepts an empty value (unset) or a member of the set.
func checkEnum(field, val string, set map[string]bool) error {
	if val == "" || set[val] {
		return nil
	}
	return unprocessable("invalid %s: %q", field, val)
}

func normalizeVoice(v *models.BrandVoice) {
	if v.Samples == nil {
		v.Samples = models.StringSlice{}
	}
	if v.ChannelNotes == nil {
		v.ChannelNotes = models.StringMap{}
	}
	if v.Origin.Kind == "" {
		v.Origin = models.BrandOrigin{Kind: "blank"}
	}
}

func validateVoice(v *models.BrandVoice) error {
	if strings.TrimSpace(v.Name) == "" {
		return unprocessable("name is required")
	}
	if len(v.WhenToUse) > maxBrandLineBytes {
		return unprocessable("whenToUse exceeds %d KB", maxBrandLineBytes>>10)
	}
	if len(v.Samples) > maxBrandSamples {
		return unprocessable("at most %d samples", maxBrandSamples)
	}
	for _, s := range v.Samples {
		if len(s) > maxBrandSampleBytes {
			return unprocessable("a sample exceeds %d KB", maxBrandSampleBytes>>10)
		}
	}
	if len(v.ChannelNotes) > maxBrandChannelNotes {
		return unprocessable("at most %d channel notes", maxBrandChannelNotes)
	}
	for platform, note := range v.ChannelNotes {
		if len(note) > maxBrandLineBytes {
			return unprocessable("channel note for %q exceeds %d KB", platform, maxBrandLineBytes>>10)
		}
	}
	if err := checkEnum("rules.emoji", v.Rules.Emoji, voiceEmoji); err != nil {
		return err
	}
	if err := checkEnum("rules.hashtags", v.Rules.Hashtags, voiceHashtags); err != nil {
		return err
	}
	if err := checkEnum("rules.formality", v.Rules.Formality, voiceFormality); err != nil {
		return err
	}
	if err := checkEnum("rules.person", v.Rules.Person, voicePerson); err != nil {
		return err
	}
	if err := checkEnum("rules.length", v.Rules.Length, voiceLength); err != nil {
		return err
	}
	return nil
}

func normalizeAudience(a *models.BrandAudience) {
	if a.Origin.Kind == "" {
		a.Origin = models.BrandOrigin{Kind: "blank"}
	}
}

func validateAudience(a *models.BrandAudience) error {
	if strings.TrimSpace(a.Name) == "" {
		return unprocessable("name is required")
	}
	for _, line := range []string{a.Who, a.ReadsOn, a.ScrollsPastWhen, a.BelievesWhen} {
		if len(line) > maxBrandLineBytes {
			return unprocessable("a field exceeds %d KB", maxBrandLineBytes>>10)
		}
	}
	return nil
}

func normalizeGuardrails(g *models.BrandGuardrails) {
	if g.MayClaim == nil {
		g.MayClaim = models.StringSlice{}
	}
	if g.NeverClaim == nil {
		g.NeverClaim = models.StringSlice{}
	}
	if g.BannedWords == nil {
		g.BannedWords = models.StringSlice{}
	}
}

// validateGuardrails checks the rules, and facts when the key was present
// (non-nil). Rules and facts all empty is a 422: DELETE is the way to clear
// the rules.
func validateGuardrails(g *models.BrandGuardrails, facts []string) error {
	if len(facts) == 0 && len(g.MayClaim) == 0 && len(g.NeverClaim) == 0 &&
		len(g.BannedWords) == 0 && strings.TrimSpace(g.Disclaimer) == "" {
		return unprocessable("guardrails are empty — use DELETE to clear the section")
	}
	if len(facts) > repository.MaxBrandFacts {
		return unprocessable("at most %d facts per workspace", repository.MaxBrandFacts)
	}
	for _, s := range facts {
		if len(s) > maxGuardrailStmtBytes {
			return unprocessable("a statement exceeds %d KB", maxGuardrailStmtBytes>>10)
		}
	}
	for _, list := range []models.StringSlice{g.MayClaim, g.NeverClaim} {
		if len(list) > maxGuardrailStmts {
			return unprocessable("at most %d statements per list", maxGuardrailStmts)
		}
		for _, s := range list {
			if len(s) > maxGuardrailStmtBytes {
				return unprocessable("a statement exceeds %d KB", maxGuardrailStmtBytes>>10)
			}
		}
	}
	if len(g.BannedWords) > maxBrandBannedWords {
		return unprocessable("at most %d banned words", maxBrandBannedWords)
	}
	if len(g.Disclaimer) > maxBrandDisclaimerBytes {
		return unprocessable("disclaimer exceeds %d KB", maxBrandDisclaimerBytes>>10)
	}
	return nil
}

// normalizeFactStatements trims, drops blanks and de-duplicates, keeping the
// first occurrence. Never nil, so a present-but-empty list still reconciles
// (to an empty ledger).
func normalizeFactStatements(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sameStatementSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

func guardrailsEqual(a, b *models.BrandGuardrails) bool {
	return a.Disclaimer == b.Disclaimer &&
		slices.Equal(a.MayClaim, b.MayClaim) &&
		slices.Equal(a.NeverClaim, b.NeverClaim) &&
		slices.Equal(a.BannedWords, b.BannedWords)
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
