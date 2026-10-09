package assetanalysis_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/asset_analysis"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/assetanalysis"
)

const tenant = models.DefaultTenantID

type fixture struct {
	db    *bun.DB
	ctx   context.Context
	repos assetanalysis.Repos
	svc   *assetanalysis.Service
	stub  *stubAnalyzer
	jobs  *jobRecorder
}

// stubAnalyzer answers with reply and records each request.
type stubAnalyzer struct {
	mu    sync.Mutex
	reqs  []asset_analysis.AnalyzeRequest
	reply func(req asset_analysis.AnalyzeRequest) (*asset_analysis.AnalyzeResponse, error)
}

func (s *stubAnalyzer) analyze(_ context.Context, req asset_analysis.AnalyzeRequest) (*asset_analysis.AnalyzeResponse, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	return s.reply(req)
}

type queued struct {
	assetID    string
	campaignID string
	reason     string
}

type jobRecorder struct {
	analyses []queued
	rebriefs []string
}

func (j *jobRecorder) EnqueueAnalyzeAsset(_ context.Context, _, assetID string, campaignID *string, reason string) error {
	q := queued{assetID: assetID, reason: reason}
	if campaignID != nil {
		q.campaignID = *campaignID
	}
	j.analyses = append(j.analyses, q)
	return nil
}

func (j *jobRecorder) EnqueueReanalyzeCampaign(_ context.Context, _, campaignID string, _ time.Time) error {
	j.rebriefs = append(j.rebriefs, campaignID)
	return nil
}

func answer(ideas ...string) func(asset_analysis.AnalyzeRequest) (*asset_analysis.AnalyzeResponse, error) {
	return func(req asset_analysis.AnalyzeRequest) (*asset_analysis.AnalyzeResponse, error) {
		a := asset_analysis.Analysis{Language: "en", Summary: "Summary of " + req.Source.Title, KeyPoints: []string{"k"}}
		for _, title := range ideas {
			a.Ideas = append(a.Ideas, asset_analysis.Idea{Title: title, Angle: "angle", Rationale: "why", SourceLabels: []string{"Part 1"}})
		}
		if req.Lens.Campaign != nil {
			a.Relevance = &asset_analysis.Relevance{Score: 70, Why: "fits"}
		}
		return &asset_analysis.AnalyzeResponse{Analysis: a, Strategy: asset_analysis.StrategyFull, SourceTokens: 10, ModelID: "test-model"}, nil
	}
}

func newFixture(t *testing.T, limiter *entitlements.Limiter) *fixture {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	// One connection with FKs off, so assets and campaigns need no users or
	// campaign types.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	repos := assetanalysis.Repos{
		Analyses:  repository.NewAssetAnalysisRepository(db),
		Assets:    repository.NewAssetRepository(db, repository.NewTagRepository(db), repository.NewAssetFileRepository(db)),
		Chunks:    repository.NewAssetChunksRepository(db),
		Campaigns: repository.NewCampaignRepository(db, repository.NewTagRepository(db), repository.NewPlatformRepository(db), repository.NewCampaignTypeRepository(db)),
		Posts:     repository.NewPostRepository(db),
		Ideas:     repository.NewIdeaRepository(db),
		Brands:    repository.NewBrandRepository(db),
		Tenants:   repository.NewTenantRepository(db),
	}
	f := &fixture{
		db:    db,
		ctx:   tenantctx.With(context.Background(), tenant),
		repos: repos,
		svc:   assetanalysis.New(db, repos, limiter, nil),
		stub:  &stubAnalyzer{reply: answer("Idea one", "Idea two")},
		jobs:  &jobRecorder{},
	}
	f.svc.SetAnalyzer(f.stub.analyze)
	f.svc.SetJobs(f.jobs, time.Minute)
	return f
}

