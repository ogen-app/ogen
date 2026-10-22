package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

const (
	codeDistillRunNotReady      = "run_not_ready"
	codeDistillRunCommitted     = "run_committed"
	codeDistillRefineLimit      = "refine_limit"
	codeScrapingUnavailable     = "scraping_unavailable"
	codeBrandDistillUnavailable = "distill_unavailable"
)

// BrandDistiller runs brand distillations. *branddistill.Service implements
// it.
type BrandDistiller interface {
	Request(ctx context.Context, in branddistill.RequestInput) (*models.BrandDistillRun, error)
	Get(ctx context.Context, tenantID, runID string) (*models.BrandDistillRun, error)
	List(ctx context.Context, tenantID string) ([]models.BrandDistillRun, error)
	Refine(ctx context.Context, tenantID, runID string, userID *string, answers []branddistill.Answer) (*models.BrandDistillRun, error)
	Commit(ctx context.Context, in branddistill.CommitInput) (*branddistill.CommitResult, error)
	Discard(ctx context.Context, tenantID, runID string) (*models.BrandDistillRun, error)
}

// BrandDistillHandler serves /api/brand/distills: reading a brand off a
// website, documents or answers, and saving what the user keeps.
type BrandDistillHandler struct {
	svc      BrandDistiller
	brands   repository.BrandRepository
	auth     fiber.Handler
	activity *activity.Recorder
}

func NewBrandDistillHandler(svc BrandDistiller, brands repository.BrandRepository, auth fiber.Handler, rec *activity.Recorder) *BrandDistillHandler {
	return &BrandDistillHandler{svc: svc, brands: brands, auth: auth, activity: rec}
}

func (h *BrandDistillHandler) Register(app *fiber.App) {
	g := app.Group("/api/brand/distills")
	g.Post("/", h.auth, h.Request)
	g.Get("/", h.auth, h.List)
	g.Get("/starter-questions", h.auth, h.StarterQuestions)
	g.Get("/:id", h.auth, h.Get)
	g.Post("/:id/answers", h.auth, h.Answers)
	g.Post("/:id/commit", h.auth, h.Commit)
	g.Delete("/:id", h.auth, h.Discard)
}

func (h *BrandDistillHandler) record(c *fiber.Ctx, typ string, opts ...activity.Option) {
	if h.activity == nil {
		return
	}
	h.activity.Record(reqCtx(c), activity.CategoryBrand, typ, append([]activity.Option{activity.WithSource(activity.SourceAPI)}, opts...)...)
}

type distillAnswerBody struct {
	QuestionID string `json:"questionId"`
	Question   string `json:"question"`
	Answer     string `json:"answer"`
}

// brandDistillRequest starts a run. Give at least one of the three.
type brandDistillRequest struct {
	// WebsiteURL is read: its homepage and up to nine more of its pages,
	// imported as URL assets tagged "Brand source".
	WebsiteURL string `json:"websiteUrl"`
	// AssetIDs are read as they are; uploads still being processed are waited
	// for. At most 20.
	AssetIDs []string `json:"assetIds"`
	// Answers to starter or gap questions; blank answers are ignored. At most
	// 12.
	Answers []distillAnswerBody `json:"answers"`
}

type brandDistillAnswersRequest struct {
	Answers []distillAnswerBody `json:"answers"`
}

// brandDistillRun is a run on the wire: its proposal once ready, and why it
// failed when it did.
type brandDistillRun struct {
	*models.BrandDistillRun
	Failure *branddistill.Failure `json:"failure"`
}

func runView(run *models.BrandDistillRun) brandDistillRun {
	return brandDistillRun{BrandDistillRun: run, Failure: branddistill.FailureOf(run)}
}

type brandDistillRunsResponse struct {
	Runs []brandDistillRun `json:"runs"`
}

type brandDistillQuestionsResponse struct {
	Questions []models.DistillQuestion `json:"questions"`
}

func toAnswers(in []distillAnswerBody) []branddistill.Answer {
	out := make([]branddistill.Answer, 0, len(in))
	for _, a := range in {
		out = append(out, branddistill.Answer{QuestionID: a.QuestionID, Question: a.Question, Answer: a.Answer})
	}
	return out
}

