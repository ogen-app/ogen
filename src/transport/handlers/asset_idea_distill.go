package handlers

import (
	"context"
	"database/sql"
	"errors"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

const (
	codeAnalysisNotReady  = "analysis_not_ready"
	codeDistillInProgress = "distill_in_progress"
)

// IdeaDistiller queues and lists requests for more ideas from an asset.
// *assetanalysis.Service implements it.
type IdeaDistiller interface {
	RequestDistill(ctx context.Context, in assetanalysis.DistillInput) (*models.IdeaDistillRun, error)
	ListDistills(ctx context.Context, tenantID, assetID string, campaignID *string) ([]models.IdeaDistillRun, error)
}

// AssetIdeaDistillHandler serves requests for more ideas from an analysed
// asset, through its workspace lens or a campaign lens.
type AssetIdeaDistillHandler struct {
	assets    repository.AssetRepository
	distiller IdeaDistiller
	auth      fiber.Handler
	activity  *activity.Recorder
}

func NewAssetIdeaDistillHandler(assets repository.AssetRepository, distiller IdeaDistiller, auth fiber.Handler, rec *activity.Recorder) *AssetIdeaDistillHandler {
	return &AssetIdeaDistillHandler{assets: assets, distiller: distiller, auth: auth, activity: rec}
}

func (h *AssetIdeaDistillHandler) Register(app *fiber.App) {
	app.Post("/api/content-bank/assets/:id/ideas/distill", h.auth, h.Request)
	app.Get("/api/content-bank/assets/:id/ideas/distills", h.auth, h.List)
}

// distillRequest is the POST body; both fields are optional.
type distillRequest struct {
	// Focus steers the batch, e.g. "objections from CFOs"; at most 300
	// characters.
	Focus string `json:"focus"`
	// Count is how many ideas to ask for, 3 to 10; 0 or absent is 5.
	Count int `json:"count"`
}

type distillRunResponse struct {
	Run *models.IdeaDistillRun `json:"run"`
}

type distillRunsResponse struct {
	Runs []models.IdeaDistillRun `json:"runs"`
}

// Request godoc
// @Summary      Distill more ideas from an asset
// @Description  Queues a run that adds new ideas to one lens of an analysed asset: the workspace lens, or the campaign_id lens. The model is shown every idea already in scope with its verdict and steers clear of them. Progress arrives as asset.ideas.distill.updated events; the ideas then read back from the lens and /api/ideas.
// @Tags         assets
// @Accept       json
// @Produce      json
// @Security     CookieAuth
// @Param        id           path   string          true   "Asset Sqid"
// @Param        campaign_id  query  string          false  "Campaign Sqid of the lens; absent for the workspace lens"
// @Param        body         body   distillRequest  false  "Focus and count"
// @Success      202  {object}  distillRunResponse
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      402  {object}  map[string]any     "entitlement_exceeded: idea_distills_per_month is used up"
// @Failure      403  {object}  map[string]string  "feature_not_available: asset_analysis is off for the plan"
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]any     "analysis_not_ready, or distill_in_progress with the active run"
// @Failure      422  {object}  map[string]string  "the asset type is never analysed"
// @Failure      503  {object}  map[string]string
// @Router       /api/content-bank/assets/{id}/ideas/distill [post]
func (h *AssetIdeaDistillHandler) Request(c *fiber.Ctx) error {
	ctx, assetID := reqCtx(c), c.Params("id")
	if err := h.analysable(ctx, assetID); err != nil {
		return err
	}
	var body distillRequest
	if len(c.Body()) > 0 {
		if err := c.BodyParser(&body); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid body")
		}
	}
	tenantID, _ := tenantctx.From(ctx)
	campaignID := campaignQuery(c)
	run, err := h.distiller.RequestDistill(ctx, assetanalysis.DistillInput{
		TenantID: tenantID, AssetID: assetID, CampaignID: campaignID,
		Focus: body.Focus, Count: body.Count, UserID: sessionUserID(c),
	})
	if inputErr, ok := errors.AsType[*assetanalysis.DistillInputError](err); ok {
		return fiber.NewError(fiber.StatusBadRequest, inputErr.Msg)
	}
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return fiber.NewError(fiber.StatusNotFound, "campaign not found or the asset is not in it")
	case errors.Is(err, assetanalysis.ErrAnalysisNotReady):
		return rejectCoded(c, fiber.StatusConflict, codeAnalysisNotReady, "only an asset whose analysis is ready can be distilled further", nil)
	case errors.Is(err, assetanalysis.ErrDistillInProgress):
		return rejectCoded(c, fiber.StatusConflict, codeDistillInProgress, "more ideas are already being found for this asset", fiber.Map{"run": run})
	case errors.Is(err, assetanalysis.ErrUnavailable):
		return fiber.NewError(fiber.StatusServiceUnavailable, "asset analysis is not available")
	default:
		return err
	}
	if h.activity != nil {
		h.activity.Record(ctx, activity.CategoryIdea, "ideas_distill_requested",
			activity.WithSource(activity.SourceAPI),
			activity.WithEntity("asset", assetID),
			activity.WithPayload(map[string]any{"campaign_id": campaignID, "count": run.Count, "focused": run.Focus != ""}),
		)
	}
	return c.Status(fiber.StatusAccepted).JSON(distillRunResponse{Run: run})
}

// List godoc
// @Summary      List an asset lens's distill runs
// @Description  The latest runs of the workspace lens, or of the campaign_id lens, newest first (at most 20). A run still pending or running is the one in flight.
// @Tags         assets
// @Produce      json
// @Security     CookieAuth
// @Param        id           path   string  true   "Asset Sqid"
// @Param        campaign_id  query  string  false  "Campaign Sqid of the lens; absent for the workspace lens"
// @Success      200  {object}  distillRunsResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      422  {object}  map[string]string  "the asset type is never analysed"
// @Router       /api/content-bank/assets/{id}/ideas/distills [get]
func (h *AssetIdeaDistillHandler) List(c *fiber.Ctx) error {
	ctx, assetID := reqCtx(c), c.Params("id")
	if err := h.analysable(ctx, assetID); err != nil {
		return err
	}
	tenantID, _ := tenantctx.From(ctx)
	runs, err := h.distiller.ListDistills(ctx, tenantID, assetID, campaignQuery(c))
	if err != nil {
		return err
	}
	return c.JSON(distillRunsResponse{Runs: runs})
}

// analysable is 404 for an unknown asset and 422 for a type that is never
// analysed.
func (h *AssetIdeaDistillHandler) analysable(ctx context.Context, assetID string) error {
	asset, err := h.assets.GetByID(ctx, assetID)
	if err != nil {
		return notFound(err, "asset not found")
	}
	if !models.IsAnalyzableAssetType(asset.Type) {
		return fiber.NewError(fiber.StatusUnprocessableEntity, "this asset type is not analysed")
	}
	return nil
}
