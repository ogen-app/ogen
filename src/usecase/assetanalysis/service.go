// Package assetanalysis runs the LLM analysis of text assets: it reads an
// asset through a lens (the workspace, or one campaign it serves), skips runs
// whose inputs have not changed, applies the tier gates, and stores the
// result with the ideas it distilled.
package assetanalysis

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

const logComponent = "assetanalysis"

// Entitlement keys that gate analysis per tier.
const (
	FeatureAnalysis      = "asset_analysis"
	FeatureMonthlyRuns   = "asset_analyses_per_month"
	ideaAuthor           = "Ogen"
	eventType            = "asset.analysis.updated"
	unavailableReason    = "Analysis is temporarily unavailable — it will be retried"
	failedOutputReason   = "The model's answer could not be read"
	emptySourceReason    = "The asset has no text to analyse"
	disabledReason       = "Asset analysis is not included in this plan"
	limitReachedReason   = "This month's asset analyses are used up"
	sourceMissingReason  = "The asset's text could not be read"
	modelExhaustedReason = "The analysis model could not be reached"
)

var (
	runsTotal  = expvar.NewMap("ogen_asset_analysis_runs_total")
	noopsTotal = expvar.NewInt("ogen_asset_analysis_noop_total")
	ideasTotal = expvar.NewInt("ogen_asset_analysis_ideas_total")
)

// ErrUnavailable means no analysis model is configured right now; the job
// retries later.
var ErrUnavailable = errors.New("asset analysis unavailable: no model configured")

// Analyzer runs the analysis flow.
type Analyzer func(ctx context.Context, req asset_analysis.AnalyzeRequest) (*asset_analysis.AnalyzeResponse, error)

// Repos are the stores the service reads and writes.
type Repos struct {
	Analyses  repository.AssetAnalysisRepository
	Assets    repository.AssetRepository
	Chunks    repository.AssetChunksRepository
	Campaigns repository.CampaignRepository
	Posts     repository.PostRepository
	Ideas     repository.IdeaRepository
	Brands    repository.BrandRepository
	Tenants   repository.TenantRepository
}

// Service runs analyses. It is safe for concurrent use.
type Service struct {
	db       *bun.DB
	repos    Repos
	limiter  *entitlements.Limiter
	hub      eventhub.Hub
	analyzer atomic.Pointer[Analyzer]
	now      func() time.Time

	jobs            Jobs
	rebriefDebounce time.Duration
}

// New builds the service. limiter and hub may be nil.
func New(db *bun.DB, repos Repos, limiter *entitlements.Limiter, hub eventhub.Hub) *Service {
	return &Service{db: db, repos: repos, limiter: limiter, hub: hub, now: time.Now, rebriefDebounce: DefaultRebriefDebounce}
}

// SetAnalyzer binds the flow once the model runtime is up; nil unbinds it.
func (s *Service) SetAnalyzer(a Analyzer) {
	if a == nil {
		s.analyzer.Store(nil)
		return
	}
	s.analyzer.Store(&a)
}

// Lens names one analysis: an asset and the campaign it is read for (nil for
// the workspace lens).
type Lens struct {
	TenantID   string
	AssetID    string
	CampaignID *string
}

func (l Lens) kind() string {
	if l.CampaignID == nil {
		return "workspace"
	}
	return "campaign"
}

// target is everything a run needs, loaded and checked.
type target struct {
	lens     Lens
	asset    *models.Asset
	campaign *models.Campaign
	row      *models.AssetAnalysis
}

