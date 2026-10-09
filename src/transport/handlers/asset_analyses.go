package handlers

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

const codeAnalysisNotRetryable = "analysis_not_retryable"

// AnalysisRetrier re-queues a failed or skipped analysis.
// *assetanalysis.Service implements it.
type AnalysisRetrier interface {
	Retry(ctx context.Context, tenantID, assetID string, campaignID *string) error
}

// AssetAnalysesHandler serves the LLM analyses of assets: the workspace lens
// and one lens per campaign the asset serves, each with the ideas it
// distilled.
type AssetAnalysesHandler struct {
	analyses  repository.AssetAnalysisRepository
	ideas     repository.IdeaRepository
	assets    repository.AssetRepository
	campaigns repository.CampaignRepository
	retrier   AnalysisRetrier
	auth      fiber.Handler
}

func NewAssetAnalysesHandler(
	analyses repository.AssetAnalysisRepository,
	ideas repository.IdeaRepository,
	assets repository.AssetRepository,
	campaigns repository.CampaignRepository,
	retrier AnalysisRetrier,
	auth fiber.Handler,
) *AssetAnalysesHandler {
	return &AssetAnalysesHandler{analyses: analyses, ideas: ideas, assets: assets, campaigns: campaigns, retrier: retrier, auth: auth}
}

func (h *AssetAnalysesHandler) Register(app *fiber.App) {
	app.Get("/api/content-bank/assets/:id/analyses", h.auth, h.List)
	app.Get("/api/content-bank/assets/:id/analysis", h.auth, h.Get)
	app.Post("/api/content-bank/assets/:id/analysis/retry", h.auth, h.Retry)
	app.Get("/api/campaigns/:id/asset-analyses", h.auth, h.ListForCampaign)
}

// assetAnalysisResponse is one lens of an asset's analysis. The meeting
// fields are present for audio only and relevance for campaign lenses only.
type assetAnalysisResponse struct {
	ID            string                        `json:"id"`
	AssetID       string                        `json:"asset_id"`
	CampaignID    *string                       `json:"campaign_id"`
	CampaignName  *string                       `json:"campaign_name"`
	Status        models.AssetAnalysisStatus    `json:"status"          swaggertype:"string" enums:"pending,running,ready,failed,skipped"`
	Strategy      *models.AssetAnalysisStrategy `json:"strategy"        swaggertype:"string" enums:"full,map_reduce" extensions:"x-nullable"`
	Summary       string                        `json:"summary"`
	Language      string                        `json:"language"`
	KeyPoints     []string                      `json:"key_points"`
	Topics        []string                      `json:"topics"`
	Entities      []models.AnalysisEntity       `json:"entities"`
	Participants  []models.AnalysisParticipant  `json:"participants,omitempty"`
	Commitments   []models.AnalysisCommitment   `json:"commitments,omitempty"`
	Decisions     []models.AnalysisDecision     `json:"decisions,omitempty"`
	OpenQuestions []string                      `json:"open_questions,omitempty"`
	Relevance     *models.AnalysisRelevance     `json:"relevance"`
	PartialSource bool                          `json:"partial_source"`
	Truncated     bool                          `json:"truncated"`
	FailureCode   string                        `json:"failure_code"`
	FailureReason string                        `json:"failure_reason"`
	AnalyzedAt    *time.Time                    `json:"analyzed_at"`
	// Ideas are embedded on asset reads and left out of the campaign list.
	Ideas []models.Idea `json:"ideas,omitempty"`
}

type assetAnalysesResponse struct {
	Analyses []assetAnalysisResponse `json:"analyses"`
}

