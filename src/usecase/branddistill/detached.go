package branddistill

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/runslot"
)

// finishRunTimeout bounds one detached run of the model stages, like the
// attempt timeout of a run inside a River worker.
const finishRunTimeout = 20 * time.Minute

// ErrRunGone means the run row no longer exists.
var ErrRunGone = errors.New("brand distillation run no longer exists")

// Finish runs a prepared run's model stages inline.
func (s *Service) Finish(ctx context.Context, tenantID, runID string) error {
	return s.finish(ctx, tenantID, runID)
}

// StartFinishDetached claims the run's slot and runs its model stages
// detached from ctx. It returns the run's token, or "" when another run
// holds the slot (read it with FinishSlot).
func (s *Service) StartFinishDetached(ctx context.Context, tenantID, runID string) (string, error) {
	if s.repos.Slots == nil {
		return "", errors.New("brand distillation run slots are not wired")
	}
	ctx = tenantctx.With(ctx, tenantID)
	if _, err := s.repos.Slots.Read(ctx, runID); errors.Is(err, sql.ErrNoRows) {
		return "", ErrRunGone
	} else if err != nil {
		return "", err
	}
	return runslot.Start(ctx, s.repos.Slots, runID, finishRunTimeout, nil, func(ctx context.Context) error {
		return s.finish(ctx, tenantID, runID)
	})
}

// FinishSlot reads the run's slot.
func (s *Service) FinishSlot(ctx context.Context, tenantID, runID string) (runslot.Slot, error) {
	ctx = tenantctx.With(ctx, tenantID)
	slot, err := runslot.Read(ctx, s.repos.Slots, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return runslot.Slot{}, ErrRunGone
	}
	return slot, err
}

// Map lists a site's pages with the service's own mapper, for a run whose
// job maps inside ogen.
func (s *Service) Map(ctx context.Context, in firecrawl.MapRequest) ([]firecrawl.Link, error) {
	if s.deps.Mapper == nil {
		return nil, &firecrawl.APIError{Message: "firecrawl: client disabled"}
	}
	return s.deps.Mapper.Map(ctx, in)
}

// CanMap reports whether the service has a mapper, so a job may defer the
// map instead.
func (s *Service) CanMap() bool { return s.deps.Mapper != nil }
