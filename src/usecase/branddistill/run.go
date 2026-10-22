package branddistill

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/assetsource"
	"github.com/ogen-app/ogen/src/usecase/brandsource"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

// mapLimit caps the links one site map returns.
const mapLimit = 200

// Reasons a run fails, as the user reads them.
const (
	noSourcesReason = "There was not enough text to read: the pages or files came back nearly empty. Try a document, or answer a few questions."
	scrapingReason  = "Reading websites is not set up for this workspace."
	refusedReason   = "The model declined to read this material."
	invalidReason   = "The proposal came back unreadable. Try again."
	internalReason  = "Something went wrong while reading. Try again."
	inactiveReason  = "The workspace is not active."
	disabledReason  = "Reading your brand is not part of this plan."
	limitReasonText = "This month's brand reads are used up."
)

// Run moves a run on. It returns nil once the run is settled or closed under
// it, ErrWaiting while its sources ingest, and other errors when the job
// should retry.
func (s *Service) Run(ctx context.Context, tenantID, runID string) error {
	err := s.run(ctx, tenantID, runID)
	if errors.Is(err, repository.ErrBrandDistillClosed) {
		return nil
	}
	return err
}

func (s *Service) run(ctx context.Context, tenantID, runID string) error {
	ctx = tenantctx.With(ctx, tenantID)
	run, err := s.repos.Runs.Get(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load the run: %w", err)
	}
	if !run.Status.Open() {
		return nil
	}
	if run.ParentRunID == nil {
		if ok := s.admit(ctx, run); !ok {
			return nil
		}
		if run.WebsiteURL != nil && run.Website == nil {
			if err := s.collectWebsite(ctx, run); err != nil || !run.Status.Open() {
				return err
			}
		}
		waiting, err := s.settleSources(ctx, run)
		if err != nil {
			return err
		}
		if waiting {
			return ErrWaiting
		}
		if run.Candidates == nil {
			if err := s.read(ctx, run); err != nil || !run.Status.Open() {
				return err
			}
		}
	}
	return s.compose(ctx, run)
}

// Fail settles a run whose job gave up, so it never stays open.
func (s *Service) Fail(ctx context.Context, tenantID, runID string, cause error) {
	ctx = tenantctx.With(ctx, tenantID)
	run, err := s.repos.Runs.Get(ctx, runID)
	if err != nil || !run.Status.Open() {
		return
	}
	slog.WarnContext(ctx, "brand distillation gave up", logging.AttrComponent, logComponent, "run_id", runID, logging.AttrError, cause)
	s.fail(ctx, run, models.BrandDistillCodeInternal, internalReason)
}

// FailInactive settles the run of a suspended or deleted workspace.
func (s *Service) FailInactive(ctx context.Context, tenantID, runID string) {
	ctx = tenantctx.With(ctx, tenantID)
	if run, err := s.repos.Runs.Get(ctx, runID); err == nil && run.Status.Open() {
		s.fail(ctx, run, models.BrandDistillCodeTenantInactive, inactiveReason)
	}
}

// admit applies the tier gates before any work; a gated run fails.
func (s *Service) admit(ctx context.Context, run *models.BrandDistillRun) bool {
	if run.Charged {
		return true
	}
	tenantID, _ := tenantctx.From(ctx)
	if on, _ := s.limiter.Gate(ctx, tenantID, FeatureGate); !on {
		s.fail(ctx, run, models.BrandDistillCodeDisabled, disabledReason)
		return false
	}
	if dec, _ := s.limiter.Allow(ctx, tenantID, FeatureMonthly); !dec.Allowed {
		s.fail(ctx, run, models.BrandDistillCodeLimitReached, limitReasonText)
		return false
	}
	return true
}