func toAnalysisResponse(a models.AssetAnalysis) assetAnalysisResponse {
	r := a.Result
	return assetAnalysisResponse{
		ID: a.ID, AssetID: a.AssetID, CampaignID: a.CampaignID, CampaignName: a.CampaignName,
		Status: a.Status, Strategy: a.Strategy, Summary: a.Summary, Language: a.Language,
		KeyPoints: nonNil(r.KeyPoints), Topics: nonNil(r.Topics), Entities: nonNil(r.Entities),
		Participants: r.Participants, Commitments: r.Commitments, Decisions: r.Decisions,
		OpenQuestions: r.OpenQuestions, Relevance: r.Relevance,
		PartialSource: a.PartialSource, Truncated: a.Truncated,
		FailureCode: a.FailureCode, FailureReason: a.FailureReason, AnalyzedAt: a.AnalyzedAt,
	}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// List godoc
// @Summary      List an asset's analyses
// @Description  The workspace lens first, then one lens per live campaign the asset serves, each with the ideas it distilled.
// @Tags         assets
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Asset Sqid"
// @Success      200  {object}  assetAnalysesResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/content-bank/assets/{id}/analyses [get]
func (h *AssetAnalysesHandler) List(c *fiber.Ctx) error {
	ctx, assetID := reqCtx(c), c.Params("id")
	if _, err := h.assets.GetByID(ctx, assetID); err != nil {
		return notFound(err, "asset not found")
	}
	lenses, err := h.analyses.ListByAsset(ctx, assetID)
	if err != nil {
		return err
	}
	out, err := h.withIdeas(ctx, assetID, lenses)
	if err != nil {
		return err
	}
	return c.JSON(assetAnalysesResponse{Analyses: out})
}

// Get godoc
// @Summary      Get one analysis lens of an asset
// @Description  Without campaign_id, the workspace lens; with it, that campaign's lens. Ideas are embedded.
// @Tags         assets
// @Produce      json
// @Security     CookieAuth
// @Param        id           path      string  true   "Asset Sqid"
// @Param        campaign_id  query     string  false  "Campaign Sqid"
// @Success      200  {object}  assetAnalysisResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/content-bank/assets/{id}/analysis [get]
func (h *AssetAnalysesHandler) Get(c *fiber.Ctx) error {
	ctx, assetID := reqCtx(c), c.Params("id")
	lens, err := h.analyses.GetLens(ctx, assetID, campaignQuery(c))
	if err != nil {
		return notFound(err, "analysis not found")
	}
	out, err := h.withIdeas(ctx, assetID, []models.AssetAnalysis{*lens})
	if err != nil {
		return err
	}
	return c.JSON(out[0])
}

// Retry godoc
// @Summary      Retry a failed or skipped analysis
// @Description  Re-queues the lens (the workspace lens, or campaign_id's). Only a failed or skipped lens can be retried.
// @Tags         assets
// @Produce      json
// @Security     CookieAuth
// @Param        id           path      string  true   "Asset Sqid"
// @Param        campaign_id  query     string  false  "Campaign Sqid"
// @Success      202
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      409  {object}  map[string]string  "analysis_not_retryable"
// @Failure      422  {object}  map[string]string  "the asset type is never analysed"
// @Failure      503  {object}  map[string]string
// @Router       /api/content-bank/assets/{id}/analysis/retry [post]
func (h *AssetAnalysesHandler) Retry(c *fiber.Ctx) error {
	ctx, assetID := reqCtx(c), c.Params("id")
	asset, err := h.assets.GetByID(ctx, assetID)
	if err != nil {
		return notFound(err, "asset not found")
	}
	if !models.IsAnalyzableAssetType(asset.Type) {
		return fiber.NewError(fiber.StatusUnprocessableEntity, "this asset type is not analysed")
	}
	tenantID, _ := tenantctx.From(ctx)
	err = h.retrier.Retry(ctx, tenantID, assetID, campaignQuery(c))
	switch {
	case err == nil:
		return c.SendStatus(fiber.StatusAccepted)
	case errors.Is(err, sql.ErrNoRows):
		return fiber.NewError(fiber.StatusNotFound, "analysis not found")
	case errors.Is(err, assetanalysis.ErrNotRetryable):
		return rejectCoded(c, fiber.StatusConflict, codeAnalysisNotRetryable, "only a failed or skipped analysis can be retried", nil)
	case errors.Is(err, assetanalysis.ErrUnavailable):
		return fiber.NewError(fiber.StatusServiceUnavailable, "asset analysis is not available")
	}
	return err
}

// ListForCampaign godoc
// @Summary      List a campaign's asset analyses
// @Description  One lens per asset attached to the campaign, most relevant first. Ideas are not embedded; read them from /api/ideas?campaign_id=.
// @Tags         campaigns
// @Produce      json
// @Security     CookieAuth
// @Param        id   path      string  true  "Campaign Sqid"
// @Success      200  {object}  assetAnalysesResponse
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Router       /api/campaigns/{id}/asset-analyses [get]
func (h *AssetAnalysesHandler) ListForCampaign(c *fiber.Ctx) error {
	ctx, campaignID := reqCtx(c), c.Params("id")
	if _, err := h.campaigns.GetByID(ctx, campaignID); err != nil {
		return notFound(err, "campaign not found")
	}
	lenses, err := h.analyses.ListByCampaign(ctx, campaignID)
	if err != nil {
		return err
	}
	out := make([]assetAnalysisResponse, 0, len(lenses))
	for _, l := range lenses {
		out = append(out, toAnalysisResponse(l))
	}
	return c.JSON(assetAnalysesResponse{Analyses: out})
}

// withIdeas renders lenses with the ideas each distilled, in one ideas read.
func (h *AssetAnalysesHandler) withIdeas(ctx context.Context, assetID string, lenses []models.AssetAnalysis) ([]assetAnalysisResponse, error) {
	ideas, err := h.ideas.List(ctx, repository.IdeaListFilter{AssetID: assetID})
	if err != nil {
		return nil, err
	}
	byLens := map[string][]models.Idea{}
	for _, idea := range ideas {
		if idea.AssetAnalysisID != nil {
			byLens[*idea.AssetAnalysisID] = append(byLens[*idea.AssetAnalysisID], idea)
		}
	}
	out := make([]assetAnalysisResponse, 0, len(lenses))
	for _, l := range lenses {
		r := toAnalysisResponse(l)
		r.Ideas = nonNil(byLens[l.ID])
		out = append(out, r)
	}
	return out, nil
}

// campaignQuery reads the optional campaign_id lens selector.
func campaignQuery(c *fiber.Ctx) *string {
	if id := c.Query("campaign_id"); id != "" {
		return &id
	}
	return nil
}
