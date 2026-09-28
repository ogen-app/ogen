package handlers

import (
	"context"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// validateBrandRefs checks that a brand_voice_id / brand_audience_id belong to
// the caller's tenant. nil ids are allowed (clearing the ref). A
// nil repo skips validation (unwired in tests). The getters are tenant-scoped,
// so a foreign or unknown id resolves to nil → 422.
func validateBrandRefs(ctx context.Context, repo repository.BrandRepository, voiceID, audienceID *string) error {
	if repo == nil {
		return nil
	}
	if voiceID != nil {
		v, err := repo.GetVoice(ctx, *voiceID)
		if err != nil {
			return err
		}
		if v == nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity, "brand_voice_id not found")
		}
	}
	if audienceID != nil {
		a, err := repo.GetAudience(ctx, *audienceID)
		if err != nil {
			return err
		}
		if a == nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity, "brand_audience_id not found")
		}
	}
	return nil
}