// collectWebsite maps the site, picks its pages and imports them as brand
// sources. A site map that fails for good falls back to the homepage.
func (s *Service) collectWebsite(ctx context.Context, run *models.BrandDistillRun) error {
	if s.deps.Mapper == nil || s.deps.Importer == nil || !s.deps.Importer.Configured(ctx) {
		s.fail(ctx, run, models.BrandDistillCodeScraping, scrapingReason)
		return nil
	}
	site := *run.WebsiteURL
	root, _, _ := siteRoot(site)
	links, err := s.deps.Mapper.Map(ctx, firecrawl.MapRequest{URL: root, Limit: mapLimit})
	if err != nil && firecrawl.IsTransient(err) {
		return fmt.Errorf("map %s: %w", root, err)
	}
	if err == nil && s.deps.Recorder != nil {
		s.deps.Recorder.Record(ctx, firecrawl.VendorFirecrawl, "url_map", vendors.MeterEvent{
			Model: "map", Operation: firecrawl.OpMap, Usage: vendors.Usage{vendors.KindURLMap: 1},
		})
	}
	run.Website = &models.DistillWebsite{URL: site, Mapped: err == nil && len(links) > 1, PagesFound: len(links)}
	userID := ptrValue(run.CreatedBy)
	mark, err := s.deps.Marker.Prepare(ctx, models.AssetRoleWebsite, run.ID, userID)
	if err != nil {
		return err
	}
	tenantID, _ := tenantctx.From(ctx)
	for _, page := range SelectPages(site, links, MaxWebsitePages) {
		src := models.DistillSource{Title: page, Type: models.AssetTypeURL, Role: models.DistillRoleWebsite, URL: page, Status: models.DistillSourcePending}
		res, err := s.importPage(ctx, tenantID, userID, page, &mark)
		switch {
		case errors.Is(err, urlimport.ErrUnavailable):
			s.fail(ctx, run, models.BrandDistillCodeScraping, scrapingReason)
			return nil
		case isQuota(err):
			src.Status = models.DistillSourceSkippedQuota
		case err != nil:
			s.warn(ctx, "import "+page, run.ID, err)
			src.Status = models.DistillSourceFailed
		default:
			src.AssetID = res.Asset.ID
		}
		run.Sources = append(run.Sources, src)
	}
	run.Status = models.BrandDistillStatusCollecting
	return s.save(ctx, run, "status", "website", "sources")
}

func (s *Service) importPage(ctx context.Context, tenantID, userID, page string, mark *brandsource.Mark) (*urlimport.Result, error) {
	normalized, err := urlimport.Normalize(ctx, page)
	if err != nil {
		return nil, err
	}
	return s.deps.Importer.Import(ctx, urlimport.Input{TenantID: tenantID, UserID: userID, URL: normalized, Mark: mark})
}

func isQuota(err error) bool {
	if _, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
		return true
	}
	_, ok := errors.AsType[*entitlements.FeatureNotAvailableError](err)
	return ok
}

// settleSources reads where each source's ingestion is. It reports true
// while some are still ingesting and the run has not waited collectTimeout;
// after that, the unfinished ones are left out.
func (s *Service) settleSources(ctx context.Context, run *models.BrandDistillRun) (bool, error) {
	pending := false
	for i := range run.Sources {
		src := &run.Sources[i]
		if src.AssetID == "" || src.Status == models.DistillSourceTimedOut {
			continue
		}
		a, err := s.repos.Assets.GetByID(ctx, src.AssetID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			src.Status = models.DistillSourceFailed
			continue
		case err != nil:
			return false, fmt.Errorf("load source %s: %w", src.AssetID, err)
		}
		src.Status = sourceStatus(a.Status)
		if a.Title != "" {
			src.Title = a.Title
		}
		if src.Role == models.DistillRoleWebsite && src.Status != models.DistillSourcePending && src.Status != models.DistillSourceFailed &&
			len(strings.Fields(a.Content)) < minPageWords {
			src.Status = models.DistillSourceThin
		}
		pending = pending || src.Status == models.DistillSourcePending
	}
	if pending && s.now().Sub(run.CreatedAt) < collectTimeout {
		run.Status = models.BrandDistillStatusCollecting
		return true, s.save(ctx, run, "status", "sources")
	}
	for i := range run.Sources {
		if run.Sources[i].Status == models.DistillSourcePending {
			run.Sources[i].Status = models.DistillSourceTimedOut
		}
	}
	return false, nil
}

// sourceStatus maps an asset's ingest status onto a source's.
func sourceStatus(assetStatus string) string {
	switch assetStatus {
	case models.AssetStatusReady:
		return models.DistillSourceReady
	case models.AssetStatusPartial:
		return models.DistillSourcePartial
	case models.AssetStatusFailed:
		return models.DistillSourceFailed
	}
	return models.DistillSourcePending
}

