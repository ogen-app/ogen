package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
)

const (
	defaultChunkPageSize = 100
	maxChunkPageSize     = 500
)

// AssetChunkLister pages an asset's chunks without their embeddings (CON-312).
// Implemented by repository.AssetChunksRepository.
type AssetChunkLister interface {
	ListPageByAssetID(ctx context.Context, assetID string, offset, limit int) ([]models.AssetChunk, int, error)
}

// SetChunkLister wires the chunk view (CON-312). Nil leaves GET /:id/chunks
// answering 409.
func (h *AssetsHandler) SetChunkLister(l AssetChunkLister) { h.chunks = l }

// assetChunksResponse is one page of an asset's searchable chunks.
type assetChunksResponse struct {
	Chunks []models.AssetChunk `json:"chunks"`
	Total  int                 `json:"total"`
	Offset int                 `json:"offset"`
	Limit  int                 `json:"limit"`
}

// Chunks godoc
// @Summary      List asset chunks
// @Description  Returns one page of an asset's searchable chunks in chunk order, without embeddings (CON-312). Chunks from service-ingested assets carry `source_label` (a citation like "Slide 4", "Sheet 'Q3' rows 10–24", "1:05–1:40", "Region 2") and `source_anchor` (its structured location: page/slide/sheet/section/email/time/image, with `start_ms`/`end_ms` for audio and a normalized `bbox` for image regions). Both are absent on markdown and URL chunks.
// @Tags         content-bank
// @Produce      json
// @Security     CookieAuth
// @Param        id      path      string  true   "Asset Sqid"
// @Param        offset  query     int     false  "Chunks to skip (default 0)"
// @Param        limit   query     int     false  "Page size (default 100, max 500)"
// @Success      200     {object}  assetChunksResponse
// @Failure      401     {object}  map[string]string
// @Failure      404     {object}  map[string]string
// @Router       /api/content-bank/assets/{id}/chunks [get]
func (h *AssetsHandler) Chunks(c *fiber.Ctx) error {
	if h.chunks == nil {
		return fiber.NewError(fiber.StatusConflict, "chunk view is not configured")
	}
	asset, err := h.repo.GetByID(reqCtx(c), c.Params("id"))
	if err != nil {
		return notFound(err, "asset not found")
	}
	offset := max(c.QueryInt("offset", 0), 0)
	limit := c.QueryInt("limit", defaultChunkPageSize)
	if limit <= 0 {
		limit = defaultChunkPageSize
	}
	limit = min(limit, maxChunkPageSize)

	chunks, total, err := h.chunks.ListPageByAssetID(reqCtx(c), asset.ID, offset, limit)
	if err != nil {
		return err
	}
	return c.JSON(assetChunksResponse{Chunks: chunks, Total: total, Offset: offset, Limit: limit})
}
