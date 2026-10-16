package queues

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// Inline runs. runLocal drives a job's steps to the end in-process, carrying
// out each action with ogen's own clients the way river-service would. It
// backs the processors' Process methods, which the tests run against fakes,
// so the steps river-service drives are exercised end to end here too.

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
// it. It returns nil when done, a *SnoozeError when the job asked to run
// again later, or the error a retry would carry.
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
		case out.Wait > 0:
			t := time.NewTimer(out.Wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		case out.Retry != nil:
			if out.Retry.After > 0 {
				return &SnoozeError{After: out.Retry.After}
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

// finish works a done step's child jobs inline, one after another. A failing
// child is logged and skipped, so one can't starve the rest; the next sweep
// tries it again. Cancels need a queue, which only river-service has.
func (a localActions) finish(ctx context.Context, kind string, d *StepDone) error {
	if d == nil {
		return nil
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

// SnoozeError is an inline run that asked to run again after After, without
// using an attempt.
type SnoozeError struct {
	After time.Duration
}

func (e *SnoozeError) Error() string { return fmt.Sprintf("snoozed for %s", e.After) }
