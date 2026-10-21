package assetanalysis

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// FeatureMonthlyDistills caps distill runs per month. The asset_analysis gate
// covers them too.
const FeatureMonthlyDistills = "idea_distills_per_month"

const (
	distillEventType   = "asset.ideas.distill.updated"
	lensGoneReason     = "The asset left the campaign before its ideas were saved"
	distillLimitReason = "This month's idea runs are used up"
	// maxPriorIdeas caps the existing ideas a distill prompt lists; every
	// title in scope is still checked for repeats.
	maxPriorIdeas = 100
	// DistillListLimit is how many runs a lens's run list returns.
	DistillListLimit = 20
)

var (
	distillRunsTotal    = expvar.NewMap("ogen_idea_distill_runs_total")
	distillIdeasTotal   = expvar.NewInt("ogen_idea_distill_ideas_total")
	distillDroppedTotal = expvar.NewInt("ogen_idea_distill_duplicates_dropped_total")
	distillFocusTotal   = expvar.NewMap("ogen_idea_distill_focus_total")
)

// ErrAnalysisNotReady means the lens has no finished analysis to distill
// from.
var ErrAnalysisNotReady = errors.New("analysis is not ready")

// ErrDistillInProgress means the lens already has a distill run waiting or
// running; RequestDistill returns that run with it.
var ErrDistillInProgress = errors.New("a distill run of this lens is in progress")

// DistillInputError is a distill request that cannot be accepted as given.
type DistillInputError struct{ Msg string }

func (e *DistillInputError) Error() string { return e.Msg }

// Distiller runs the distill flow.
type Distiller func(ctx context.Context, req asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error)

// DistillInput is a request for more ideas from one lens. Count 0 takes the
// default.
type DistillInput struct {
	TenantID   string
	AssetID    string
	CampaignID *string
	Focus      string
	Count      int
	UserID     *string
}

// RequestDistill queues a distill run of a ready lens and returns it pending.
// sql.ErrNoRows means the campaign is gone or does not hold the asset. A
// gated tier gets the limiter's FeatureNotAvailableError or
// QuotaExceededError; a lens with an active run gets that run with
// ErrDistillInProgress.
func (s *Service) RequestDistill(ctx context.Context, in DistillInput) (*models.IdeaDistillRun, error) {
	ctx = tenantctx.With(ctx, in.TenantID)
	focus, count, err := distillParams(in.Focus, in.Count)
	if err != nil {
		return nil, err
	}
	lens, err := s.readyLens(ctx, in.AssetID, in.CampaignID)
	if err != nil {
		return nil, err
	}
	if s.jobs == nil || s.distiller.Load() == nil {
		return nil, ErrUnavailable
	}
	if err := s.limiter.RequireGate(ctx, in.TenantID, FeatureAnalysis); err != nil {
		return nil, err
	}
	if _, err := s.limiter.Require(ctx, in.TenantID, FeatureMonthlyDistills); err != nil {
		return nil, err
	}
	run := &models.IdeaDistillRun{
		AnalysisID: lens.ID, AssetID: in.AssetID, CampaignID: in.CampaignID,
		Focus: focus, Count: count, CreatedBy: in.UserID,
	}
	if err := s.repos.Distills.Create(ctx, run); err != nil {
		if errors.Is(err, repository.ErrDistillActive) {
			if active, aerr := s.repos.Distills.Active(ctx, lens.ID); aerr == nil {
				return active, ErrDistillInProgress
			}
			return nil, ErrDistillInProgress
		}
		return nil, err
	}
	if err := s.jobs.EnqueueDistillIdeas(ctx, in.TenantID, run.ID); err != nil {
		s.setDistillStatus(ctx, run, models.IdeaDistillStatusFailed, models.AnalysisCodeModelUnavailable, unavailableReason)
		return nil, fmt.Errorf("queue the distill run: %w", err)
	}
	distillFocusTotal.Add(focusKind(focus), 1)
	s.publishDistill(ctx, run)
	return run, nil
}

