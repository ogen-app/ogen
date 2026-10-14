package queues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Step-run jobs ogen still works itself. Until a kind is listed in
// OUTBOX_RELAY_KINDS, its River worker here runs the same steps river-service
// would, carrying out each action in-process with ogen's own clients. So each
// such job has one implementation, its steps, whichever service runs it.

// maxLocalSteps bounds one local run, as river-service bounds its own.
const maxLocalSteps = 512

// localActions carries out step actions in-process.
type localActions struct {
	zernio *zernio.Client
	// storage serves the stored files a transfer names by key.
	storage storage.Storage
	// transfer streams media between stored files and upload URLs. Nil uses
	// a client bounded only by the job's context.
	transfer *http.Client
	// children are the steps of the child kinds a job fans out into; a
	// local run works each child inline, as its own job.
	children map[string]stepFunc
}

// runLocal runs a step-run job to completion in-process, from its first
// step. args is the job's args value, encoded as river-service would store
// it. The returned error is what the River worker returns: nil when done, a
// snooze or an error to retry.
func runLocal(ctx context.Context, step stepFunc, kind string, args any, attempt, maxAttempts int, acts localActions) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("%s: encode args: %w", kind, err)
	}
	in := StepInput{Kind: kind, Args: raw, Attempt: attempt, MaxAttempts: maxAttempts}
	for range maxLocalSteps {
		out, err := step(ctx, in)
		if err != nil {
			return err
		}
		in.State, in.Result = out.State, nil
		switch {
		case out.HTTP != nil && out.HTTP.local != nil:
			in.Result = runLocalAction(ctx, out.HTTP.local)
		case out.HTTP != nil:
			in.Result = acts.http(ctx, out.HTTP)
		case out.Transfer != nil:
			in.Result = acts.transferMedia(ctx, out.Transfer)
		case out.Sidecar != nil && out.Sidecar.local != nil:
			in.Result = runLocalAction(ctx, out.Sidecar.local)
		case out.Sidecar != nil:
			in.Result = &StepResult{Status: int(codesUnimplemented), Err: "sidecar call without a local client"}
		case out.Retry != nil:
			if out.Retry.After > 0 {
				return river.JobSnooze(out.Retry.After)
			}
			return fmt.Errorf("%s: %s", kind, out.Retry.Reason)
		default:
			return acts.finish(ctx, kind, out.Done)
		}
	}
	return fmt.Errorf("%s: no result after %d steps", kind, maxLocalSteps)
}

// runLocalAction makes an action through its local closure.
func runLocalAction(ctx context.Context, act localAction) *StepResult {
	v, err := act(ctx)
	return &StepResult{local: &localOutcome{value: v, err: err}}
}

// codesUnimplemented is gRPC's UNIMPLEMENTED, the code river-service returns
// for a sidecar it has no address for.
const codesUnimplemented = 12

func (a localActions) http(ctx context.Context, c *HTTPCall) *StepResult {
	if c.Upstream != UpstreamZernio {
		return &StepResult{Err: fmt.Sprintf("upstream %q is not run inside ogen", c.Upstream)}
	}
	res, missing := a.zernio.Send(ctx, zernio.Call{Method: c.Method, Path: c.Path, Query: c.Query, Headers: c.Headers, Body: c.Body})
	if missing {
		return &StepResult{CredentialMissing: true, Err: "zernio_api_key is not set"}
	}
	return &StepResult{Status: res.Status, Body: res.Body, Err: res.Err}
}