// distillError answers the use case's refusals.
func distillError(c *fiber.Ctx, err error, run *models.BrandDistillRun) error {
	if in, ok := errors.AsType[*branddistill.InputError](err); ok {
		extra := fiber.Map{}
		if in.ProposalID != "" {
			extra["proposalId"] = in.ProposalID
		}
		return rejectCoded(c, fiber.StatusUnprocessableEntity, in.Code, in.Msg, extra)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fiber.NewError(fiber.StatusNotFound, "run not found")
	case errors.Is(err, branddistill.ErrInProgress):
		var open any
		if run != nil {
			open = runView(run)
		}
		return rejectCoded(c, fiber.StatusConflict, codeDistillInProgress, "your brand is already being read", fiber.Map{"run": open})
	case errors.Is(err, branddistill.ErrScrapingUnavailable):
		return rejectCoded(c, fiber.StatusConflict, codeScrapingUnavailable, "reading websites is not set up here", nil)
	case errors.Is(err, branddistill.ErrNotReady):
		return rejectCoded(c, fiber.StatusConflict, codeDistillRunNotReady, "the run has no proposal waiting", nil)
	case errors.Is(err, branddistill.ErrCommitted):
		return rejectCoded(c, fiber.StatusConflict, codeDistillRunCommitted, "the run was saved already", nil)
	case errors.Is(err, branddistill.ErrRefineLimit):
		return rejectCoded(c, fiber.StatusUnprocessableEntity, codeDistillRefineLimit, "no more answer rounds for this run", nil)
	case errors.Is(err, branddistill.ErrUnavailable):
		return rejectCoded(c, fiber.StatusServiceUnavailable, codeBrandDistillUnavailable, "reading your brand is not available", nil)
	}
	return err
}

// Request godoc
// @Summary      Read a brand off a website, documents or answers
// @Description  Queues a run that proposes voices, audiences, guardrail lines and facts from a website (its homepage and up to nine pages chosen from its site map, imported as URL assets tagged "Brand source"), any ready or still-ingesting assets, and/or answers to starter questions. Nothing is saved to the Brand until the proposal is committed. Progress arrives as brand.distill.updated events on topic entity:brand_distill:<id>.
// @Tags         brand
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        body  body      brandDistillRequest  true  "What to read"
// @Success      202   {object}  brandDistillRun
// @Failure      401   {object}  map[string]string
// @Failure      402   {object}  map[string]any     "entitlement_exceeded: brand_distills_per_month is used up"
// @Failure      403   {object}  map[string]string  "feature_not_available: brand_distill is off for the plan"
// @Failure      409   {object}  map[string]any     "distill_in_progress with the open run, or scraping_unavailable"
// @Failure      422   {object}  map[string]any     "empty_request, invalid_url, unknown_asset, asset_failed, too_many or invalid_answer"
// @Failure      503   {object}  map[string]any     "distill_unavailable"
// @Router       /api/brand/distills [post]
func (h *BrandDistillHandler) Request(c *fiber.Ctx) error {
	var body brandDistillRequest
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	ctx := reqCtx(c)
	tenantID, _ := tenantctx.From(ctx)
	run, err := h.svc.Request(ctx, branddistill.RequestInput{
		TenantID: tenantID, UserID: sessionUserID(c),
		WebsiteURL: body.WebsiteURL, AssetIDs: body.AssetIDs, Answers: toAnswers(body.Answers),
	})
	if err != nil {
		return distillError(c, err, run)
	}
	h.record(c, "brand_distill_requested", activity.WithEntity("brand_distill", run.ID), activity.WithPayload(map[string]any{
		"website": run.WebsiteURL != nil, "assets": len(run.Sources), "answers": len(run.Answers),
	}))
	return c.Status(fiber.StatusAccepted).JSON(runView(run))
}

// List godoc
// @Summary      List brand distillation runs
// @Description  The workspace's latest runs, newest first (at most 10), without their proposals. An open or ready run is the one to resume.
// @Tags         brand
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  brandDistillRunsResponse
// @Failure      401  {object}  map[string]string
// @Router       /api/brand/distills [get]
func (h *BrandDistillHandler) List(c *fiber.Ctx) error {
	ctx := reqCtx(c)
	tenantID, _ := tenantctx.From(ctx)
	runs, err := h.svc.List(ctx, tenantID)
	if err != nil {
		return err
	}
	out := brandDistillRunsResponse{Runs: make([]brandDistillRun, 0, len(runs))}
	for i := range runs {
		out.Runs = append(out.Runs, runView(&runs[i]))
	}
	return c.JSON(out)
}

