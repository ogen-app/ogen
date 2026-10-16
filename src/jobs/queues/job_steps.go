package queues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
)

// Step-run jobs. Jobs that need both ogen's data and an outside call (Zernio,
// ingestion, publishing) run in river-service, which makes every outside call
// with a credential leased from ogen. Each job is a short loop: river-service
// asks Step what to do next, does it, and passes the raw result to the next
// Step. The decisions stay here, next to the data: a Step reads and writes
// ogen's tables and plans each outside call with the ordinary client (for
// Zernio, zernio.Plan / zernio.Decode). The loop keeps no durable state, so a
// job cut short retries from the start; every flow converges on a repeat.

// Upstreams river-service holds a leased key for.
const (
	UpstreamZernio    = "zernio"
	UpstreamGemini    = "gemini"
	UpstreamFirecrawl = "firecrawl"
	// UpstreamLemonSqueezy is leased the key of the billing mode ogen runs in.
	UpstreamLemonSqueezy = "lemonsqueezy"
)

// StepInput is one Step request.
type StepInput struct {
	Kind string
	// Args is the job's arguments as JSON, with the same field names as the
	// job's task here.
	Args []byte
	// State is what the previous Step returned; empty on the first.
	State []byte
	// Result is the raw result of the action the previous Step asked for.
	Result *StepResult
	// Attempt is this run's attempt, from 1, of at most MaxAttempts.
	Attempt     int
	MaxAttempts int
}

// lastAttempt reports whether no retry follows this run, so a transient
// failure must be settled now. Unknown counts are never the last.
func (in StepInput) lastAttempt() bool {
	return in.MaxAttempts > 0 && in.Attempt >= in.MaxAttempts
}

// StepResult is the raw result of an action.
type StepResult struct {
	// HTTP status for a call or transfer; gRPC code for a sidecar call.
	Status int
	Body   []byte
	Err    string
	// The upstream's key is not set in ogen, so the call was not made.
	CredentialMissing bool
	// ContentType is the response's Content-Type, for an HTTP call.
	ContentType string
	// local is the typed outcome of an action run inside ogen through its
	// local closure; river-service's results never carry one.
	local *localOutcome
}

// localOutcome is what an action's local closure returned.
type localOutcome struct {
	value any
	err   error
}

// localAction makes an action in-process, with ogen's own clients, and
// returns its typed outcome. A step attaches one to an outside call whose
// client is ogen's (an embedder, a sidecar client), so a run inside ogen calls
// that client directly and river-service gets the raw request.
type localAction func(ctx context.Context) (any, error)

// localOr returns an action's typed outcome: the in-process one when the
// step ran inside ogen, otherwise decode's reading of river-service's result.
func localOr[T any](r *StepResult, decode func(*StepResult) (T, error)) (T, error) {
	if r != nil && r.local != nil {
		v, _ := r.local.value.(T)
		return v, r.local.err
	}
	if r == nil {
		var zero T
		return zero, errors.New("no result")
	}
	return decode(r)
}

// StepOutput is what river-service does next: exactly one of the action
// fields, Done or Retry is set.
type StepOutput struct {
	State    []byte
	HTTP     *HTTPCall
	Sidecar  *SidecarCall
	Transfer *MediaTransfer
	Done     *StepDone
	Retry    *StepRetry
	// Wait has river-service step again after this long with the same
	// state, for work ogen runs detached.
	Wait time.Duration
}

// HTTPCall is one request to an outside API. With an Upstream, Path is
// relative to that API's base URL and river-service adds its key; with none,
// URL is fetched as is.
type HTTPCall struct {
	Upstream string
	Method   string
	Path     string
	Query    url.Values
	Headers  map[string]string
	Body     []byte
	URL      string
	// local makes the call in-process instead.
	local localAction
}

