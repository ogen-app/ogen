package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

func TestDrainMiddleware(t *testing.T) {
	errWork := errors.New("upstream call failed")
	stopped := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	remotelyCancelled := func() context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(river.ErrJobCancelledRemotely)
		return ctx
	}

	tests := []struct {
		name       string
		ctx        context.Context
		err        error
		wantSnooze bool
		wantErr    error
	}{
		{name: "success while running", ctx: context.Background(), err: nil},
		{name: "failure while running keeps the error", ctx: context.Background(), err: errWork, wantErr: errWork},
		{name: "success after stop", ctx: stopped(), err: nil},
		{name: "failure after stop is rescheduled", ctx: stopped(), err: errWork, wantSnooze: true},
		{name: "remote cancel keeps the error", ctx: remotelyCancelled(), err: errWork, wantErr: errWork},
		{name: "job cancel after stop keeps the cancel", ctx: stopped(), err: river.JobCancel(errWork)},
	}

	mw := &drainMiddleware{}
	job := &rivertype.JobRow{ID: 7, Kind: "process_audio", Attempt: 5, MaxAttempts: 5}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mw.Work(tt.ctx, job, func(context.Context) error { return tt.err })

			snooze, isSnooze := errors.AsType[*rivertype.JobSnoozeError](got)
			if isSnooze != tt.wantSnooze {
				t.Fatalf("Work() = %v, want snooze %v", got, tt.wantSnooze)
			}
			if isSnooze {
				if snooze.Duration != drainSnooze {
					t.Errorf("snooze duration = %v, want %v", snooze.Duration, drainSnooze)
				}
				return
			}
			if tt.wantErr != nil && !errors.Is(got, tt.wantErr) {
				t.Errorf("Work() = %v, want %v", got, tt.wantErr)
			}
			if tt.err == nil && got != nil {
				t.Errorf("Work() = %v, want nil", got)
			}
			if _, isCancel := errors.AsType[*rivertype.JobCancelError](tt.err); isCancel {
				if _, ok := errors.AsType[*rivertype.JobCancelError](got); !ok {
					t.Errorf("Work() = %v, want the JobCancel kept", got)
				}
			}
		})
	}
}