// StarterQuestions godoc
// @Summary      Questions for a brand with nothing written down
// @Description  Seven fixed questions with stable ids; answer any of them in POST /api/brand/distills.
// @Tags         brand
// @Produce      json
// @Security     CookieAuth
// @Success      200  {object}  brandDistillQuestionsResponse
// @Failure      401  {object}  map[string]string
// @Router       /api/brand/distills/starter-questions [get]
func (h *BrandDistillHandler) StarterQuestions(c *fiber.Ctx) error {
	return c.JSON(brandDistillQuestionsResponse{Questions: branddistill.StarterQuestions()})
}

// Get godoc
// @Summary      Get a brand distillation run
// @Description  The run with its sources and, once ready, its proposal: every item with the passages or answers it rests on.
// @Tags         brand
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Run id"
// @Success      200  {object}  brandDistillRun
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/brand/distills/{id} [get]
func (h *BrandDistillHandler) Get(c *fiber.Ctx) error {
	ctx := reqCtx(c)
	tenantID, _ := tenantctx.From(ctx)
	run, err := h.svc.Get(ctx, tenantID, c.Params("id"))
	if err != nil {
		return distillError(c, err, nil)
	}
	return c.JSON(runView(run))
}

// Answers godoc
// @Summary      Answer a run's questions
// @Description  Queues a new run that composes the proposal again from the same passages and every answer so far, and supersedes this one (childRunId). Not charged; at most 3 rounds per run.
// @Tags         brand
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                      true  "Run id"
// @Param        body  body      brandDistillAnswersRequest  true  "Answers"
// @Success      202   {object}  brandDistillRun
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]any  "run_not_ready, run_committed or distill_in_progress"
// @Failure      422   {object}  map[string]any  "empty_request, invalid_answer or refine_limit"
// @Router       /api/brand/distills/{id}/answers [post]
func (h *BrandDistillHandler) Answers(c *fiber.Ctx) error {
	var body brandDistillAnswersRequest
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	ctx := reqCtx(c)
	tenantID, _ := tenantctx.From(ctx)
	child, err := h.svc.Refine(ctx, tenantID, c.Params("id"), sessionUserID(c), toAnswers(body.Answers))
	if err != nil {
		return distillError(c, err, child)
	}
	h.record(c, "brand_distill_refined", activity.WithEntity("brand_distill", child.ID), activity.WithPayload(map[string]any{
		"parent_run_id": c.Params("id"), "answers": len(child.Answers),
	}))
	return c.Status(fiber.StatusAccepted).JSON(runView(child))
}

type commitLineBody struct {
	ProposalID string `json:"proposalId"`
	Text       string `json:"text"`
}

type commitVoiceBody struct {
	ProposalID   string            `json:"proposalId"`
	Name         string            `json:"name"`
	WhenToUse    string            `json:"whenToUse"`
	IsDefault    bool              `json:"isDefault"`
	Samples      []string          `json:"samples"`
	Rules        models.VoiceRules `json:"rules"`
	ChannelNotes map[string]string `json:"channelNotes"`
}

type commitAudienceBody struct {
	ProposalID      string `json:"proposalId"`
	Name            string `json:"name"`
	Who             string `json:"who"`
	ReadsOn         string `json:"readsOn"`
	ScrollsPastWhen string `json:"scrollsPastWhen"`
	BelievesWhen    string `json:"believesWhen"`
}

type commitGuardrailsBody struct {
	MayClaim    []commitLineBody `json:"mayClaim"`
	NeverClaim  []commitLineBody `json:"neverClaim"`
	BannedWords []commitLineBody `json:"bannedWords"`
	// Disclaimer replaces the workspace's own; leave it out to keep yours.
	Disclaimer *commitLineBody `json:"disclaimer"`
}

type commitFactBody struct {
	ProposalID string  `json:"proposalId"`
	Statement  string  `json:"statement"`
	Subject    string  `json:"subject"`
	Kind       string  `json:"kind"`
	ExpiresAt  *string `json:"expiresAt"`
	Source     string  `json:"source"`
}