func focusKind(focus string) string {
	if focus == "" {
		return "without"
	}
	return "with"
}

// distillParams trims and checks the focus and count.
func distillParams(focus string, count int) (string, int, error) {
	focus = strings.TrimSpace(focus)
	if utf8.RuneCountInString(focus) > models.IdeaDistillMaxFocus {
		return "", 0, &DistillInputError{Msg: fmt.Sprintf("focus must be at most %d characters", models.IdeaDistillMaxFocus)}
	}
	count = cmp.Or(count, models.IdeaDistillDefaultCount)
	if count < models.IdeaDistillMinCount || count > models.IdeaDistillMaxCount {
		return "", 0, &DistillInputError{Msg: fmt.Sprintf("count must be between %d and %d", models.IdeaDistillMinCount, models.IdeaDistillMaxCount)}
	}
	return focus, count, nil
}

// readyLens returns the asset's lens when its analysis is ready. A campaign
// lens needs a live campaign that holds the asset (else sql.ErrNoRows).
func (s *Service) readyLens(ctx context.Context, assetID string, campaignID *string) (*models.AssetAnalysis, error) {
	if campaignID != nil {
		campaign, err := s.repos.Campaigns.GetByID(ctx, *campaignID)
		if err != nil {
			return nil, err
		}
		if !hasAsset(campaign, assetID) {
			return nil, sql.ErrNoRows
		}
	}
	lens, err := s.repos.Analyses.GetLens(ctx, assetID, campaignID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAnalysisNotReady
	}
	if err != nil {
		return nil, err
	}
	if lens.Status != models.AssetAnalysisStatusReady {
		return nil, ErrAnalysisNotReady
	}
	return lens, nil
}

// ListDistills returns a lens's latest distill runs, newest first; none when
// the lens does not exist yet.
func (s *Service) ListDistills(ctx context.Context, tenantID, assetID string, campaignID *string) ([]models.IdeaDistillRun, error) {
	ctx = tenantctx.With(ctx, tenantID)
	lens, err := s.repos.Analyses.GetLens(ctx, assetID, campaignID)
	if errors.Is(err, sql.ErrNoRows) {
		return []models.IdeaDistillRun{}, nil
	}
	if err != nil {
		return nil, err
	}
	return s.repos.Distills.ListByAnalysis(ctx, lens.ID, DistillListLimit)
}

// distillTarget is everything a distill run needs, loaded and checked.
type distillTarget struct {
	run      *models.IdeaDistillRun
	lens     *models.AssetAnalysis
	asset    *models.Asset
	campaign *models.Campaign
}

// Distill runs one queued distill run. It returns nil once the run is
// settled (saved, gated or failed for good) and an error when it should be
// retried.
func (s *Service) Distill(ctx context.Context, tenantID, runID string) error {
	ctx = tenantctx.With(ctx, tenantID)
	t, err := s.loadDistill(ctx, runID)
	if err != nil || t == nil {
		return err
	}
	dec, ok := s.admitDistill(ctx, t)
	if !ok {
		return nil
	}
	distill := s.distiller.Load()
	if distill == nil {
		return ErrUnavailable
	}
	src, err := s.buildSource(ctx, t.asset)
	if err != nil {
		return fmt.Errorf("%w: %w", errSourceUnreadable, err)
	}
	if !hasText(src) {
		s.setDistillStatus(ctx, t.run, models.IdeaDistillStatusFailed, models.AnalysisCodeEmptySource, emptySourceReason)
		return nil
	}
	req, err := s.distillRequest(ctx, tenantID, t, src)
	if err != nil {
		return err
	}
	chargeID := s.recordCharge(ctx, t.run.ID)
	s.setDistillStatus(ctx, t.run, models.IdeaDistillStatusRunning, "", "")
	resp, err := (*distill)(ctx, req)
	if err != nil {
		_, rejected := errors.AsType[*asset_analysis.ValidationError](err)
		retryErr := s.distillFailure(ctx, t.run, err)
		if retryErr != nil || rejected {
			s.releaseCharge(ctx, chargeID)
		}
		return retryErr
	}
	s.limiter.DispatchCrossing(ctx, tenantID, dec)
	return s.saveDistill(ctx, t, resp)
}

