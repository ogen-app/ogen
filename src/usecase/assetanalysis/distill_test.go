package assetanalysis_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/ideafit"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

// stubDistiller answers with reply and records each request.
type stubDistiller struct {
	mu    sync.Mutex
	reqs  []asset_analysis.DistillRequest
	reply func(req asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error)
}

func (s *stubDistiller) distill(_ context.Context, req asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.reply(req)
}

func distilled(titles ...string) func(asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error) {
	return func(asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error) {
		resp := &asset_analysis.DistillResponse{Strategy: asset_analysis.StrategyFull, ModelID: "distill-model", Dropped: 1}
		for _, title := range titles {
			resp.Ideas = append(resp.Ideas, asset_analysis.Idea{
				Title: title, Angle: "new angle", Rationale: "fits", SourceLabels: []string{"Part 1"},
				Fit: rated(ideafit.LensWorkspace, 4),
			})
		}
		return resp, nil
	}
}

func withDistiller(f *fixture, reply func(asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error)) *stubDistiller {
	d := &stubDistiller{reply: reply}
	f.svc.SetDistiller(d.distill)
	return d
}

func (f *fixture) setVerdict(t *testing.T, assetID, title string, v models.IdeaVerdict) {
	t.Helper()
	ideas, err := f.repos.Ideas.List(f.ctx, repository.IdeaListFilter{AssetID: assetID})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(ideas, func(idea models.Idea) bool { return idea.Title == title })
	if i < 0 {
		t.Fatalf("no idea %q", title)
	}
	now := time.Now().UTC()
	ideas[i].Verdict, ideas[i].DecidedAt = &v, &now
	if _, err := f.repos.Ideas.Update(f.ctx, &ideas[i], "verdict", "decided_at"); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) requestDistill(t *testing.T, assetID string, campaignID *string, focus string, count int) *models.IdeaDistillRun {
	t.Helper()
	r, err := f.svc.RequestDistill(f.ctx, assetanalysis.DistillInput{
		TenantID: tenant, AssetID: assetID, CampaignID: campaignID, Focus: focus, Count: count, UserID: new("user-1"),
	})
	if err != nil {
		t.Fatalf("request distill: %v", err)
	}
	return r
}

func (f *fixture) distillRun(t *testing.T, id string) *models.IdeaDistillRun {
	t.Helper()
	r, err := f.repos.Distills.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDistillAddsIdeasThatSurviveARerun(t *testing.T) {
	f := newFixture(t, nil)
	d := withDistiller(f, distilled("More A", "More B"))
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "First paragraph.")
	run(t, f, "md-1", nil)
	f.setVerdict(t, "md-1", "Idea one", models.IdeaVerdictNo)

	r := f.requestDistill(t, "md-1", nil, "  pricing  ", 3)
	if r.Status != models.IdeaDistillStatusPending || r.Focus != "pricing" || r.Count != 3 || !slices.Equal(f.jobs.distills, []string{r.ID}) {
		t.Fatalf("run = %+v, queued %v", r, f.jobs.distills)
	}
	if err := f.svc.Distill(f.ctx, tenant, r.ID); err != nil {
		t.Fatal(err)
	}

	req := d.reqs[0]
	if req.Focus != "pricing" || req.Count != 3 || req.Summary != "Summary of Asset md-1" || req.Lens.Campaign != nil {
		t.Fatalf("request = %+v", req)
	}
	verdicts := map[string]string{}
	for _, p := range req.Prior {
		verdicts[p.Title] = p.Verdict
	}
	if verdicts["Idea one"] != asset_analysis.PriorRejected || verdicts["Idea two"] != asset_analysis.PriorUndecided {
		t.Fatalf("prior = %+v", req.Prior)
	}
	if !slices.Contains(req.KnownTitles, "Idea one") || !slices.Contains(req.KnownTitles, "Idea two") {
		t.Fatalf("known titles = %v", req.KnownTitles)
	}

	got := f.distillRun(t, r.ID)
	if got.Status != models.IdeaDistillStatusReady || got.IdeasCount != 2 || got.ModelID != "distill-model" || got.FinishedAt == nil {
		t.Fatalf("saved run = %+v", got)
	}
	ideas := scoredIdeas(t, f, "md-1")
	more := ideas["More A"]
	if more.DistillRunID == nil || *more.DistillRunID != r.ID || more.AssetAnalysisID == nil || *more.AssetAnalysisID != f.lens(t, "md-1", nil).ID ||
		more.FitScore == nil || more.Origin != models.IdeaOriginAssetAnalysis || more.CampaignID != nil || more.CreatedByName != "Ogen" {
		t.Fatalf("distilled idea = %+v", more)
	}
	if n, _ := f.repos.Distills.CountChargesSince(f.ctx, time.Now().Add(-time.Hour)); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}

	// An edit re-runs the analysis: its own untriaged idea is replaced, the
	// distilled ones stay and are listed as captured.
	if _, err := f.db.NewUpdate().Model((*models.Asset)(nil)).Set("content = 'Rewritten.'").Where("id = 'md-1'").Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.stub.reply = answer("Idea three")
	run(t, f, "md-1", nil)
	titles := f.ideaTitles(t, "md-1")
	slices.Sort(titles)
	if !slices.Equal(titles, []string{"Idea one", "Idea three", "More A", "More B"}) {
		t.Fatalf("ideas after re-run = %v", titles)
	}
	if existing := f.stub.reqs[1].Lens.ExistingIdeas; !slices.Contains(existing, "More A") || slices.Contains(existing, "Idea two") {
		t.Fatalf("existing ideas = %v, want the distilled ones and not the replaced one", existing)
	}
}

