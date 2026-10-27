// Package runslot runs a model call detached from the job step that starts
// it. The step claims the run row's slot with a token, the call heartbeats
// while it works and records its outcome on the row, and later steps poll the
// slot. A river-service deploy therefore never cuts a call (the restarted job
// finds the slot and waits on it), and an ogen deploy that cuts one shows as
// a stale heartbeat, which the job retries.
package runslot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const (
	// Stale is how long a run may go without a heartbeat before its slot
	// counts as lost.
	Stale = time.Minute
	// heartbeatEvery is how often a detached run refreshes its slot.
	heartbeatEvery = 15 * time.Second
	// selfFence is how long a run goes on without a confirmed heartbeat
	// before it stops itself: inside Stale, so it has stopped before another
	// run could take its slot, and the two never both write.
	selfFence = Stale - heartbeatEvery
	// finishTries bounds the attempts to record a run's outcome; a lost
	// outcome would make the job run the model again.
	finishTries = 3
)

// Row is a run row's slot columns.
type Row struct {
	Token       *string    `bun:"run_token"`
	HeartbeatAt *time.Time `bun:"run_heartbeat_at"`
	DoneAt      *time.Time `bun:"run_done_at"`
	Err         string     `bun:"run_error"`
	Code        string     `bun:"run_error_code"`
}

// Store keeps the slot on one table's rows, in the tenant of the context.
type Store interface {
	// Claim takes the slot for token unless a live run holds it.
	Claim(ctx context.Context, id, token string, staleBefore time.Time) (bool, error)
	// Heartbeat refreshes token's run; false means token no longer holds
	// an unfinished slot (another run took it).
	Heartbeat(ctx context.Context, id, token string) (bool, error)
	Finish(ctx context.Context, id, token, errText, code string) error
	// Read returns the row's slot, sql.ErrNoRows when the row is gone.
	Read(ctx context.Context, id string) (Row, error)
}

// Slot is a run's detached call as its job sees it.
type Slot struct {
	Token string
	// Done means the call finished; Err and Code describe a failure.
	Done bool
	Err  error
	Code string
	// Running means the call is in flight and heartbeating.
	Running bool
}

// Start claims row id's slot and runs fn detached from ctx, bounded by
// timeout. It returns the run's token, or "" when another run holds the slot
// (read it with Read). code turns fn's error into the code stored beside it.
func Start(ctx context.Context, store Store, id string, timeout time.Duration, code func(error) string, fn func(context.Context) error) (string, error) {
	token, err := models.NewID()
	if err != nil {
		return "", err
	}
	claimed, err := store.Claim(ctx, id, token, time.Now().Add(-Stale))
	if err != nil {
		return "", fmt.Errorf("claim run slot: %w", err)
	}
	if !claimed {
		return "", nil
	}
	go run(context.WithoutCancel(ctx), store, id, token, timeout, code, fn)
	return token, nil
}

func run(ctx context.Context, store Store, id, token string, timeout time.Duration, code func(error) string, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	// A run that lost its slot is cancelled, so it stops spending on the
	// model and never records a result over the run that took the slot.
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("detached run panicked: %v", r)
			}
		}()
		done <- fn(runCtx)
	}()
	var runErr error
	lastBeat := time.Now()
wait:
	for {
		select {
		case runErr = <-done:
			break wait
		case <-beat.C:
			held, err := store.Heartbeat(ctx, id, token)
			switch {
			case err != nil:
				slog.WarnContext(ctx, "could not refresh a run slot", logging.AttrComponent, "runslot", "run_id", id, logging.AttrError, err)
				if time.Since(lastBeat) >= selfFence {
					slog.WarnContext(ctx, "detached run could not confirm its slot; stopping it", logging.AttrComponent, "runslot", "run_id", id)
					stopRun()
				}
			case held:
				lastBeat = time.Now()
			default:
				slog.WarnContext(ctx, "detached run lost its slot to another run; stopping it", logging.AttrComponent, "runslot", "run_id", id)
				stopRun()
			}
		}
	}
	errText, errCode := "", ""
	if runErr != nil {
		errText = runErr.Error()
		if code != nil {
			errCode = code(runErr)
		}
	}
	// The outcome is recorded even when the run's own deadline passed.
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer fcancel()
	var err error
	for try := range finishTries {
		if err = store.Finish(fctx, id, token, errText, errCode); err == nil {
			return
		}
		if try < finishTries-1 && sleep(fctx, time.Second<<try) != nil {
			break
		}
	}
	slog.ErrorContext(fctx, "could not record a detached run's outcome", logging.AttrComponent, "runslot", "run_id", id, logging.AttrError, err)
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Read returns row id's slot.
func Read(ctx context.Context, store Store, id string) (Slot, error) {
	row, err := store.Read(ctx, id)
	if err != nil {
		return Slot{}, err
	}
	slot := Slot{}
	if row.Token != nil {
		slot.Token = *row.Token
	}
	switch {
	case row.DoneAt != nil:
		slot.Done = true
		if row.Err != "" {
			slot.Err, slot.Code = errors.New(row.Err), row.Code
		}
	case row.HeartbeatAt != nil && row.HeartbeatAt.After(time.Now().Add(-Stale)):
		slot.Running = true
	}
	return slot, nil
}