func (f *fixture) seedAsset(t *testing.T, id, assetType, status, content string) {
	t.Helper()
	now := time.Now().UTC()
	a := &models.Asset{
		ID: id, Title: "Asset " + id, Content: content, Status: status, Type: new(assetType),
		TagIDs: models.StringSlice{}, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := f.db.NewInsert().Model(a).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) seedCampaign(t *testing.T, id string, assetIDs ...string) {
	t.Helper()
	now := time.Now().UTC()
	c := &models.Campaign{
		ID: id, Name: "Campaign " + id, Description: "d", TargetPersona: "p", KeyMessages: "first message",
		ToneGuidelines: "t", AssetIDs: models.StringSlice(assetIDs), TargetPlatforms: models.CampaignPlatforms{},
		CampaignTypeID: "ctype", Language: "en", PublishingTime: "09:00", PublishingDays: models.StringSlice{},
		GoalCadence: "month", Status: models.StatusActive, Currency: "USD", TagIDs: models.StringSlice{},
		CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}
	if err := f.repos.Campaigns.Create(f.ctx, c); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) lens(t *testing.T, assetID string, campaignID *string) *models.AssetAnalysis {
	t.Helper()
	a, err := f.repos.Analyses.GetLens(f.ctx, assetID, campaignID)
	if err != nil {
		t.Fatalf("lens %s/%v: %v", assetID, campaignID, err)
	}
	return a
}

func (f *fixture) ideaTitles(t *testing.T, assetID string) []string {
	t.Helper()
	ideas, err := f.repos.Ideas.List(f.ctx, repository.IdeaListFilter{AssetID: assetID})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range ideas {
		out = append(out, i.Title)
	}
	return out
}

func run(t *testing.T, f *fixture, assetID string, campaignID *string) {
	t.Helper()
	if err := f.svc.Run(f.ctx, assetanalysis.Lens{TenantID: tenant, AssetID: assetID, CampaignID: campaignID}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRunWorkspaceLensStoresAnalysisAndIdeas(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "First paragraph.\n\nSecond paragraph.")

	run(t, f, "md-1", nil)

	a := f.lens(t, "md-1", nil)
	if a.Status != models.AssetAnalysisStatusReady || a.Summary != "Summary of Asset md-1" || a.ModelID != "test-model" ||
		a.Strategy == nil || *a.Strategy != models.AssetAnalysisStrategyFull || a.AnalyzedAt == nil {
		t.Fatalf("lens = %+v", a)
	}
	req := f.stub.reqs[0]
	if len(req.Source.Segments) != 1 || req.Source.Segments[0].Label != "Part 1" || req.Lens.Campaign != nil {
		t.Fatalf("request = %+v", req)
	}
	ideas, err := f.repos.Ideas.List(f.ctx, repository.IdeaListFilter{AssetID: "md-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ideas) != 2 || ideas[0].CampaignID != nil || ideas[0].CreatedByName != "Ogen" ||
		ideas[0].Note != "angle\n\nwhy\n\nSources: Part 1" || ideas[0].Verdict != nil {
		t.Fatalf("ideas = %+v", ideas)
	}
	runs, err := f.repos.Analyses.CountRunsSince(f.ctx, time.Now().Add(-time.Hour))
	if err != nil || runs != 1 {
		t.Fatalf("runs = %d, %v", runs, err)
	}

	run(t, f, "md-1", nil)
	if len(f.stub.reqs) != 1 {
		t.Fatalf("an unchanged re-run called the model again (%d calls)", len(f.stub.reqs))
	}
}

func TestRunCampaignLensRerunsOnBriefChange(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "doc-1", models.AssetTypeDocument, models.AssetStatusReady, "")
	f.seedAsset(t, "doc-2", models.AssetTypeDocument, models.AssetStatusReady, "")
	if err := f.repos.Chunks.UpsertChunks(f.ctx, "doc-1", []models.AssetChunk{
		{ID: "c1", AssetID: "doc-1", ChunkIndex: 0, Content: "Slide text", TokenCount: 3, SourceLabel: new("Slide 1"),
			Embedding: pgvector.NewHalfVector(make([]float32, 3072))},
	}); err != nil {
		t.Fatal(err)
	}
	f.seedCampaign(t, "camp-1", "doc-1", "doc-2")
	camp := "camp-1"

	run(t, f, "doc-1", &camp)

	req := f.stub.reqs[0]
	if req.Lens.Campaign == nil || req.Lens.Campaign.KeyMessages != "first message" || req.Source.Segments[0].Label != "Slide 1" {
		t.Fatalf("campaign request = %+v", req)
	}
	a := f.lens(t, "doc-1", &camp)
	if a.Result.Relevance == nil || a.Result.Relevance.Score != 70 || a.CampaignName == nil {
		t.Fatalf("campaign lens = %+v", a)
	}
	ideas, _ := f.repos.Ideas.List(f.ctx, repository.IdeaListFilter{CampaignID: camp})
	if len(ideas) != 2 {
		t.Fatalf("campaign ideas = %d", len(ideas))
	}
	now := time.Now().UTC()
	ideas[0].Verdict, ideas[0].DecidedAt = new(models.IdeaVerdictYes), &now
	if _, err := f.repos.Ideas.Update(f.ctx, &ideas[0], "verdict", "decided_at"); err != nil {
		t.Fatal(err)
	}

	if _, err := f.db.NewUpdate().Model((*models.Campaign)(nil)).Set("key_messages = 'new message'").Where("id = ?", camp).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.stub.reply = answer("Idea three")
	run(t, f, "doc-1", &camp)

	if len(f.stub.reqs) != 2 {
		t.Fatalf("a brief change did not re-run the lens")
	}
	got := f.ideaTitles(t, "doc-1")
	slices.Sort(got)
	if !slices.Equal(got, []string{"Idea one", "Idea three"}) {
		t.Fatalf("ideas after re-run = %v, want the triaged one kept and the new one", got)
	}
	if existing := f.stub.reqs[1].Lens.ExistingIdeas; !slices.Contains(existing, "Idea one") || slices.Contains(existing, "Idea two") {
		t.Fatalf("existing ideas = %v, want the triaged one only", existing)
	}
}

func TestRunSkipsGoneOrIneligibleTargets(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "img-1", models.AssetTypeImage, models.AssetStatusReady, "a photo")
	f.seedAsset(t, "pdf-1", models.AssetTypePDF, models.AssetStatusProcessing, "")
	f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
	f.seedCampaign(t, "camp-1")
	camp := "camp-1"

	run(t, f, "img-1", nil)
	run(t, f, "pdf-1", nil)
	run(t, f, "missing", nil)
	run(t, f, "md-1", &camp) // not attached
	if len(f.stub.reqs) != 0 {
		t.Fatalf("model called %d times for nothing analysable", len(f.stub.reqs))
	}
}

func TestRunStoresAnEmptyAnalysisForSilentSources(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "audio-1", models.AssetTypeAudio, models.AssetStatusPartial, "")

	run(t, f, "audio-1", nil)

	a := f.lens(t, "audio-1", nil)
	if a.Status != models.AssetAnalysisStatusReady || a.Summary != "" || a.Strategy != nil || len(f.stub.reqs) != 0 {
		t.Fatalf("lens = %+v, calls = %d", a, len(f.stub.reqs))
	}
}

func TestRunSettlesFlowFailures(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		status  models.AssetAnalysisStatus
		code    string
		wantErr bool
	}{
		{"empty source", &asset_analysis.ValidationError{Msg: "empty"}, models.AssetAnalysisStatusFailed, models.AnalysisCodeEmptySource, false},
		{"invalid output", &asset_analysis.AIError{Msg: "bad", Invalid: true}, models.AssetAnalysisStatusFailed, models.AnalysisCodeInvalidOutput, false},
		{"transient", &asset_analysis.AIError{Msg: "503"}, models.AssetAnalysisStatusPending, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
			f.stub.reply = func(asset_analysis.AnalyzeRequest) (*asset_analysis.AnalyzeResponse, error) { return nil, tc.err }

			err := f.svc.Run(f.ctx, assetanalysis.Lens{TenantID: tenant, AssetID: "md-1"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v", err, tc.wantErr)
			}
			a := f.lens(t, "md-1", nil)
			if a.Status != tc.status || a.FailureCode != tc.code {
				t.Fatalf("status = %s/%s, want %s/%s", a.Status, a.FailureCode, tc.status, tc.code)
			}
			if tc.wantErr {
				f.svc.Fail(f.ctx, assetanalysis.Lens{TenantID: tenant, AssetID: "md-1"}, err)
				if a := f.lens(t, "md-1", nil); a.Status != models.AssetAnalysisStatusFailed || a.FailureCode != models.AnalysisCodeModelUnavailable {
					t.Fatalf("after Fail = %s/%s", a.Status, a.FailureCode)
				}
			}
		})
	}
}