func TestRequestDistillRefusals(t *testing.T) {
	f := newFixture(t, nil)
	withDistiller(f, distilled("x"))
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	f.seedAsset(t, "md-2", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	f.seedCampaign(t, "camp-1", "md-2")
	camp := "camp-1"
	in := func(assetID string, campaignID *string, focus string, count int) assetanalysis.DistillInput {
		return assetanalysis.DistillInput{TenantID: tenant, AssetID: assetID, CampaignID: campaignID, Focus: focus, Count: count}
	}

	if _, err := f.svc.RequestDistill(f.ctx, in("md-1", nil, "", 0)); !errors.Is(err, assetanalysis.ErrAnalysisNotReady) {
		t.Fatalf("no lens yet: err = %v", err)
	}
	if _, err := f.svc.RequestDistill(f.ctx, in("md-1", &camp, "", 0)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("campaign without the asset: err = %v", err)
	}
	run(t, f, "md-1", nil)
	for _, bad := range []assetanalysis.DistillInput{in("md-1", nil, "", 2), in("md-1", nil, "", 11), in("md-1", nil, strings.Repeat("é", 301), 5)} {
		if _, err := f.svc.RequestDistill(f.ctx, bad); !isInputError(err) {
			t.Fatalf("count %d focus %d runes: err = %v, want an input error", bad.Count, len([]rune(bad.Focus)), err)
		}
	}

	first, err := f.svc.RequestDistill(f.ctx, in("md-1", nil, strings.Repeat("é", 300), 0))
	if err != nil || first.Count != models.IdeaDistillDefaultCount {
		t.Fatalf("first request = %+v, %v", first, err)
	}
	active, err := f.svc.RequestDistill(f.ctx, in("md-1", nil, "", 5))
	if !errors.Is(err, assetanalysis.ErrDistillInProgress) || active == nil || active.ID != first.ID {
		t.Fatalf("second request = %+v, %v; want the active run", active, err)
	}

	runs, err := f.svc.ListDistills(f.ctx, tenant, "md-1", nil)
	if err != nil || len(runs) != 1 || runs[0].ID != first.ID {
		t.Fatalf("list = %+v, %v", runs, err)
	}
	if runs, err := f.svc.ListDistills(f.ctx, tenant, "md-2", nil); err != nil || len(runs) != 0 {
		t.Fatalf("list of a lens never analysed = %+v, %v", runs, err)
	}
}

func isInputError(err error) bool {
	_, ok := errors.AsType[*assetanalysis.DistillInputError](err)
	return ok
}

func TestDistillHonoursItsOwnCap(t *testing.T) {
	cat, err := entitlements.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("feature off", func(t *testing.T) {
		f := newFixture(t, nil)
		f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
		run(t, f, "md-1", nil)
		limiter := entitlements.NewLimiter(gateResolver{[]entitlements.EntitlementValue{entitlement(assetanalysis.FeatureAnalysis, false)}}, cat, entitlements.ModeEnforce)
		f.svc = assetanalysis.New(f.db, f.repos, limiter, nil)
		f.svc.SetJobs(f.jobs, time.Minute, editDebounce)
		withDistiller(f, distilled("x"))
		_, err := f.svc.RequestDistill(f.ctx, assetanalysis.DistillInput{TenantID: tenant, AssetID: "md-1"})
		if _, ok := errors.AsType[*entitlements.FeatureNotAvailableError](err); !ok {
			t.Fatalf("err = %v, want FeatureNotAvailableError", err)
		}
	})
	t.Run("monthly cap", func(t *testing.T) {
		limiter := entitlements.NewLimiter(gateResolver{[]entitlements.EntitlementValue{
			entitlement(assetanalysis.FeatureMonthlyRuns, float64(1)),
			entitlement(assetanalysis.FeatureMonthlyDistills, float64(1)),
		}}, cat, entitlements.ModeEnforce)
		f := newFixture(t, limiter)
		limiter.Register(assetanalysis.FeatureMonthlyRuns, entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) {
			return f.repos.Analyses.CountRunsSince(ctx, time.Now().Add(-time.Hour))
		}))
		limiter.Register(assetanalysis.FeatureMonthlyDistills, entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) {
			return f.repos.Distills.CountChargesSince(ctx, time.Now().Add(-time.Hour))
		}))
		d := withDistiller(f, distilled("More"))
		f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
		run(t, f, "md-1", nil)

		r := f.requestDistill(t, "md-1", nil, "", 3)
		if err := f.svc.Distill(f.ctx, tenant, r.ID); err != nil {
			t.Fatal(err)
		}
		if n, _ := f.repos.Analyses.CountRunsSince(f.ctx, time.Now().Add(-time.Hour)); n != 1 {
			t.Fatalf("analysis runs = %d: a distill used the analysis allowance", n)
		}
		_, err := f.svc.RequestDistill(f.ctx, assetanalysis.DistillInput{TenantID: tenant, AssetID: "md-1"})
		if qe, ok := errors.AsType[*entitlements.QuotaExceededError](err); !ok || qe.Key != assetanalysis.FeatureMonthlyDistills {
			t.Fatalf("err = %v, want the distill cap", err)
		}

		// A run queued before the cap filled is skipped when it starts.
		late := &models.IdeaDistillRun{AnalysisID: f.lens(t, "md-1", nil).ID, AssetID: "md-1", Count: 3}
		if err := f.repos.Distills.Create(f.ctx, late); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Distill(f.ctx, tenant, late.ID); err != nil {
			t.Fatal(err)
		}
		if got := f.distillRun(t, late.ID); got.Status != models.IdeaDistillStatusSkipped || got.FailureCode != models.AnalysisCodeLimitReached || len(d.reqs) != 1 {
			t.Fatalf("late run = %s/%s, calls %d", got.Status, got.FailureCode, len(d.reqs))
		}
	})
}

