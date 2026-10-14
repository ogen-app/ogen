package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// drainSnooze is how long a job interrupted by a client stop waits before it
// is worked again. During a deploy the replacement replica is already up, so
// it picks the job up within seconds.
const drainSnooze = 5 * time.Second

// drainMiddleware reschedules a job whose run was cut short by the client
// stopping. A stop that outlasts the soft-stop timeout cancels every running
// job's context; without this, the cancelled run would count as a failed
// attempt, and a job on its last attempt would give up on work that was only
// interrupted. A snooze consumes no attempt, and the job resumes from its last
// checkpoint.
//
// The middleware sits outside River's per-job timeout, so its context ends
// only when the client stops or the job is cancelled remotely. A remote
// JobCancel keeps River's own handling.
type drainMiddleware struct {
	river.MiddlewareDefaults
}

func (*drainMiddleware) Work(ctx context.Context, job *rivertype.JobRow, doInner func(context.Context) error) error {
	err := doInner(ctx)
	if err == nil || ctx.Err() == nil || errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely) {
		return err
	}
	if _, ok := errors.AsType[*rivertype.JobSnoozeError](err); ok {
		return err
	}
	if _, ok := errors.AsType[*rivertype.JobCancelError](err); ok {
		return err
	}
	slog.InfoContext(ctx, "job interrupted by shutdown; rescheduled",
		logging.AttrComponent, "jobs",
		"job_kind", job.Kind, "job_id", job.ID, "job_attempt", job.Attempt,
		logging.AttrError, err)
	return river.JobSnooze(drainSnooze)
}
