package assetanalysis

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// Detached runs. When river-service orchestrates an analysis, the model call
// runs here, detached from the job step that started it: the step claims the
// lens's run slot, the run heartbeats while it works and records its outcome
// on the lens row, and later steps poll the slot. A river-service deploy
// therefore never cuts a call (the restarted job finds the slot and waits on
// it), and an ogen deploy that cuts one shows as a stale heartbeat, which the
// job retries.

const (
	// runSlotStale is how long a run may go without a heartbeat before its
	// slot counts as lost.
	runSlotStale = time.Minute
	// runHeartbeatEvery is how often a detached run refreshes its slot.
	runHeartbeatEvery = 15 * time.Second
	// detachedRunTimeout bounds one detached run, like the attempt timeout of
	// an analysis run inside a River worker.
	detachedRunTimeout = 15 * time.Minute
)

// runErrSourceUnreadable is the slot's error code for a source that could not
// be read, so Fail can tell it from a model outage.
const runErrSourceUnreadable = "source_unreadable"

// ErrLensGone means the lens no longer applies: its asset or campaign is gone,
// or the asset can't be analysed.
var ErrLensGone = errors.New("analysis lens no longer applies")

// RunSlot is a lens's detached run as its job sees it.
type RunSlot struct {
	Token string
	// Done means the run finished, with Err nil on success.
	Done bool
	Err  error
	// Running means the run is in flight and heartbeating.
	Running bool
}

// StartDetached claims the lens's run slot and starts the analysis detached
// from ctx. It returns the run's token, or "" when another run holds the
// slot (read it with Slot).
func (s *Service) StartDetached(ctx context.Context, l Lens, reason string) (string, error) {
	ctx = tenantctx.With(ctx, l.TenantID)
	t, err := s.load(ctx, l)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", ErrLensGone
	}
	token, err := models.NewID()
	if err != nil {
		return "", err
	}
	claimed, err := s.repos.Analyses.ClaimRunSlot(ctx, t.row.ID, token, s.now().Add(-runSlotStale))
	if err != nil {
		return "", fmt.Errorf("claim run slot: %w", err)
	}
	if !claimed {
		return "", nil
	}
	go s.runDetached(context.WithoutCancel(ctx), l, reason, t.row.ID, token)
	return token, nil
}

func (s *Service) runDetached(ctx context.Context, l Lens, reason, rowID, token string) {
	ctx, cancel := context.WithTimeout(ctx, detachedRunTimeout)
	defer cancel()
	beat := time.NewTicker(runHeartbeatEvery)
	defer beat.Stop()
	// A run that lost its slot is cancelled, so it stops spending on the
	// model and never records a result over the run that took the slot.
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("analysis run panicked: %v", r)
			}
		}()
		done <- s.Run(runCtx, l, reason)
	}()
	var runErr error
wait:
	for {
		select {
		case runErr = <-done:
			break wait
		case <-beat.C:
			held, err := s.repos.Analyses.HeartbeatRunSlot(ctx, rowID, token)
			switch {
			case err != nil:
				slog.WarnContext(ctx, "could not refresh the analysis run slot", logging.AttrComponent, logComponent, logging.AttrError, err)
			case !held:
				slog.WarnContext(ctx, "analysis run lost its slot to another run; stopping it", logging.AttrComponent, logComponent, "lens_id", rowID)
				stopRun()
			}
		}
	}
	errText, code := "", ""
	if runErr != nil {
		errText = runErr.Error()
		if errors.Is(runErr, errSourceUnreadable) {
			code = runErrSourceUnreadable
		}
	}
	// The outcome is recorded even when the run's own deadline passed.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer fcancel()
	if err := s.repos.Analyses.FinishRunSlot(fctx, rowID, token, errText, code); err != nil {
		slog.ErrorContext(fctx, "could not record the analysis run outcome", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
}

// Slot reads the lens's run slot. A lens that no longer exists reads as
// ErrLensGone.
func (s *Service) Slot(ctx context.Context, l Lens) (RunSlot, error) {
	ctx = tenantctx.With(ctx, l.TenantID)
	row, err := s.repos.Analyses.GetLens(ctx, l.AssetID, l.CampaignID)
	if errors.Is(err, sql.ErrNoRows) {
		return RunSlot{}, ErrLensGone
	}
	if err != nil {
		return RunSlot{}, err
	}
	slot := RunSlot{}
	if row.RunToken != nil {
		slot.Token = *row.RunToken
	}
	switch {
	case row.RunDoneAt != nil:
		slot.Done = true
		if row.RunError != "" {
			slot.Err = errors.New(row.RunError)
			if row.RunErrorCode == runErrSourceUnreadable {
				slot.Err = fmt.Errorf("%w: %s", errSourceUnreadable, row.RunError)
			}
		}
	case row.RunHeartbeatAt != nil && row.RunHeartbeatAt.After(s.now().Add(-runSlotStale)):
		slot.Running = true
	}
	return slot, nil
}

// Touch records that the lens's job is stepping on it, so the backfill
// doesn't take the lens for stranded. Best effort.
func (s *Service) Touch(ctx context.Context, l Lens) {
	ctx = tenantctx.With(ctx, l.TenantID)
	row, err := s.repos.Analyses.GetLens(ctx, l.AssetID, l.CampaignID)
	if err != nil {
		return
	}
	if err := s.repos.Analyses.TouchLens(ctx, row.ID); err != nil {
		slog.WarnContext(ctx, "could not record analysis job activity", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
}