// FailDistill marks a run failed after its last attempt, so it never stays
// pending.
func (s *Service) FailDistill(ctx context.Context, tenantID, runID string, cause error) {
	ctx = tenantctx.With(ctx, tenantID)
	run, err := s.repos.Distills.Get(ctx, runID)
	if err != nil || !run.Status.Active() {
		return
	}
	code, reason := models.AnalysisCodeModelUnavailable, modelExhaustedReason
	if errors.Is(cause, errSourceUnreadable) {
		code, reason = models.AnalysisCodeSourceUnavailable, sourceMissingReason
	}
	s.setDistillStatus(ctx, run, models.IdeaDistillStatusFailed, code, reason)
}

// loadDistill reads the run, its lens, asset and campaign. It returns nil
// when there is nothing to do: the run is gone or settled, or its lens went
// away (the run is then failed).
func (s *Service) loadDistill(ctx context.Context, runID string) (*distillTarget, error) {
	run, err := s.repos.Distills.Get(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load distill run: %w", err)
	}
	if !run.Status.Active() {
		return nil, nil
	}
	t := &distillTarget{run: run}
	gone := func() (*distillTarget, error) {
		s.setDistillStatus(ctx, run, models.IdeaDistillStatusFailed, models.AnalysisCodeLensGone, lensGoneReason)
		return nil, nil
	}
	if t.lens, err = s.repos.Analyses.GetLens(ctx, run.AssetID, run.CampaignID); errors.Is(err, sql.ErrNoRows) || (err == nil && t.lens.ID != run.AnalysisID) {
		return gone()
	} else if err != nil {
		return nil, fmt.Errorf("load lens: %w", err)
	}
	if t.asset, err = s.repos.Assets.GetByID(ctx, run.AssetID); errors.Is(err, sql.ErrNoRows) {
		return gone()
	} else if err != nil {
		return nil, fmt.Errorf("load asset: %w", err)
	}
	if run.CampaignID != nil {
		t.campaign, err = s.repos.Campaigns.GetByID(ctx, *run.CampaignID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !hasAsset(t.campaign, run.AssetID)) {
			return gone()
		}
		if err != nil {
			return nil, fmt.Errorf("load campaign: %w", err)
		}
	}
	return t, nil
}

// admitDistill applies the tier gates; a gated run is marked skipped.
func (s *Service) admitDistill(ctx context.Context, t *distillTarget) (entitlements.Decision, bool) {
	tenantID, _ := tenantctx.From(ctx)
	if on, _ := s.limiter.Gate(ctx, tenantID, FeatureAnalysis); !on {
		s.setDistillStatus(ctx, t.run, models.IdeaDistillStatusSkipped, models.AnalysisCodeEntitlementDisabled, disabledReason)
		return entitlements.Decision{}, false
	}
	dec, _ := s.limiter.Allow(ctx, tenantID, FeatureMonthlyDistills)
	if !dec.Allowed {
		s.setDistillStatus(ctx, t.run, models.IdeaDistillStatusSkipped, models.AnalysisCodeLimitReached, distillLimitReason)
		return dec, false
	}
	return dec, true
}