// gateResolver serves a fixed set of entitlement values.
type gateResolver struct {
	values []entitlements.EntitlementValue
}

func (g gateResolver) ResolveCurrent(context.Context, string) (*entitlements.Resolution, error) {
	return &entitlements.Resolution{Entitlements: g.values}, nil
}

func entitlement(key string, value any) entitlements.EntitlementValue {
	return entitlements.EntitlementValue{Feature: entitlements.Feature{Key: key}, Value: value}
}

func TestRunHonoursTheTierGates(t *testing.T) {
	cat, err := entitlements.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("feature off", func(t *testing.T) {
		limiter := entitlements.NewLimiter(gateResolver{[]entitlements.EntitlementValue{entitlement(assetanalysis.FeatureAnalysis, false)}}, cat, entitlements.ModeEnforce)
		f := newFixture(t, limiter)
		f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
		run(t, f, "md-1", nil)
		if a := f.lens(t, "md-1", nil); a.Status != models.AssetAnalysisStatusSkipped || a.FailureCode != models.AnalysisCodeEntitlementDisabled || len(f.stub.reqs) != 0 {
			t.Fatalf("lens = %s/%s calls=%d", a.Status, a.FailureCode, len(f.stub.reqs))
		}
	})
	t.Run("monthly cap reached", func(t *testing.T) {
		limiter := entitlements.NewLimiter(gateResolver{[]entitlements.EntitlementValue{entitlement(assetanalysis.FeatureMonthlyRuns, float64(1))}}, cat, entitlements.ModeEnforce)
		f := newFixture(t, limiter)
		limiter.Register(assetanalysis.FeatureMonthlyRuns, entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) {
			return f.repos.Analyses.CountRunsSince(ctx, time.Now().Add(-time.Hour))
		}))
		f.seedAsset(t, "md-1", models.AssetTypeMarkdown, models.AssetStatusReady, "text")
		f.seedAsset(t, "md-2", models.AssetTypeMarkdown, models.AssetStatusReady, "more text")
		run(t, f, "md-1", nil)
		run(t, f, "md-2", nil)
		if a := f.lens(t, "md-2", nil); a.Status != models.AssetAnalysisStatusSkipped || a.FailureCode != models.AnalysisCodeLimitReached || len(f.stub.reqs) != 1 {
			t.Fatalf("second lens = %s/%s calls=%d", a.Status, a.FailureCode, len(f.stub.reqs))
		}
		if err := f.svc.Retry(f.ctx, tenant, "md-2", nil); err != nil {
			t.Fatalf("retry a skipped lens: %v", err)
		}
		if err := f.svc.Retry(f.ctx, tenant, "md-1", nil); !errors.Is(err, assetanalysis.ErrNotRetryable) {
			t.Fatalf("retry a ready lens: %v", err)
		}
	})
}

