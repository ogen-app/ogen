// Package branddistill reads a workspace's website, documents and answers
// and proposes the voices, audiences, guardrails and facts in them. A run
// collects its sources as assets, reads them for passages quoted word for
// word, composes a proposal, grounds every item against those passages and
// waits for the user. Nothing is brand material until the user commits it.
package branddistill

import (
	"context"
	"errors"
	"expvar"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/assetsource"
	"github.com/ogen-app/ogen/src/usecase/brandsource"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

// Entitlement features: the gate and the monthly allowance.
const (
	FeatureGate    = "brand_distill"
	FeatureMonthly = "brand_distills_per_month"
)

const (
	logComponent = "branddistill"
	eventType    = "brand.distill.updated"
	// collectTimeout is how long a run waits for its sources to be ingested
	// before reading what is ready.
	collectTimeout = 15 * time.Minute
	// maxReadTokens caps what one run reads across its sources.
	maxReadTokens = 400_000
	// minPageWords is the least text a website page needs to be read.
	minPageWords = 200
	// MaxRefines caps the answer rounds of one run chain.
	MaxRefines = 3
	// ExpireAfter is how long a proposal waits to be committed.
	ExpireAfter = 30 * 24 * time.Hour
	// ListLimit is how many runs the list returns.
	ListLimit = 10
)

var (
	runsTotal       = expvar.NewMap("ogen_brand_distill_runs_total")
	droppedTotal    = expvar.NewMap("ogen_brand_distill_dropped_total")
	committedTotal  = expvar.NewMap("ogen_brand_distill_committed_total")
	candidatesTotal = expvar.NewInt("ogen_brand_distill_candidates_total")
)

var (
	// ErrUnavailable means runs cannot run here: no job queue or no model.
	ErrUnavailable = errors.New("brand distillation is not available")
	// ErrInProgress means the workspace already has an open run; it is
	// returned with that run.
	ErrInProgress = errors.New("a brand distillation run is in progress")
	// ErrScrapingUnavailable means a website was given but pages cannot be
	// imported here.
	ErrScrapingUnavailable = errors.New("url scraping is not configured")
	// ErrNotReady means the run has no proposal waiting.
	ErrNotReady = errors.New("the run has no proposal waiting")
	// ErrCommitted means the run was committed already.
	ErrCommitted = errors.New("the run was committed already")
	// ErrRefineLimit means the run chain has had all its answer rounds.
	ErrRefineLimit = errors.New("no more answer rounds for this run")
	// ErrWaiting tells the job to come back: sources are still ingesting.
	ErrWaiting = errors.New("sources are still being ingested")
)

// InputError is a request that cannot be accepted as given. Code is
// machine-readable, Msg is safe to show, ProposalID names the item at fault.
type InputError struct {
	Code       string
	Msg        string
	ProposalID string
}

func (e *InputError) Error() string { return e.Msg }

// Input error codes.
const (
	CodeEmptyRequest   = "empty_request"
	CodeInvalidURL     = "invalid_url"
	CodeUnknownAsset   = "unknown_asset"
	CodeAssetFailed    = "asset_failed"
	CodeTooMany        = "too_many"
	CodeInvalidAnswer  = "invalid_answer"
	CodeUnknownItem    = "unknown_proposal_item"
	CodeInvalidItem    = "invalid_item"
	CodeNothingToSave  = "nothing_to_save"
	CodeUnknownQuestID = "unknown_question"
)

// Request limits.
const (
	MaxAssets        = 20
	MaxAnswers       = 12
	MaxAnswerBytes   = 2 << 10
	MaxQuestionBytes = 500
)

// Jobs queues a run's job. *queues.Enqueuer implements it.
type Jobs interface {
	EnqueueDistillBrand(ctx context.Context, tenantID, runID string) error
}

// Mapper lists a site's pages. *firecrawl.Client implements it.
type Mapper interface {
	Map(ctx context.Context, in firecrawl.MapRequest) ([]firecrawl.Link, error)
}

// Importer imports a page as a URL asset. *urlimport.Service implements it.
type Importer interface {
	Configured(ctx context.Context) bool
	Import(ctx context.Context, in urlimport.Input) (*urlimport.Result, error)
}

// Recorder meters vendor calls. *usage.Recorder implements it.
type Recorder interface {
	Record(ctx context.Context, vendor, feature string, ev vendors.MeterEvent)
}

// Notifier tells the run's author it finished. *notify.Service implements
// it.
type Notifier interface {
	Emit(ctx context.Context, userID string, spec notify.Spec) error
}

// Flows are the two stages of the brand_distill flow.
type Flows struct {
	Extract func(ctx context.Context, req brand_distill.ExtractRequest) (*brand_distill.ExtractResponse, error)
	Compose func(ctx context.Context, req brand_distill.ComposeRequest) (*brand_distill.ComposeResponse, error)
}

// Repos are the stores a run reads and writes.
type Repos struct {
	Runs   repository.BrandDistillRepository
	Assets repository.AssetRepository
	Chunks assetsource.Chunks
	Brands repository.BrandRepository
}

// Deps are the collaborators that may be absent: without Mapper or Importer
// a website cannot be read; without Recorder or Notifier nothing is metered
// or announced.
type Deps struct {
	Mapper   Mapper
	Importer Importer
	Marker   *brandsource.Marker
	Recorder Recorder
	Notifier Notifier
}

// Service runs brand distillations.
type Service struct {
	db      *bun.DB
	repos   Repos
	limiter *entitlements.Limiter
	hub     eventhub.Hub
	jobs    Jobs
	deps    Deps
	flows   atomic.Pointer[Flows]
	now     func() time.Time
}

// New builds the service. jobs, limiter and hub may be nil.
func New(db *bun.DB, repos Repos, limiter *entitlements.Limiter, hub eventhub.Hub, jobs Jobs, deps Deps) *Service {
	return &Service{db: db, repos: repos, limiter: limiter, hub: hub, jobs: jobs, deps: deps, now: time.Now}
}

// SetFlows binds the model stages; nil unbinds them (no Anthropic key).
func (s *Service) SetFlows(f *Flows) { s.flows.Store(f) }

// SetClock replaces the clock; tests only.
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Available reports whether a run could be started and finished here.
func (s *Service) Available() bool { return s.jobs != nil && s.flows.Load() != nil }

// EventPayload is the SSE body of a brand.distill.updated frame. The proposal
// is never in it; the client reads it once the run is ready.
type EventPayload struct {
	ID          string                    `json:"id"`
	Status      models.BrandDistillStatus `json:"status"`
	ParentRunID *string                   `json:"parentRunId"`
	ChildRunID  *string                   `json:"childRunId"`
	Sources     []EventSource             `json:"sources"`
	Counts      Counts                    `json:"counts"`
	Failure     *Failure                  `json:"failure,omitempty"`
}

// EventSource is one source's progress.
type EventSource struct {
	AssetID string `json:"assetId"`
	Status  string `json:"status"`
}

// Counts is how much a proposal holds.
type Counts struct {
	Voices     int `json:"voices"`
	Audiences  int `json:"audiences"`
	Guardrails int `json:"guardrails"`
	Facts      int `json:"facts"`
	Questions  int `json:"questions"`
}

// Failure is why a run failed.
type Failure struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// CountsOf counts a proposal's items.
func CountsOf(p *models.BrandProposal) Counts {
	if p == nil {
		return Counts{}
	}
	g := len(p.Guardrails.MayClaim) + len(p.Guardrails.NeverClaim) + len(p.Guardrails.BannedWords)
	if p.Guardrails.Disclaimer != nil {
		g++
	}
	return Counts{Voices: len(p.Voices), Audiences: len(p.Audiences), Guardrails: g, Facts: len(p.Facts), Questions: len(p.Questions)}
}

// FailureOf is the run's failure, nil unless it failed.
func FailureOf(run *models.BrandDistillRun) *Failure {
	if run.Status != models.BrandDistillStatusFailed {
		return nil
	}
	return &Failure{Code: run.FailureCode, Reason: run.FailureReason}
}

// Topic is the SSE topic of a run.
func Topic(runID string) string { return "entity:brand_distill:" + runID }

// publish sends the run's state to its topic. Best effort.
func (s *Service) publish(ctx context.Context, run *models.BrandDistillRun) {
	if s.hub == nil {
		return
	}
	tenantID, _ := tenantctx.From(ctx)
	payload := EventPayload{
		ID: run.ID, Status: run.Status, ParentRunID: run.ParentRunID, ChildRunID: run.ChildRunID,
		Sources: make([]EventSource, 0, len(run.Sources)), Counts: CountsOf(run.Proposal), Failure: FailureOf(run),
	}
	for _, src := range run.Sources {
		payload.Sources = append(payload.Sources, EventSource{AssetID: src.AssetID, Status: src.Status})
	}
	_ = s.hub.Publish(ctx, eventhub.Event{Topic: Topic(run.ID), TenantID: tenantID, Type: eventType, Payload: payload})
}

// save writes the named columns and publishes the run. A failed write is
// returned: the job retries it.
func (s *Service) save(ctx context.Context, run *models.BrandDistillRun, columns ...string) error {
	if err := s.repos.Runs.Save(ctx, s.db, run, columns...); err != nil {
		return err
	}
	s.publish(ctx, run)
	return nil
}

// fail settles a run as failed, releases its charge and tells its author.
func (s *Service) fail(ctx context.Context, run *models.BrandDistillRun, code, reason string) {
	run.Status, run.FailureCode, run.FailureReason = models.BrandDistillStatusFailed, code, reason
	if err := s.save(ctx, run, "status", "failure_code", "failure_reason", "sources"); err != nil {
		slog.WarnContext(ctx, "could not fail the brand distillation", logging.AttrComponent, logComponent,
			"run_id", run.ID, logging.AttrError, err)
	}
	if run.Charged {
		if err := s.repos.Runs.ReleaseCharge(ctx, run.ID); err != nil {
			slog.WarnContext(ctx, "could not release the charge", logging.AttrComponent, logComponent, "run_id", run.ID, logging.AttrError, err)
		}
	}
	runsTotal.Add("failed/"+code, 1)
	s.announce(ctx, run)
}

// announce tells the run's author it is ready or failed. Best effort.
func (s *Service) announce(ctx context.Context, run *models.BrandDistillRun) {
	if s.deps.Notifier == nil || run.CreatedBy == nil {
		return
	}
	spec := notify.Spec{
		EntityType: "brand_distill",
		EntityID:   run.ID,
		ActionURL:  "/foundation/read/" + run.ID,
		DedupeKey:  "brand_distill." + string(run.Status) + ":" + run.ID,
	}
	switch run.Status {
	case models.BrandDistillStatusReady:
		c := CountsOf(run.Proposal)
		spec.Level, spec.Type, spec.Title = models.NotificationLevelSuccess, "brand_distill.ready", "Your brand is ready to review"
		spec.Body = "Ogen read your material and proposes voices, audiences, guardrails and facts. Nothing is saved until you say so."
		spec.Data = map[string]any{"voices": c.Voices, "audiences": c.Audiences, "guardrails": c.Guardrails, "facts": c.Facts}
	case models.BrandDistillStatusFailed:
		spec.Level, spec.Type, spec.Title = models.NotificationLevelError, "brand_distill.failed", "Couldn't read your brand"
		spec.Body = run.FailureReason
		spec.Data = map[string]any{"code": run.FailureCode}
	default:
		return
	}
	_ = s.deps.Notifier.Emit(ctx, *run.CreatedBy, spec)
}