// read builds the segments of the usable sources and keeps the passages the
// extract stage finds in them. A run with answers and no readable source
// skips reading; one with neither fails.
func (s *Service) read(ctx context.Context, run *models.BrandDistillRun) error {
	segs, err := s.segments(ctx, run)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		if len(run.Answers) == 0 {
			s.fail(ctx, run, models.BrandDistillCodeNoSources, noSourcesReason)
			return nil
		}
		run.Candidates = []models.DistillCandidate{}
		return nil
	}
	flows := s.flows.Load()
	if flows == nil {
		return ErrUnavailable
	}
	if err := s.charge(ctx, run); err != nil {
		return err
	}
	run.Status = models.BrandDistillStatusReading
	if err := s.save(ctx, run, "status", "sources"); err != nil {
		return err
	}
	resp, err := flows.Extract(ctx, brand_distill.ExtractRequest{Segments: segs})
	if err != nil {
		return s.flowFailure(ctx, run, err)
	}
	byRef := make(map[string]brand_distill.Segment, len(segs))
	for _, seg := range segs {
		byRef[seg.Ref] = seg
	}
	for i := range resp.Candidates {
		c := &resp.Candidates[i]
		c.Label, c.AssetID = byRef[c.Ref].Label, assetOfRef(run, c.Ref)
	}
	run.Candidates = resp.Candidates
	if run.ModelIDs == nil {
		run.ModelIDs = models.StringMap{}
	}
	run.ModelIDs["extract"] = resp.ModelID
	candidatesTotal.Add(int64(len(resp.Candidates)))
	droppedTotal.Add("unquoted", int64(resp.Dropped))
	if len(run.Candidates) == 0 && len(run.Answers) == 0 {
		s.fail(ctx, run, models.BrandDistillCodeNoSources, noSourcesReason)
		return nil
	}
	return s.save(ctx, run, "candidates", "model_ids")
}

// charge counts a first run against the monthly allowance once, before its
// first model call.
func (s *Service) charge(ctx context.Context, run *models.BrandDistillRun) error {
	if run.Charged || run.ParentRunID != nil {
		return nil
	}
	if err := s.repos.Runs.RecordCharge(ctx, run.ID); err != nil {
		return fmt.Errorf("charge the run: %w", err)
	}
	run.Charged = true
	return s.save(ctx, run, "charged")
}

// segments reads every usable source, refs "s<source>.<segment>", up to
// maxReadTokens; a source cut short is marked truncated.
func (s *Service) segments(ctx context.Context, run *models.BrandDistillRun) ([]brand_distill.Segment, error) {
	var out []brand_distill.Segment
	used := 0
	for i := range run.Sources {
		src := &run.Sources[i]
		if src.Status != models.DistillSourceReady && src.Status != models.DistillSourcePartial {
			continue
		}
		a, err := s.repos.Assets.GetByID(ctx, src.AssetID)
		if errors.Is(err, sql.ErrNoRows) {
			src.Status = models.DistillSourceFailed
			continue
		}
		if err != nil {
			return nil, err
		}
		read, err := assetsource.Read(ctx, s.repos.Chunks, a)
		if err != nil {
			return nil, fmt.Errorf("read source %s: %w", a.ID, err)
		}
		for j, seg := range read.Segments {
			if strings.TrimSpace(seg.Text) == "" {
				continue
			}
			n := seg.Tokens
			if n == 0 {
				n = brand_distill.EstimateTokens(seg.Text)
			}
			if used+n > maxReadTokens {
				src.Truncated = true
				break
			}
			used += n
			out = append(out, brand_distill.Segment{
				Ref:       fmt.Sprintf("s%d.%d", i+1, j+1),
				Label:     segmentLabel(*src, read, seg),
				Text:      seg.Text,
				Tokens:    seg.Tokens,
				Recording: read.Recording,
			})
		}
	}
	return out, nil
}

// segmentLabel is how a person cites a segment: a page's host and path, a
// file's title and the segment's own label.
func segmentLabel(src models.DistillSource, read assetsource.Source, seg assetsource.Segment) string {
	if src.URL != "" {
		if u, err := url.Parse(src.URL); err == nil {
			return strings.TrimPrefix(u.Host, "www.") + strings.TrimSuffix(u.EscapedPath(), "/")
		}
	}
	if len(read.Segments) == 1 && strings.HasPrefix(seg.Label, "Part ") {
		return read.Title
	}
	return read.Title + " · " + seg.Label
}

// assetOfRef is the asset a "s<source>.<segment>" ref points into.
func assetOfRef(run *models.BrandDistillRun, ref string) string {
	var i, j int
	if _, err := fmt.Sscanf(ref, "s%d.%d", &i, &j); err != nil || i < 1 || i > len(run.Sources) {
		return ""
	}
	return run.Sources[i-1].AssetID
}

