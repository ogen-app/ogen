package handlers

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

const (
	defaultChunkPageSize = 100
	maxChunkPageSize     = 500
)

// AssetChunkLister pages an asset's chunks without their embeddings.
// Implemented by repository.AssetChunksRepository.
type AssetChunkLister interface {
	ListPageByAssetID(ctx context.Context, assetID string, offset, limit int) ([]models.AssetChunk, int, error)
}

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

const (
	// maxSearchLimit is also the default: the Assets filter intersects the list
	// with every matching asset, so by default it gets them all.
	maxSearchLimit    = 1000
	minSearchQueryLen = 2
	maxSearchQueryLen = 200
)

// AssetChunkSearcher finds the assets whose chunks mention a text.
// Implemented by repository.AssetChunksRepository.
type AssetChunkSearcher interface {
	SearchText(ctx context.Context, query string, limit int) ([]repository.AssetTextMatch, int, error)
}

// assetSearchResponse lists the assets that mention a query, most matches
// first. Total counts every matching asset; it exceeds len(Results) when the
// limit cut the list.
type assetSearchResponse struct {
	Query   string                      `json:"query"`
	Total   int                         `json:"total"`
	Results []repository.AssetTextMatch `json:"results"`
}

// Search godoc
// @Summary      Search inside assets
// @Description  Returns the workspace's assets whose content mentions `q`, one row per asset: the assets with the most matching chunks first. Among the first 50 results, equal counts are ordered by the best chunk's `score`; elsewhere, and when `limit` cuts the list, by asset ID. Full-text, not semantic: words are matched with English stemming ("cycles" finds "cycle"), "quoted phrases" match as phrases, `-word` excludes and `or` alternates. Covers every asset kind that has chunks (PDF, documents and spreadsheets, URL, markdown, audio transcripts, image regions). `matches` is the number of matching chunks. The first 50 results also carry evidence from their best chunk: `score`, `snippet`, `chunk_index` and `source_label`; later results carry only `asset_id` and `matches`. `snippet` is HTML-escaped text whose only markup is `<mark>` around matched words, with "…" where it was cut. `source_label` is absent for chunks without a location (markdown, URL). `total` is the number of matching assets; it exceeds the number of `results` only when `limit` cut the list. A query of only stopwords or punctuation returns no results.
// @Tags         content-bank
// @Produce      json
// @Security     CookieAuth
// @Param        q      query     string  true   "Text to find (2–200 characters)"
// @Param        limit  query     int     false  "Max assets (default and max 1000)"
// @Success      200    {object}  assetSearchResponse
// @Failure      400    {object}  map[string]string  "code: query_too_short | query_too_long"
// @Failure      401    {object}  map[string]string
// @Router       /api/content-bank/assets/search [get]
func (h *AssetsHandler) Search(c *fiber.Ctx) error {
	if h.chunkSearch == nil {
		return fiber.NewError(fiber.StatusConflict, "asset search is not configured")
	}
	query := strings.TrimSpace(c.Query("q"))
	switch n := utf8.RuneCountInString(query); {
	case n < minSearchQueryLen:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"code":  models.AssetCodeQueryTooShort,
			"error": fmt.Sprintf("q must be at least %d characters", minSearchQueryLen),
		})
	case n > maxSearchQueryLen:
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"code":  models.AssetCodeQueryTooLong,
			"error": fmt.Sprintf("q must be at most %d characters", maxSearchQueryLen),
		})
	}
	limit := c.QueryInt("limit", maxSearchLimit)
	if limit <= 0 {
		limit = maxSearchLimit
	}
	limit = min(limit, maxSearchLimit)

	results, total, err := h.chunkSearch.SearchText(reqCtx(c), query, limit)
	if err != nil {
		return err
	}
	if results == nil {
		results = []repository.AssetTextMatch{}
	}
	return c.JSON(assetSearchResponse{Query: query, Total: total, Results: results})
}