// SidecarCall is one RPC to a sidecar: a unary Request, or for a
// client-streaming method a Stream of a stored file.
type SidecarCall struct {
	Service string
	Method  string
	Request []byte
	Stream  *StreamSource
	Timeout time.Duration
	// local makes the call in-process instead.
	local localAction
}

// StreamSource streams a stored file to a sidecar: Header first, then frames
// carrying the bytes in field ChunkField.
type StreamSource struct {
	SourceURL  string
	Header     []byte
	ChunkField int32
}

// MediaTransfer copies a stored file to a presigned upload URL.
type MediaTransfer struct {
	SourceURL   string
	UploadURL   string
	ContentType string
	Size        int64
	// storageKey names the stored file, so a run inside ogen reads it from
	// storage directly rather than through SourceURL.
	storageKey string
}

// StepDone ends the job, optionally with child jobs for river-service to
// enqueue and queued jobs for it to cancel. Follow-up jobs of kinds ogen
// enqueues anyway go through the Enqueuer instead; Enqueue is for the kinds
// that exist only between the two services.
type StepDone struct {
	Outcome string
	Enqueue []ChildJob
	Cancel  []JobMatch
}

// JobMatch selects the queued jobs of one kind whose args match every
// condition.
type JobMatch struct {
	Kind string
	Args []ArgMatch
}

// ArgMatch holds when the string arg Key is one of Values; an empty value
// also matches a job without the key.
type ArgMatch struct {
	Key    string
	Values []string
}

// ChildJob is a job river-service enqueues on ogen's behalf.
type ChildJob struct {
	Kind string
	Args []byte
}

// StepRetry stops the job for a later retry.
type StepRetry struct {
	Reason string
	After  time.Duration
}

// ErrUnknownStepKind means this ogen has no steps for the kind: a newer
// river-service is ahead of it.
var ErrUnknownStepKind = errors.New("job steps: unknown job kind")

type stepFunc func(ctx context.Context, in StepInput) (StepOutput, error)

// JobSteps answers Step for every kind river-service runs step by step.
type JobSteps struct {
	deps  Deps
	kinds map[string]stepFunc
}

// NewJobSteps builds every kind's steps from the job deps.
func NewJobSteps(d Deps) *JobSteps {
	s := &JobSteps{deps: d, kinds: map[string]stepFunc{}}
	s.registerZernioLifecycle()
	s.registerPublishing()
	s.registerIngestion()
	s.registerAnalysis()
	s.registerWeeklyReport()
	s.registerBilling()
	return s
}

// registerPublishing adds the publishing jobs. Each runs the same steps here
// when ogen still works the kind itself.
func (s *JobSteps) registerPublishing() {
	d := s.deps
	s.kinds[SubmitPostToZernioQueue] = (&SubmitPostProcessor{Deps: d.Zernio, Tenants: d.Tenants, Notifier: d.Notifier, Members: d.Users}).step
	s.kinds[PollZernioStatusQueue] = (&PollZernioStatusProcessor{Deps: d.Zernio, Notifier: d.Notifier, Members: d.Users}).step
	s.kinds[CancelZernioJobQueue] = (&CancelZernioJobProcessor{Deps: d.Zernio}).step
	s.kinds[WithdrawZernioPostQueue] = (&WithdrawZernioPostProcessor{Deps: d.Zernio, Notifier: d.Notifier, Members: d.Users}).step
	s.kinds[PostFirstCommentQueue] = (&PostFirstCommentProcessor{Deps: d.Zernio, Notifier: d.Notifier}).step

	orphans := &SweepZernioOrphansProcessor{Deps: d.Zernio, Config: d.OrphanSweep}
	s.kinds[SweepZernioOrphansQueue] = orphans.step
	s.kinds[OrphanSweepProfileKind] = orphans.profileStep
	analytics := newRefreshZernioAnalytics(d)
	s.kinds[RefreshZernioAnalyticsQueue] = analytics.step
	s.kinds[AnalyticsTenantKind] = analytics.tenantStep
	followers := newRefreshZernioFollowers(d)
	s.kinds[RefreshZernioFollowersQueue] = followers.step
	s.kinds[FollowersTenantKind] = followers.tenantStep
}