// compose writes, grounds and stores the proposal, and the run is ready.
func (s *Service) compose(ctx context.Context, run *models.BrandDistillRun) error {
	flows := s.flows.Load()
	if flows == nil {
		return ErrUnavailable
	}
	if err := s.charge(ctx, run); err != nil {
		return err
	}
	run.Status = models.BrandDistillStatusComposing
	if err := s.save(ctx, run, "status", "sources"); err != nil {
		return err
	}
	brand, err := s.repos.Brands.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("load the brand: %w", err)
	}
	req := brand_distill.ComposeRequest{
		Candidates: run.Candidates,
		Labels:     labelsOf(run.Candidates),
		Answers:    flowAnswers(run.Answers),
		Digest:     digest(brand),
		Today:      s.now().UTC().Format("2006-01-02"),
	}
	resp, err := flows.Compose(ctx, req)
	if err != nil {
		return s.flowFailure(ctx, run, err)
	}
	proposal := ground(resp.Draft, groundInput{
		candidates: run.Candidates,
		answers:    run.Answers,
		brand:      brand,
		today:      s.now().UTC(),
		depth:      run.Depth,
	})
	if run.ParentRunID != nil {
		if parent, err := s.repos.Runs.Get(ctx, *run.ParentRunID); err == nil {
			carryIDs(parent.Proposal, &proposal)
		}
	}
	run.Proposal = &proposal
	if run.ModelIDs == nil {
		run.ModelIDs = models.StringMap{}
	}
	run.ModelIDs["compose"] = resp.ModelID
	run.Status, run.FailureCode, run.FailureReason = models.BrandDistillStatusReady, "", ""
	if err := s.save(ctx, run, "status", "proposal", "model_ids", "failure_code", "failure_reason"); err != nil {
		return err
	}
	s.supersedeParent(ctx, run)
	droppedTotal.Add("ungrounded", int64(proposal.Dropped.Ungrounded))
	droppedTotal.Add("duplicate", int64(proposal.Dropped.Duplicate))
	droppedTotal.Add("limit", int64(proposal.Dropped.Limit))
	runsTotal.Add("ready", 1)
	if run.ParentRunID == nil {
		tenantID, _ := tenantctx.From(ctx)
		if dec, err := s.limiter.Allow(ctx, tenantID, FeatureMonthly); err == nil {
			s.limiter.DispatchCrossing(ctx, tenantID, dec)
		}
	}
	s.announce(ctx, run)
	return nil
}

// flowFailure settles what a failed model stage means for the run: a final
// failure is stored and nil returned; anything else is returned to retry.
func (s *Service) flowFailure(ctx context.Context, run *models.BrandDistillRun, err error) error {
	if _, ok := errors.AsType[*brand_distill.ValidationError](err); ok {
		s.fail(ctx, run, models.BrandDistillCodeNoSources, noSourcesReason)
		return nil
	}
	if aiErr, ok := errors.AsType[*brand_distill.AIError](err); ok && aiErr.Refused {
		s.fail(ctx, run, models.BrandDistillCodeModelRefused, refusedReason)
		return nil
	}
	if aiErr, ok := errors.AsType[*brand_distill.AIError](err); ok && aiErr.Invalid {
		s.fail(ctx, run, models.BrandDistillCodeInvalidOutput, invalidReason)
		return nil
	}
	if _, ok := errors.AsType[*usage.LimitExceededError](err); ok {
		s.fail(ctx, run, models.BrandDistillCodeLimitReached, limitReasonText)
		return nil
	}
	return err
}

func labelsOf(cs []models.DistillCandidate) map[string]string {
	out := make(map[string]string, len(cs))
	for _, c := range cs {
		out[c.Ref] = c.Label
	}
	return out
}

func flowAnswers(in []models.DistillAnswer) []brand_distill.Answer {
	out := make([]brand_distill.Answer, 0, len(in))
	for _, a := range in {
		out = append(out, brand_distill.Answer{QuestionID: a.QuestionID, Question: a.Question, Answer: a.Answer})
	}
	return out
}

// digest is the workspace's current brand, in brief, every fact included so
// none is proposed twice.
func digest(b *models.BrandData) brand_distill.Digest {
	var d brand_distill.Digest
	for _, v := range b.Voices {
		vd := brand_distill.VoiceDigest{Name: v.Name, WhenToUse: v.WhenToUse}
		if len(v.Samples) > 0 {
			vd.Sample = clampRunes(v.Samples[0], 200)
		}
		d.Voices = append(d.Voices, vd)
	}
	for _, a := range b.Audiences {
		d.Audiences = append(d.Audiences, brand_distill.AudienceDigest{Name: a.Name, Who: a.Who})
	}
	if g := b.Guardrails; g != nil {
		d.MayClaim, d.NeverClaim, d.BannedWords, d.Disclaimer = g.MayClaim, g.NeverClaim, g.BannedWords, g.Disclaimer
	}
	for _, f := range b.Facts {
		d.Facts = append(d.Facts, f.Statement)
	}
	return d
}

func (s *Service) warn(ctx context.Context, what, id string, err error) {
	slog.WarnContext(ctx, "brand distillation: could not "+what, logging.AttrComponent, logComponent, "id", id, logging.AttrError, err)
}