// brandDistillCommitRequest is what the user keeps, edits included. Items
// left out are not saved.
type brandDistillCommitRequest struct {
	Voices     []commitVoiceBody    `json:"voices"`
	Audiences  []commitAudienceBody `json:"audiences"`
	Guardrails commitGuardrailsBody `json:"guardrails"`
	Facts      []commitFactBody     `json:"facts"`
}

type appliedItem struct {
	ProposalID string `json:"proposalId"`
	ID         string `json:"id"`
}

type brandDistillApplied struct {
	Voices     []appliedItem `json:"voices"`
	Audiences  []appliedItem `json:"audiences"`
	Guardrails []string      `json:"guardrails"`
	Facts      []appliedItem `json:"facts"`
}

type brandDistillCommitResponse struct {
	Brand   *models.BrandData        `json:"brand"`
	Applied brandDistillApplied      `json:"applied"`
	Skipped []repository.SkippedItem `json:"skipped"`
}

func lines(in []commitLineBody) []branddistill.CommitLine {
	out := make([]branddistill.CommitLine, 0, len(in))
	for _, l := range in {
		out = append(out, branddistill.CommitLine{ProposalID: l.ProposalID, Text: l.Text})
	}
	return out
}

func appliedItems(ids map[string]string, order []string) []appliedItem {
	out := []appliedItem{}
	for _, pid := range order {
		if id, ok := ids[pid]; ok {
			out = append(out, appliedItem{ProposalID: pid, ID: id})
		}
	}
	return out
}

// commitInput turns the body into the use case's input; a bad date is a 422.
func commitInput(body brandDistillCommitRequest) (branddistill.CommitInput, error) {
	in := branddistill.CommitInput{
		MayClaim: lines(body.Guardrails.MayClaim), NeverClaim: lines(body.Guardrails.NeverClaim), BannedWords: lines(body.Guardrails.BannedWords),
	}
	for _, v := range body.Voices {
		in.Voices = append(in.Voices, branddistill.CommitVoice{
			ProposalID: v.ProposalID, Name: v.Name, WhenToUse: v.WhenToUse, IsDefault: v.IsDefault,
			Samples: v.Samples, Rules: v.Rules, ChannelNotes: v.ChannelNotes,
		})
	}
	for _, a := range body.Audiences {
		in.Audiences = append(in.Audiences, branddistill.CommitAudience{
			ProposalID: a.ProposalID, Name: a.Name, Who: a.Who, ReadsOn: a.ReadsOn, ScrollsPastWhen: a.ScrollsPastWhen, BelievesWhen: a.BelievesWhen,
		})
	}
	if d := body.Guardrails.Disclaimer; d != nil {
		in.Disclaimer = &branddistill.CommitLine{ProposalID: d.ProposalID, Text: d.Text}
	}
	for _, f := range body.Facts {
		fact := branddistill.CommitFact{
			ProposalID: f.ProposalID, Statement: f.Statement, Subject: models.FactSubject(f.Subject), Kind: models.FactKind(f.Kind), Source: f.Source,
		}
		if f.ExpiresAt != nil && strings.TrimSpace(*f.ExpiresAt) != "" {
			d, err := models.ParseCalendarDate(strings.TrimSpace(*f.ExpiresAt))
			if err != nil {
				return in, &branddistill.InputError{Code: branddistill.CodeInvalidItem, Msg: "invalid expiresAt (want YYYY-MM-DD)", ProposalID: f.ProposalID}
			}
			fact.ExpiresAt = &d
		}
		in.Facts = append(in.Facts, fact)
	}
	return in, nil
}