func TestSchedulingQueuesEveryLens(t *testing.T) {
	f := newFixture(t, nil)
	f.seedAsset(t, "pdf-1", models.AssetTypePDF, models.AssetStatusReady, "")
	f.seedAsset(t, "pdf-2", models.AssetTypePDF, models.AssetStatusProcessing, "")
	f.seedCampaign(t, "camp-a", "pdf-1")
	f.seedCampaign(t, "camp-b", "pdf-1")

	f.svc.ScheduleAsset(f.ctx, tenant, "pdf-1")
	if len(f.jobs.analyses) != 3 {
		t.Fatalf("queued = %+v, want the workspace lens and two campaign lenses", f.jobs.analyses)
	}
	if all, _ := f.repos.Analyses.ListByAsset(f.ctx, "pdf-1"); len(all) != 3 || all[0].Status != models.AssetAnalysisStatusPending {
		t.Fatalf("pending lenses = %+v", all)
	}

	f.jobs.analyses = nil
	f.svc.ScheduleAttached(f.ctx, tenant, "camp-a", []string{"pdf-1", "pdf-2"})
	if len(f.jobs.analyses) != 1 || f.jobs.analyses[0] != (queued{"pdf-1", "camp-a", assetanalysis.ReasonAttached}) {
		t.Fatalf("attach queued = %+v, want only the ready asset", f.jobs.analyses)
	}

	f.svc.Detach(f.ctx, tenant, "camp-a", []string{"pdf-1"})
	camp := "camp-a"
	if _, err := f.repos.Analyses.GetLens(f.ctx, "pdf-1", &camp); err == nil {
		t.Fatal("detached lens survived")
	}

	f.svc.ScheduleRebrief(f.ctx, tenant, "camp-b")
	f.jobs.analyses = nil
	if err := f.svc.Rebrief(f.ctx, tenant, "camp-b"); err != nil {
		t.Fatal(err)
	}
	if len(f.jobs.rebriefs) != 1 || len(f.jobs.analyses) != 1 || f.jobs.analyses[0].reason != assetanalysis.ReasonBrief {
		t.Fatalf("rebrief = %v, queued = %+v", f.jobs.rebriefs, f.jobs.analyses)
	}
}