func TestDistillSettlesFailures(t *testing.T) {
	f := newFixture(t, nil)
	d := withDistiller(f, nil)
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	run(t, f, "md-1", nil)
	charges := func() int64 {
		n, _ := f.repos.Distills.CountChargesSince(f.ctx, time.Now().Add(-time.Hour))
		return n
	}

	d.reply = func(asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error) {
		return nil, &asset_analysis.AIError{Msg: "bad json", Invalid: true}
	}
	r := f.requestDistill(t, "md-1", nil, "", 3)
	if err := f.svc.Distill(f.ctx, tenant, r.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.distillRun(t, r.ID); got.Status != models.IdeaDistillStatusFailed || got.FailureCode != models.AnalysisCodeInvalidOutput || charges() != 1 {
		t.Fatalf("invalid output: run %s/%s, charges %d", got.Status, got.FailureCode, charges())
	}

	d.reply = func(asset_analysis.DistillRequest) (*asset_analysis.DistillResponse, error) {
		return nil, errors.New("overloaded")
	}
	r = f.requestDistill(t, "md-1", nil, "", 3)
	if err := f.svc.Distill(f.ctx, tenant, r.ID); err == nil {
		t.Fatal("a transient failure was not returned for a retry")
	}
	if got := f.distillRun(t, r.ID); got.Status != models.IdeaDistillStatusPending || charges() != 1 {
		t.Fatalf("transient failure: run %s, charges %d; want pending and the charge released", got.Status, charges())
	}
	f.svc.FailDistill(f.ctx, tenant, r.ID, errors.New("overloaded"))
	if got := f.distillRun(t, r.ID); got.Status != models.IdeaDistillStatusFailed || got.FailureCode != models.AnalysisCodeModelUnavailable {
		t.Fatalf("after the last attempt: %s/%s", got.Status, got.FailureCode)
	}
	if err := f.svc.Distill(f.ctx, tenant, r.ID); err != nil || len(d.reqs) != 2 {
		t.Fatalf("a settled run ran again: %v, calls %d", err, len(d.reqs))
	}
}

func TestDistillOfADetachedAssetIsDropped(t *testing.T) {
	f := newFixture(t, nil)
	d := withDistiller(f, distilled("More"))
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	f.seedCampaign(t, "camp-1", "md-1")
	camp := "camp-1"
	run(t, f, "md-1", &camp)
	r := f.requestDistill(t, "md-1", &camp, "", 3)

	if _, err := f.db.NewUpdate().Model((*models.Campaign)(nil)).Set("asset_ids = '[]'::jsonb").Where("id = ?", camp).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Distill(f.ctx, tenant, r.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.distillRun(t, r.ID); got.Status != models.IdeaDistillStatusFailed || got.FailureCode != models.AnalysisCodeLensGone || len(d.reqs) != 0 {
		t.Fatalf("run = %s/%s, calls %d", got.Status, got.FailureCode, len(d.reqs))
	}
}

// hubRecorder keeps every published event.
type hubRecorder struct {
	eventhub.Hub
	mu     sync.Mutex
	events []eventhub.Event
}

func (h *hubRecorder) Publish(_ context.Context, ev eventhub.Event) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
	return nil
}

func TestDistillPublishesEachStatusToItsTopics(t *testing.T) {
	f := newFixture(t, nil)
	hub := &hubRecorder{}
	f.svc = assetanalysis.New(f.db, f.repos, nil, hub)
	f.svc.SetAnalyzer(f.stub.analyze)
	f.svc.SetJobs(f.jobs, time.Minute, editDebounce)
	withDistiller(f, distilled("More A", "More B"))
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	f.seedCampaign(t, "camp-1", "md-1")
	camp := "camp-1"
	run(t, f, "md-1", &camp)
	hub.events = nil

	r := f.requestDistill(t, "md-1", &camp, "", 3)
	if err := f.svc.Distill(f.ctx, tenant, r.ID); err != nil {
		t.Fatal(err)
	}

	var seen []string
	for _, ev := range hub.events {
		p, ok := ev.Payload.(assetanalysis.DistillEventPayload)
		if !ok || ev.Type != "asset.ideas.distill.updated" || p.RunID != r.ID || ev.TenantID != tenant {
			t.Fatalf("event = %+v", ev)
		}
		seen = append(seen, ev.Topic+" "+string(p.Status))
		if p.Status == models.IdeaDistillStatusReady && p.IdeasCount != 2 {
			t.Fatalf("ready event ideas_count = %d", p.IdeasCount)
		}
	}
	want := []string{
		"entity:asset:md-1 pending", "entity:campaign:camp-1 pending",
		"entity:asset:md-1 running", "entity:campaign:camp-1 running",
		"entity:asset:md-1 ready", "entity:campaign:camp-1 ready",
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("events = %v, want %v", seen, want)
	}
}