// Run analyses one lens. It returns nil when there is nothing to do (the
// asset or campaign is gone, the inputs are unchanged, the tier is gated) or
// once the result is stored; an error means the run should be retried.
func (s *Service) Run(ctx context.Context, l Lens) error {
	ctx = tenantctx.With(ctx, l.TenantID)
	t, err := s.load(ctx, l)
	if err != nil || t == nil {
		return err
	}
	src, err := s.buildSource(ctx, t.asset)
	if err != nil {
		return fmt.Errorf("%w: %w", errSourceUnreadable, err)
	}
	if !hasText(src) {
		// Silent audio, or a partial transcript that withholds its text: an
		// empty analysis, not a failure, and no model call.
		return s.save(ctx, t, &asset_analysis.AnalyzeResponse{}, fingerprint(src, false, asset_analysis.Lens{}))
	}
	lensCtx, err := s.buildLens(ctx, l.TenantID, t.asset, t.campaign, t.row.ID)
	if err != nil {
		return err
	}
	fp := fingerprint(src, t.asset.Status == models.AssetStatusPartial, lensCtx)
	if t.row.Status == models.AssetAnalysisStatusReady && t.row.ContextFingerprint == fp {
		noopsTotal.Add(1)
		return nil
	}
	dec, ok := s.admit(ctx, t)
	if !ok {
		return nil
	}
	analyze := s.analyzer.Load()
	if analyze == nil {
		return ErrUnavailable
	}
	s.setStatus(ctx, t, models.AssetAnalysisStatusRunning, "", "")
	resp, err := (*analyze)(ctx, asset_analysis.AnalyzeRequest{
		CampaignID: ptrValue(l.CampaignID),
		Source:     src,
		Lens:       lensCtx,
	})
	if err != nil {
		return s.handleFailure(ctx, t, err)
	}
	if err := s.repos.Analyses.RecordRun(ctx, t.row.ID); err != nil {
		slog.WarnContext(ctx, "could not record the run", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
	s.limiter.DispatchCrossing(ctx, l.TenantID, dec)
	return s.save(ctx, t, resp, fp)
}

// Fail marks a lens failed after its last attempt, so it never stays
// running.
func (s *Service) Fail(ctx context.Context, l Lens, cause error) {
	ctx = tenantctx.With(ctx, l.TenantID)
	row, err := s.repos.Analyses.GetLens(ctx, l.AssetID, l.CampaignID)
	if err != nil {
		return
	}
	code, reason := models.AnalysisCodeModelUnavailable, modelExhaustedReason
	if errors.Is(cause, errSourceUnreadable) {
		code, reason = models.AnalysisCodeSourceUnavailable, sourceMissingReason
	}
	s.setStatus(ctx, &target{lens: l, row: row}, models.AssetAnalysisStatusFailed, code, reason)
}

var errSourceUnreadable = errors.New("asset source unreadable")

// load re-reads the asset and campaign and makes sure the lens row exists.
// It returns nil when the lens no longer applies.
func (s *Service) load(ctx context.Context, l Lens) (*target, error) {
	asset, err := s.repos.Assets.GetByID(ctx, l.AssetID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load asset: %w", err)
	}
	if !models.IsAnalyzableAssetType(asset.Type) || !models.IsAnalyzableAssetStatus(asset.Status) {
		return nil, nil
	}
	t := &target{lens: l, asset: asset}
	if l.CampaignID != nil {
		campaign, err := s.repos.Campaigns.GetByID(ctx, *l.CampaignID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("load campaign: %w", err)
		}
		if !hasAsset(campaign, l.AssetID) {
			return nil, nil
		}
		t.campaign = campaign
	}
	if t.row, err = s.repos.Analyses.Ensure(ctx, s.db, l.AssetID, l.CampaignID); err != nil {
		return nil, fmt.Errorf("ensure lens: %w", err)
	}
	return t, nil
}

// admit applies the tier gates. A gated lens is marked skipped.
func (s *Service) admit(ctx context.Context, t *target) (entitlements.Decision, bool) {
	tenantID := t.lens.TenantID
	if on, _ := s.limiter.Gate(ctx, tenantID, FeatureAnalysis); !on {
		s.setStatus(ctx, t, models.AssetAnalysisStatusSkipped, models.AnalysisCodeEntitlementDisabled, disabledReason)
		return entitlements.Decision{}, false
	}
	dec, _ := s.limiter.Allow(ctx, tenantID, FeatureMonthlyRuns)
	if !dec.Allowed {
		s.setStatus(ctx, t, models.AssetAnalysisStatusSkipped, models.AnalysisCodeLimitReached, limitReachedReason)
		return dec, false
	}
	return dec, true
}

// handleFailure settles what a failed flow call means for the lens: a
// problem retrying cannot fix is stored, anything else goes back to River.
func (s *Service) handleFailure(ctx context.Context, t *target, err error) error {
	if _, ok := errors.AsType[*asset_analysis.ValidationError](err); ok {
		s.setStatus(ctx, t, models.AssetAnalysisStatusFailed, models.AnalysisCodeEmptySource, emptySourceReason)
		return nil
	}
	if aiErr, ok := errors.AsType[*asset_analysis.AIError](err); ok && aiErr.Invalid {
		s.setStatus(ctx, t, models.AssetAnalysisStatusFailed, models.AnalysisCodeInvalidOutput, failedOutputReason)
		return nil
	}
	if _, ok := errors.AsType[*usage.LimitExceededError](err); ok {
		s.setStatus(ctx, t, models.AssetAnalysisStatusSkipped, models.AnalysisCodeLimitReached, limitReachedReason)
		return nil
	}
	s.setStatus(ctx, t, models.AssetAnalysisStatusPending, "", unavailableReason)
	return err
}

// save stores the result under the lens row lock. A campaign lens whose asset
// was detached meanwhile is dropped instead.
func (s *Service) save(ctx context.Context, t *target, resp *asset_analysis.AnalyzeResponse, fp string) error {
	row := t.row
	now := s.now().UTC()
	row.Status = models.AssetAnalysisStatusReady
	row.Strategy = nil
	if resp.Strategy != "" {
		row.Strategy = new(models.AssetAnalysisStrategy(resp.Strategy))
	}
	row.Summary = resp.Analysis.Summary
	row.Result = toResult(resp)
	row.Language = resp.Analysis.Language
	row.PartialSource = t.asset.Status == models.AssetStatusPartial
	row.Truncated = resp.Truncated
	row.SourceTokens = resp.SourceTokens
	row.ContextFingerprint = fp
	row.ModelID = resp.ModelID
	row.FailureCode, row.FailureReason = "", ""
	row.AnalyzedAt = &now

	ideas, err := ideaRows(row, resp.Analysis.Ideas)
	if err != nil {
		return err
	}
	saved := false
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := s.repos.Analyses.LockLens(ctx, tx, row.AssetID, row.CampaignID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if row.CampaignID != nil {
			attached, err := s.stillAttached(ctx, tx, *row.CampaignID, row.AssetID)
			if err != nil || !attached {
				return err
			}
		}
		saved = true
		return s.repos.Analyses.SaveResult(ctx, tx, row, ideas)
	})
	if err != nil {
		return fmt.Errorf("save analysis: %w", err)
	}
	if !saved {
		return nil
	}
	runsTotal.Add(t.lens.kind()+"/"+cmp.Or(string(resp.Strategy), "empty"), 1)
	ideasTotal.Add(int64(len(ideas)))
	s.publish(ctx, t.lens, row.ID, models.AssetAnalysisStatusReady, len(ideas))
	return nil
}

func (s *Service) stillAttached(ctx context.Context, tx bun.Tx, campaignID, assetID string) (bool, error) {
	n, err := tx.NewSelect().
		Model((*models.Campaign)(nil)).
		Where("c.id = ?", campaignID).
		Where("c.deleted_at IS NULL").
		Where("c.asset_ids @> ?::jsonb", jsonIDs(assetID)).
		Count(ctx)
	return n > 0, err
}

func (s *Service) setStatus(ctx context.Context, t *target, status models.AssetAnalysisStatus, code, reason string) {
	if err := s.repos.Analyses.SetStatus(ctx, s.db, t.row.ID, status, code, reason); err != nil {
		slog.WarnContext(ctx, "could not set analysis status", logging.AttrComponent, logComponent,
			"analysis_id", t.row.ID, "status", status, logging.AttrError, err)
		return
	}
	if status != models.AssetAnalysisStatusPending {
		runsTotal.Add(t.lens.kind()+"/"+string(status), 1)
	}
	s.publish(ctx, t.lens, t.row.ID, status, 0)
}

// EventPayload is the SSE body of an asset.analysis.updated frame.
type EventPayload struct {
	AssetID    string                     `json:"asset_id"`
	CampaignID *string                    `json:"campaign_id"`
	AnalysisID string                     `json:"analysis_id"`
	Status     models.AssetAnalysisStatus `json:"status"`
	IdeasCount int                        `json:"ideas_count"`
}

// publish sends the status change to the asset's topic and, for a campaign
// lens, the campaign's. Best effort.
func (s *Service) publish(ctx context.Context, l Lens, analysisID string, status models.AssetAnalysisStatus, ideas int) {
	if s.hub == nil {
		return
	}
	payload := EventPayload{AssetID: l.AssetID, CampaignID: l.CampaignID, AnalysisID: analysisID, Status: status, IdeasCount: ideas}
	topics := []string{"entity:asset:" + l.AssetID}
	if l.CampaignID != nil {
		topics = append(topics, "entity:campaign:"+*l.CampaignID)
	}
	for _, topic := range topics {
		_ = s.hub.Publish(ctx, eventhub.Event{Topic: topic, TenantID: l.TenantID, Type: eventType, Payload: payload})
	}
}

func toResult(resp *asset_analysis.AnalyzeResponse) models.AssetAnalysisResult {
	a := resp.Analysis
	r := models.AssetAnalysisResult{
		KeyPoints:     a.KeyPoints,
		Topics:        a.Topics,
		OpenQuestions: a.OpenQuestions,
		MapModelID:    resp.MapModelID,
	}
	for _, e := range a.Entities {
		r.Entities = append(r.Entities, models.AnalysisEntity{Name: e.Name, Kind: e.Kind})
	}
	for _, p := range a.Participants {
		r.Participants = append(r.Participants, models.AnalysisParticipant{Name: p.Name, Role: p.Role, Confidence: p.Confidence})
	}
	for _, c := range a.Commitments {
		r.Commitments = append(r.Commitments, models.AnalysisCommitment{
			Owner: c.Owner, Commitment: c.Commitment, Due: c.Due, SourceLabel: c.SourceLabel, Confidence: c.Confidence,
		})
	}
	for _, d := range a.Decisions {
		r.Decisions = append(r.Decisions, models.AnalysisDecision{Text: d.Text, SourceLabel: d.SourceLabel})
	}
	if a.Relevance != nil {
		r.Relevance = &models.AnalysisRelevance{Score: a.Relevance.Score, Why: a.Relevance.Why}
	}
	return r
}

// ideaRows turns the distilled ideas into inbox rows of the lens's scope.
func ideaRows(row *models.AssetAnalysis, ideas []asset_analysis.Idea) ([]models.Idea, error) {
	out := make([]models.Idea, 0, len(ideas))
	for _, idea := range ideas {
		id, err := models.NewID()
		if err != nil {
			return nil, err
		}
		out = append(out, models.Idea{
			ID:              id,
			Title:           idea.Title,
			Note:            ideaNote(idea),
			CampaignID:      row.CampaignID,
			Origin:          models.IdeaOriginAssetAnalysis,
			AssetAnalysisID: new(row.ID),
			CreatedByName:   ideaAuthor,
		})
	}
	return out, nil
}

// ideaNote is the idea's angle and rationale with its citations.
func ideaNote(idea asset_analysis.Idea) string {
	var parts []string
	if idea.Angle != "" {
		parts = append(parts, idea.Angle)
	}
	if idea.Rationale != "" {
		parts = append(parts, idea.Rationale)
	}
	if len(idea.SourceLabels) > 0 {
		parts = append(parts, "Sources: "+strings.Join(idea.SourceLabels, ", "))
	}
	return strings.Join(parts, "\n\n")
}

// hasText reports whether any segment has non-blank text.
func hasText(src asset_analysis.Source) bool {
	return slices.ContainsFunc(src.Segments, func(seg asset_analysis.Segment) bool {
		return strings.TrimSpace(seg.Text) != ""
	})
}

// jsonIDs encodes ids as a jsonb array literal for containment checks.
func jsonIDs(ids ...string) string {
	b, _ := json.Marshal(ids)
	return string(b)
}

func hasAsset(c *models.Campaign, assetID string) bool {
	return slices.Contains(c.AssetIDs, assetID)
}

func ptrValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