// Commit godoc
// @Summary      Save what you keep of a proposal
// @Description  Adds the kept items, edits included, to the Brand in one transaction: voices (the workspace default only when it has none), audiences, guardrail lines merged into the existing lists, ledger facts, and the disclaimer only when sent. Nothing existing is changed or removed. Repeats and items over a cap come back in skipped. The run becomes committed.
// @Tags         brand
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id    path      string                     true  "Run id"
// @Param        body  body      brandDistillCommitRequest  true  "Kept items"
// @Success      200   {object}  brandDistillCommitResponse
// @Failure      401   {object}  map[string]string
// @Failure      404   {object}  map[string]string
// @Failure      409   {object}  map[string]any  "run_not_ready or run_committed"
// @Failure      422   {object}  map[string]any  "nothing_to_save, unknown_proposal_item or invalid_item, with proposalId"
// @Router       /api/brand/distills/{id}/commit [post]
func (h *BrandDistillHandler) Commit(c *fiber.Ctx) error {
	var body brandDistillCommitRequest
	if err := c.BodyParser(&body); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid body")
	}
	in, err := commitInput(body)
	if err != nil {
		return distillError(c, err, nil)
	}
	ctx := reqCtx(c)
	tenantID, _ := tenantctx.From(ctx)
	in.TenantID, in.RunID, in.UserID = tenantID, c.Params("id"), sessionUserID(c)
	res, err := h.svc.Commit(ctx, in)
	if err != nil {
		return distillError(c, err, nil)
	}
	brand, err := h.brands.GetAll(ctx)
	if err != nil {
		return err
	}
	out := brandDistillCommitResponse{
		Brand:   brand,
		Skipped: res.Result.Skipped,
		Applied: brandDistillApplied{Guardrails: res.Result.Guardrails},
	}
	if out.Skipped == nil {
		out.Skipped = []repository.SkippedItem{}
	}
	if out.Applied.Guardrails == nil {
		out.Applied.Guardrails = []string{}
	}
	var voiceIDs, audienceIDs, factIDs []string
	for _, v := range in.Voices {
		voiceIDs = append(voiceIDs, v.ProposalID)
	}
	for _, a := range in.Audiences {
		audienceIDs = append(audienceIDs, a.ProposalID)
	}
	for _, f := range in.Facts {
		factIDs = append(factIDs, f.ProposalID)
	}
	out.Applied.Voices = appliedItems(res.Result.Voices, voiceIDs)
	out.Applied.Audiences = appliedItems(res.Result.Audiences, audienceIDs)
	out.Applied.Facts = appliedItems(res.Result.Facts, factIDs)
	h.recordCommit(c, res, out.Applied)
	return c.JSON(out)
}

// recordCommit records the commit and each entry it created, naming the run.
func (h *BrandDistillHandler) recordCommit(c *fiber.Ctx, res *branddistill.CommitResult, applied brandDistillApplied) {
	runID := res.Run.ID
	withRun := activity.WithPayload(map[string]any{"distill_run_id": runID})
	for _, v := range applied.Voices {
		h.record(c, "brand_voice_created", activity.WithEntity("brand_voice", v.ID), withRun)
	}
	for _, a := range applied.Audiences {
		h.record(c, "brand_audience_created", activity.WithEntity("brand_audience", a.ID), withRun)
	}
	for _, f := range applied.Facts {
		h.record(c, "brand_fact_created", activity.WithEntity("brand_fact", f.ID), withRun)
	}
	if len(applied.Guardrails) > 0 {
		h.record(c, "brand_guardrails_updated", withRun)
	}
	if res.Result.StanceCleared {
		h.record(c, "brand_guardrails_stance_set", activity.WithPayload(map[string]any{"none": false, "distill_run_id": runID}))
	}
	h.record(c, "brand_distill_committed", activity.WithEntity("brand_distill", runID), activity.WithPayload(map[string]any{
		"voices": len(applied.Voices), "audiences": len(applied.Audiences), "guardrails": len(applied.Guardrails),
		"facts": len(applied.Facts), "skipped": len(res.Result.Skipped),
	}))
}

// Discard godoc
// @Summary      Discard a brand distillation run
// @Description  Closes a run that is not committed; an open run stops. A charged run that never finished goes back to the allowance.
// @Tags         brand
// @Security     CookieAuth
// @Param        id   path  string  true  "Run id"
// @Success      204
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]any  "run_committed"
// @Router       /api/brand/distills/{id} [delete]
func (h *BrandDistillHandler) Discard(c *fiber.Ctx) error {
	ctx := reqCtx(c)
	tenantID, _ := tenantctx.From(ctx)
	run, err := h.svc.Discard(ctx, tenantID, c.Params("id"))
	if err != nil {
		return distillError(c, err, nil)
	}
	h.record(c, "brand_distill_discarded", activity.WithEntity("brand_distill", run.ID))
	return c.SendStatus(fiber.StatusNoContent)
}