// registerIngestion adds the ingest jobs.
func (s *JobSteps) registerIngestion() {
	d := s.deps
	s.kinds[ProcessPDFQueue] = (&ProcessPDFProcessor{Deps: d.PDF}).step
	s.kinds[ProcessDocumentQueue] = (&ProcessDocumentProcessor{Deps: d.Document}).step
	s.kinds[ProcessURLQueue] = (&ProcessURLProcessor{Deps: d.URL}).step
	s.kinds[ProcessImageQueue] = (&ProcessImageProcessor{Deps: d.Image}).step
	s.kinds[ReembedImageQueue] = (&ReembedImageProcessor{Deps: d.Image}).step
	s.kinds[ProcessAudioKind] = (&ProcessAudioProcessor{Deps: d.Audio}).step
}

// registerAnalysis adds the asset analysis jobs. Their model calls run here,
// detached; river-service only orchestrates them.
func (s *JobSteps) registerAnalysis() {
	d := s.deps
	s.kinds[AnalyzeAssetKind] = (&AnalyzeAssetProcessor{Deps: d.Analysis}).step
	s.kinds[ReanalyzeCampaignKind] = (&ReanalyzeCampaignProcessor{Deps: d.Analysis}).step
	s.kinds[DistillAssetIdeasKind] = (&DistillAssetIdeasProcessor{Deps: d.Analysis}).step
	s.kinds[DistillBrandKind] = (&DistillBrandProcessor{Deps: d.BrandDistill}).step
}

// snooze stops the job and runs it again after wait, without using an
// attempt. A retry without a delay would use one, so the wait is at least a
// second.
func snooze(reason string, wait time.Duration) (StepOutput, error) {
	return StepOutput{Retry: &StepRetry{Reason: reason, After: max(wait, time.Second)}}, nil
}

// Step returns what river-service does next for one job. An error is
// transient (a database failure): river-service retries the job later.
func (s *JobSteps) Step(ctx context.Context, in StepInput) (StepOutput, error) {
	fn, ok := s.kinds[in.Kind]
	if !ok {
		return StepOutput{}, fmt.Errorf("%w: %q", ErrUnknownStepKind, in.Kind)
	}
	return fn(ctx, in)
}

func done(outcome string, children ...ChildJob) (StepOutput, error) {
	return StepOutput{Done: &StepDone{Outcome: outcome, Enqueue: children}}, nil
}

func retry(reason string, err error) (StepOutput, error) {
	if err != nil {
		reason += ": " + err.Error()
	}
	return StepOutput{Retry: &StepRetry{Reason: reason}}, nil
}

func encodeState(state any) ([]byte, error) {
	return json.Marshal(state)
}

func decodeState(raw []byte, into any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, into)
}

// nextZernio asks river-service to make a planned Zernio call.
func nextZernio(state any, call zernio.Call) (StepOutput, error) {
	raw, err := encodeState(state)
	if err != nil {
		return StepOutput{}, err
	}
	return StepOutput{State: raw, HTTP: &HTTPCall{
		Upstream: UpstreamZernio, Method: call.Method, Path: call.Path,
		Query: call.Query, Headers: call.Headers, Body: call.Body,
	}}, nil
}

// zernioResult is the Zernio client's view of a step result.
func zernioResult(r *StepResult) zernio.Result {
	if r == nil {
		return zernio.Result{Err: "no result"}
	}
	return zernio.Result{Status: r.Status, Body: r.Body, Err: r.Err}
}

// zernioDisabled reports a result for a call river-service couldn't make
// because no Zernio key is set in ogen: the integration is off.
func zernioDisabled(r *StepResult) bool {
	return r != nil && r.CredentialMissing
}