// distillRequest builds the flow input: the lens context, the stored
// analysis and the ideas already in scope.
func (s *Service) distillRequest(ctx context.Context, tenantID string, t *distillTarget, src asset_analysis.Source) (asset_analysis.DistillRequest, error) {
	lens, err := s.lensContext(ctx, tenantID, t.asset, t.campaign)
	if err != nil {
		return asset_analysis.DistillRequest{}, err
	}
	ideas, err := s.repos.Ideas.List(ctx, ideaScope(t.campaign))
	if err != nil {
		return asset_analysis.DistillRequest{}, fmt.Errorf("list ideas: %w", err)
	}
	prior, known := priorIdeas(ideas, t.lens.ID)
	return asset_analysis.DistillRequest{
		CampaignID:  ptrValue(t.run.CampaignID),
		Source:      src,
		Lens:        lens,
		Summary:     t.lens.Summary,
		KeyPoints:   t.lens.Result.KeyPoints,
		Prior:       prior,
		KnownTitles: known,
		Focus:       t.run.Focus,
		Count:       t.run.Count,
	}, nil
}

// priorIdeas orders the ideas in scope for the prompt, this lens's first and
// then the rest, newest first in each, capped at maxPriorIdeas. known holds
// every title.
func priorIdeas(ideas []models.Idea, lensID string) (prior []asset_analysis.PriorIdea, known []string) {
	ordered := slices.Clone(ideas)
	own := func(i models.Idea) bool { return i.AssetAnalysisID != nil && *i.AssetAnalysisID == lensID }
	slices.SortStableFunc(ordered, func(a, b models.Idea) int {
		if oa, ob := own(a), own(b); oa != ob {
			if oa {
				return -1
			}
			return 1
		}
		return b.CreatedAt.Compare(a.CreatedAt)
	})
	known = make([]string, 0, len(ordered))
	for _, idea := range ordered {
		known = append(known, idea.Title)
		if len(prior) == maxPriorIdeas {
			continue
		}
		prior = append(prior, asset_analysis.PriorIdea{
			Title:   idea.Title,
			Angle:   ptrValue(idea.Angle),
			Verdict: priorVerdict(idea.Verdict),
			Sources: idea.Sources,
		})
	}
	return prior, known
}

func priorVerdict(v *models.IdeaVerdict) string {
	if v == nil {
		return asset_analysis.PriorUndecided
	}
	switch *v {
	case models.IdeaVerdictYes:
		return asset_analysis.PriorAccepted
	case models.IdeaVerdictLater:
		return asset_analysis.PriorLater
	case models.IdeaVerdictNo:
		return asset_analysis.PriorRejected
	}
	return asset_analysis.PriorUndecided
}

// distillFailure settles what a failed flow call means for the run, as
// handleFailure does for an analysis.
func (s *Service) distillFailure(ctx context.Context, run *models.IdeaDistillRun, err error) error {
	if _, ok := errors.AsType[*asset_analysis.ValidationError](err); ok {
		s.setDistillStatus(ctx, run, models.IdeaDistillStatusFailed, models.AnalysisCodeEmptySource, emptySourceReason)
		return nil
	}
	if aiErr, ok := errors.AsType[*asset_analysis.AIError](err); ok && aiErr.Invalid {
		s.setDistillStatus(ctx, run, models.IdeaDistillStatusFailed, models.AnalysisCodeInvalidOutput, failedOutputReason)
		return nil
	}
	if aiErr, ok := errors.AsType[*asset_analysis.AIError](err); ok && aiErr.Refused {
		s.setDistillStatus(ctx, run, models.IdeaDistillStatusFailed, models.AnalysisCodeModelRefused, refusedReason)
		return nil
	}
	if _, ok := errors.AsType[*usage.LimitExceededError](err); ok {
		s.setDistillStatus(ctx, run, models.IdeaDistillStatusSkipped, models.AnalysisCodeLimitReached, limitReachedReason)
		return nil
	}
	s.setDistillStatus(ctx, run, models.IdeaDistillStatusPending, "", unavailableReason)
	return err
}