// transferMedia streams a stored file from its download link to an upload
// URL, as river-service does.
func (a localActions) transferMedia(ctx context.Context, t *MediaTransfer) *StepResult {
	client := a.transfer
	if client == nil {
		client = &http.Client{}
	}
	src, err := a.openSource(ctx, client, t)
	if err != nil {
		return &StepResult{Err: err.Error()}
	}
	defer func() { _ = src.Close() }()
	put, err := http.NewRequestWithContext(ctx, http.MethodPut, t.UploadURL, src)
	if err != nil {
		return &StepResult{Err: err.Error()}
	}
	if t.ContentType != "" {
		put.Header.Set("Content-Type", t.ContentType)
	}
	if t.Size > 0 {
		put.ContentLength = t.Size
	}
	resp, err := client.Do(put)
	if err != nil {
		return &StepResult{Err: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return &StepResult{Status: resp.StatusCode, Body: b}
}

// openSource opens the file a transfer copies: from storage when the step
// named its key, otherwise through its download link.
func (a localActions) openSource(ctx context.Context, client *http.Client, t *MediaTransfer) (io.ReadCloser, error) {
	if t.storageKey != "" && a.storage != nil {
		rc, err := a.storage.Download(ctx, t.storageKey)
		if err != nil {
			return nil, fmt.Errorf("read source: %w", err)
		}
		return rc, nil
	}
	get, err := http.NewRequestWithContext(ctx, http.MethodGet, t.SourceURL, nil)
	if err != nil {
		return nil, err
	}
	src, err := client.Do(get)
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	if src.StatusCode >= 300 {
		_ = src.Body.Close()
		return nil, fmt.Errorf("read source: status %d", src.StatusCode)
	}
	return src.Body, nil
}

// finish carries out a done step's cancels on the worker's own River client,
// then works its child jobs inline, one after another. Cancels are best
// effort, as they are in river-service: a job left behind exits on its own
// when it runs. A failing child is logged and skipped, so one can't starve
// the rest; the next sweep tries it again.
func (a localActions) finish(ctx context.Context, kind string, d *StepDone) error {
	if d == nil {
		return nil
	}
	for _, m := range d.Cancel {
		if _, err := cancelMatchingLocal(ctx, m); err != nil {
			slog.WarnContext(ctx, "could not cancel queued jobs", logging.AttrComponent, "jobs.steps",
				"job_kind", kind, "cancel_kind", m.Kind, logging.AttrError, err)
		}
	}
	for _, c := range d.Enqueue {
		step, ok := a.children[c.Kind]
		if !ok {
			return fmt.Errorf("%s: no local steps for child job kind %q", kind, c.Kind)
		}
		if err := runLocal(ctx, step, c.Kind, json.RawMessage(c.Args), 1, 1, a); err != nil {
			slog.WarnContext(ctx, "child job failed", logging.AttrComponent, "jobs.steps",
				"job_kind", kind, "child_kind", c.Kind, logging.AttrError, err)
		}
	}
	return nil
}

// cancelLimit bounds the jobs one match cancels.
const cancelLimit = 100

// liveJobStates are the states a job can still be cancelled in.
var liveJobStates = []rivertype.JobState{
	rivertype.JobStateAvailable,
	rivertype.JobStatePending,
	rivertype.JobStateRetryable,
	rivertype.JobStateRunning,
	rivertype.JobStateScheduled,
}

// cancelMatchingLocal cancels the queued jobs m selects on the worker's River
// client and returns how many River cancelled outright; a running one
// finishes its attempt.
func cancelMatchingLocal(ctx context.Context, m JobMatch) (int, error) {
	client, err := river.ClientFromContextSafely[*sql.Tx](ctx)
	if err != nil {
		return 0, err
	}
	params := river.NewJobListParams().Kinds(m.Kind).States(liveJobStates...).First(cancelLimit)
	if len(m.Args) > 0 {
		where, named := matchSQL(m.Args)
		params = params.Where(where, named)
	}
	res, err := client.JobList(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("list %s jobs: %w", m.Kind, err)
	}
	cancelled := 0
	for _, job := range res.Jobs {
		row, err := client.JobCancel(ctx, job.ID)
		if errors.Is(err, rivertype.ErrNotFound) {
			continue
		}
		if err != nil {
			return cancelled, fmt.Errorf("cancel %s job %d: %w", m.Kind, job.ID, err)
		}
		if row.State == rivertype.JobStateCancelled {
			cancelled++
		}
	}
	return cancelled, nil
}

// matchSQL turns arg conditions into a JobList filter. Keys and values are
// bound, never spliced in.
func matchSQL(args []ArgMatch) (string, river.NamedArgs) {
	conds := make([]string, 0, len(args))
	named := river.NamedArgs{}
	for i, a := range args {
		k, v := fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i)
		conds = append(conds, fmt.Sprintf("COALESCE(args->>@%s, '') = ANY(@%s)", k, v))
		named[k], named[v] = a.Key, a.Values
	}
	return strings.Join(conds, " AND "), named
}
