package assetanalysis

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/runslot"
)

// distillRunTimeout bounds one detached distill, like the attempt timeout of a
// distill inside a River worker.
const distillRunTimeout = 10 * time.Minute

// ErrDistillGone means the distill run row no longer exists.
var ErrDistillGone = errors.New("distill run no longer exists")

// StartDistillDetached claims the distill run's slot and runs Distill
// detached from ctx. It returns the run's token, or "" when another run holds
// the slot (read it with DistillSlot).
func (s *Service) StartDistillDetached(ctx context.Context, tenantID, runID string) (string, error) {
	if s.repos.DistillSlots == nil {
		return "", errors.New("distill run slots are not wired")
	}
	ctx = tenantctx.With(ctx, tenantID)
	if _, err := s.repos.DistillSlots.Read(ctx, runID); errors.Is(err, sql.ErrNoRows) {
		return "", ErrDistillGone
	} else if err != nil {
		return "", err
	}
	return runslot.Start(ctx, s.repos.DistillSlots, runID, distillRunTimeout, distillErrCode, func(ctx context.Context) error {
		return s.Distill(ctx, tenantID, runID)
	})
}

// DistillSlot reads the distill run's slot. A source that could not be read
// comes back wrapping errSourceUnreadable, so FailDistill reports it as such.
func (s *Service) DistillSlot(ctx context.Context, tenantID, runID string) (runslot.Slot, error) {
	ctx = tenantctx.With(ctx, tenantID)
	slot, err := runslot.Read(ctx, s.repos.DistillSlots, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return runslot.Slot{}, ErrDistillGone
	}
	if err == nil && slot.Code == runErrSourceUnreadable {
		slot.Err = fmt.Errorf("%w: %s", errSourceUnreadable, slot.Err)
	}
	return slot, err
}

func distillErrCode(err error) string {
	if errors.Is(err, errSourceUnreadable) {
		return runErrSourceUnreadable
	}
	return ""
}