// saveDistill stores the new ideas under the lens row lock. A lens that went
// away meanwhile fails the run instead.
func (s *Service) saveDistill(ctx context.Context, t *distillTarget, resp *asset_analysis.DistillResponse) error {
	ideas, err := ideaRows(t.lens, resp.Ideas)
	if err != nil {
		return err
	}
	t.run.ModelID = resp.ModelID
	saved := false
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := s.repos.Analyses.LockLens(ctx, tx, t.lens.AssetID, t.lens.CampaignID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if t.lens.CampaignID != nil {
			attached, err := s.stillAttached(ctx, tx, *t.lens.CampaignID, t.lens.AssetID)
			if err != nil || !attached {
				return err
			}
		}
		saved = true
		return s.repos.Distills.SaveIdeas(ctx, tx, t.run, ideas)
	})
	if err != nil {
		return fmt.Errorf("save distilled ideas: %w", err)
	}
	if !saved {
		s.setDistillStatus(ctx, t.run, models.IdeaDistillStatusFailed, models.AnalysisCodeLensGone, lensGoneReason)
		return nil
	}
	kind := Lens{CampaignID: t.lens.CampaignID}.kind()
	distillRunsTotal.Add(kind+"/"+string(models.IdeaDistillStatusReady), 1)
	distillIdeasTotal.Add(int64(len(ideas)))
	distillDroppedTotal.Add(int64(resp.Dropped))
	for _, idea := range ideas {
		ideasScoredTotal.Add(kind+"/"+string(ideafit.BandOf(idea.FitScore)), 1)
	}
	s.publishDistill(ctx, t.run)
	return nil
}

func (s *Service) recordCharge(ctx context.Context, runID string) string {
	id, err := s.repos.Distills.RecordCharge(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "could not record the distill run", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
	return id
}

func (s *Service) releaseCharge(ctx context.Context, id string) {
	if id == "" {
		return
	}
	if err := s.repos.Distills.DeleteCharge(ctx, id); err != nil {
		slog.WarnContext(ctx, "could not release the distill run", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
}

func (s *Service) setDistillStatus(ctx context.Context, run *models.IdeaDistillRun, status models.IdeaDistillStatus, code, reason string) {
	if err := s.repos.Distills.SetStatus(ctx, run.ID, status, code, reason); err != nil {
		slog.WarnContext(ctx, "could not set distill status", logging.AttrComponent, logComponent,
			"run_id", run.ID, "status", status, logging.AttrError, err)
		return
	}
	run.Status, run.FailureCode, run.FailureReason = status, code, reason
	if !status.Active() {
		distillRunsTotal.Add(Lens{CampaignID: run.CampaignID}.kind()+"/"+string(status), 1)
	}
	s.publishDistill(ctx, run)
}

// DistillEventPayload is the SSE body of an asset.ideas.distill.updated
// frame.
type DistillEventPayload struct {
	AssetID    string                   `json:"asset_id"`
	CampaignID *string                  `json:"campaign_id"`
	AnalysisID string                   `json:"analysis_id"`
	RunID      string                   `json:"run_id"`
	Status     models.IdeaDistillStatus `json:"status"`
	IdeasCount int                      `json:"ideas_count"`
}

// publishDistill sends the run's state to the asset's topic and, for a
// campaign lens, the campaign's. Best effort.
func (s *Service) publishDistill(ctx context.Context, run *models.IdeaDistillRun) {
	if s.hub == nil {
		return
	}
	tenantID, _ := tenantctx.From(ctx)
	payload := DistillEventPayload{
		AssetID: run.AssetID, CampaignID: run.CampaignID, AnalysisID: run.AnalysisID,
		RunID: run.ID, Status: run.Status, IdeasCount: run.IdeasCount,
	}
	topics := []string{"entity:asset:" + run.AssetID}
	if run.CampaignID != nil {
		topics = append(topics, "entity:campaign:"+*run.CampaignID)
	}
	for _, topic := range topics {
		_ = s.hub.Publish(ctx, eventhub.Event{Topic: topic, TenantID: tenantID, Type: distillEventType, Payload: payload})
	}
}
