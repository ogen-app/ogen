package branddistill_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
	"github.com/ogen-app/ogen/src/usecase/brandsource"
	"github.com/ogen-app/ogen/src/usecase/urlimport"
)

const tenant = models.DefaultTenantID

type fixture struct {
	db       *bun.DB
	ctx      context.Context
	repos    branddistill.Repos
	svc      *branddistill.Service
	jobs     *jobRecorder
	mapper   *fakeMapper
	importer *fakeImporter
	flows    *stubFlows
	now      time.Time
}

type jobRecorder struct{ runs []string }

func (j *jobRecorder) EnqueueDistillBrand(_ context.Context, _, runID string) error {
	j.runs = append(j.runs, runID)
	return nil
}

type fakeMapper struct {
	links []firecrawl.Link
	err   error
}

func (m *fakeMapper) Map(context.Context, firecrawl.MapRequest) ([]firecrawl.Link, error) {
	return m.links, m.err
}

// fakeImporter stores each page as a URL asset, ingested at once unless
// status says otherwise.
type fakeImporter struct {
	f       *fixture
	status  string
	content map[string]string
	pages   []string
}

func (i *fakeImporter) Configured(context.Context) bool { return true }

func (i *fakeImporter) Import(ctx context.Context, in urlimport.Input) (*urlimport.Result, error) {
	i.pages = append(i.pages, in.URL)
	id, _ := models.NewID()
	body, ok := i.content[in.URL]
	if !ok {
		body = strings.Repeat("We roast light because it is the only way to taste where a coffee is from. ", 30)
	}
	now := time.Now().UTC()
	a := &models.Asset{
		ID: id, Title: in.URL, Content: body, Status: i.statusOr(), Type: new(models.AssetTypeURL), SourceURL: new(in.URL),
		TagIDs: models.StringSlice{}, CreatedBy: in.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if in.Mark != nil {
		in.Mark.Apply(a)
	}
	if _, err := i.f.db.NewInsert().Model(a).Exec(ctx); err != nil {
		return nil, err
	}
	return &urlimport.Result{Asset: a, Created: true}, nil
}

func (i *fakeImporter) statusOr() string {
	if i.status != "" {
		return i.status
	}
	return models.AssetStatusReady
}

// stubFlows quotes each segment's first sentence as a voice passage and
// composes a voice from them, plus one sample nothing supports.
type stubFlows struct {
	mu       sync.Mutex
	extracts int
	composes []brand_distill.ComposeRequest
	// composeErr fails the compose call; during runs before it answers.
	composeErr error
	during     func()
}

func (s *stubFlows) extract(_ context.Context, req brand_distill.ExtractRequest) (*brand_distill.ExtractResponse, error) {
	s.mu.Lock()
	s.extracts++
	s.mu.Unlock()
	resp := &brand_distill.ExtractResponse{ModelID: "extract-model", Parts: 1}
	for _, seg := range req.Segments {
		first, _, _ := strings.Cut(seg.Text, ".")
		resp.Candidates = append(resp.Candidates, models.DistillCandidate{Kind: brand_distill.KindVoice, Ref: seg.Ref, Quote: strings.TrimSpace(first) + "."})
	}
	return resp, nil
}

func (s *stubFlows) compose(_ context.Context, req brand_distill.ComposeRequest) (*brand_distill.ComposeResponse, error) {
	s.mu.Lock()
	s.composes = append(s.composes, req)
	s.mu.Unlock()
	if s.during != nil {
		s.during()
	}
	if s.composeErr != nil {
		return nil, s.composeErr
	}
	d := brand_distill.Draft{Coverage: map[string]models.DistillCoverage{"voices": {Level: "found"}}}
	if len(req.Candidates) > 0 {
		c := req.Candidates[0]
		d.Voices = []brand_distill.DraftVoice{{
			Name: "Warm", IsDefault: true,
			Samples: []brand_distill.DraftSample{{Ref: c.Ref, Text: c.Quote}, {Ref: c.Ref, Text: "We are simply the best."}},
		}}
		d.Facts = []brand_distill.DraftFact{{Statement: "We roast light.", Evidence: []brand_distill.DraftEvidence{{Ref: c.Ref, Quote: "We roast light"}}}}
	}
	for _, a := range req.Answers {
		d.Audiences = append(d.Audiences, brand_distill.DraftAudience{Name: "From " + a.QuestionID, Who: a.Answer, Evidence: []brand_distill.DraftEvidence{{QuestionID: a.QuestionID}}})
	}
	d.Questions = []brand_distill.DraftQuestion{{Text: "Who are you not for?", Section: "audiences"}}
	return &brand_distill.ComposeResponse{Draft: d, ModelID: "compose-model"}, nil
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("SET session_replication_role = replica"); err != nil {
		t.Fatal(err)
	}
	tags := repository.NewTagRepository(db)
	f := &fixture{
		db:  db,
		ctx: tenantctx.With(context.Background(), tenant),
		repos: branddistill.Repos{
			Runs:   repository.NewBrandDistillRepository(db),
			Assets: repository.NewAssetRepository(db, tags, repository.NewAssetFileRepository(db)),
			Chunks: repository.NewAssetChunksRepository(db),
			Brands: repository.NewBrandRepository(db),
		},
		jobs:   &jobRecorder{},
		mapper: &fakeMapper{},
		flows:  &stubFlows{},
		now:    time.Now(),
	}
	f.importer = &fakeImporter{f: f}
	f.svc = branddistill.New(db, f.repos, nil, nil, f.jobs, branddistill.Deps{
		Mapper: f.mapper, Importer: f.importer, Marker: &brandsource.Marker{Tags: tags},
	})
	f.svc.SetFlows(&branddistill.Flows{Extract: f.flows.extract, Compose: f.flows.compose})
	f.svc.SetClock(func() time.Time { return f.now })
	return f
}

func (f *fixture) run(t *testing.T, id string) *models.BrandDistillRun {
	t.Helper()
	run, err := f.repos.Runs.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *fixture) seedAsset(t *testing.T, id, status, content string) {
	t.Helper()
	now := time.Now().UTC()
	a := &models.Asset{
		ID: id, Title: "Brand book", Content: content, Status: status, Type: new(models.AssetTypeMarkdown),
		TagIDs: models.StringSlice{}, CreatedBy: "user-1", CreatedAt: now, UpdatedAt: now,
	}
	if _, err := f.db.NewInsert().Model(a).Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) charges(t *testing.T) int64 {
	t.Helper()
	n, err := f.repos.Runs.CountChargesSince(f.ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWebsiteRunProposesAGroundedBrand(t *testing.T) {
	f := newFixture(t)
	f.mapper.links = []firecrawl.Link{{URL: "https://acme.com/about"}, {URL: "https://acme.com/cart"}, {URL: "https://acme.com/pricing"}}
	f.importer.content = map[string]string{"https://acme.com/pricing": "Prices."}

	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, UserID: new("user-1"), WebsiteURL: "https://acme.com"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.BrandDistillStatusPending || len(f.jobs.runs) != 1 {
		t.Fatalf("run = %s, jobs = %v", run.Status, f.jobs.runs)
	}
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}

	got := f.run(t, run.ID)
	if got.Status != models.BrandDistillStatusReady || got.Proposal == nil {
		t.Fatalf("run = %s %s", got.Status, got.FailureReason)
	}
	if want := []string{"https://acme.com/", "https://acme.com/about", "https://acme.com/pricing"}; strings.Join(f.importer.pages, " ") != strings.Join(want, " ") {
		t.Fatalf("imported %v, want %v", f.importer.pages, want)
	}
	statuses := map[string]string{}
	for _, s := range got.Sources {
		statuses[s.URL] = s.Status
		if s.Role != models.DistillRoleWebsite || s.AssetID == "" {
			t.Fatalf("source = %+v", s)
		}
	}
	if statuses["https://acme.com/pricing"] != models.DistillSourceThin || statuses["https://acme.com/about"] != models.DistillSourceReady {
		t.Fatalf("source statuses = %v", statuses)
	}
	if got.Website == nil || !got.Website.Mapped || got.Website.PagesFound != 3 {
		t.Fatalf("website = %+v", got.Website)
	}
	page, err := f.repos.Assets.GetByID(f.ctx, got.Sources[0].AssetID)
	if err != nil || page.Origin != models.AssetOriginBrandDistill || page.OriginRef.RunID != run.ID || len(page.TagIDs) != 1 {
		t.Fatalf("imported page = %+v %+v %v", page.Origin, page.OriginRef, err)
	}

	p := got.Proposal
	if len(p.Voices) != 1 || len(p.Voices[0].Samples) != 1 || !p.Voices[0].Thin || p.Dropped.Ungrounded != 1 {
		t.Fatalf("voices = %+v, dropped = %+v", p.Voices, p.Dropped)
	}
	if label := p.Voices[0].Samples[0].Evidence.Label; label != "acme.com" && label != "acme.com/about" {
		t.Fatalf("sample label = %q", label)
	}
	if len(p.Facts) != 1 || p.Facts[0].Source == "" {
		t.Fatalf("facts = %+v", p.Facts)
	}
	if got.ModelIDs["extract"] != "extract-model" || got.ModelIDs["compose"] != "compose-model" {
		t.Fatalf("models = %v", got.ModelIDs)
	}
	if f.charges(t) != 1 {
		t.Fatal("the run was not charged once")
	}
	brand, _ := f.repos.Brands.GetAll(f.ctx)
	if len(brand.Voices) != 0 || len(brand.Facts) != 0 {
		t.Fatal("a proposal wrote brand material before commit")
	}

	t.Run("a settled run is left alone", func(t *testing.T) {
		if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil || f.flows.extracts != 1 {
			t.Fatalf("err = %v, extracts = %d", err, f.flows.extracts)
		}
	})
}

func TestRunWaitsForIngestingSourcesThenGoesOnWithout(t *testing.T) {
	f := newFixture(t)
	f.seedAsset(t, "deck", models.AssetStatusPending, "")
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, AssetIDs: []string{"deck"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, run.ID); !errors.Is(err, branddistill.ErrWaiting) {
		t.Fatalf("err = %v, want ErrWaiting", err)
	}
	if got := f.run(t, run.ID); got.Status != models.BrandDistillStatusCollecting {
		t.Fatalf("status = %s", got.Status)
	}

	f.now = f.now.Add(16 * time.Minute)
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.run(t, run.ID)
	if got.Status != models.BrandDistillStatusFailed || got.FailureCode != models.BrandDistillCodeNoSources || got.Sources[0].Status != models.DistillSourceTimedOut {
		t.Fatalf("run = %s/%s, source = %s", got.Status, got.FailureCode, got.Sources[0].Status)
	}
	if f.charges(t) != 0 || f.flows.extracts != 0 {
		t.Fatal("a run with nothing to read reached the model or was charged")
	}
}

func TestQuestionsOnlyRunSkipsReading(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Answers: []branddistill.Answer{
		{QuestionID: "q_offer", Question: "What do you sell?", Answer: "Coffee, to home brewers."},
		{QuestionID: "q_legal", Question: "What does legal not let you say?", Answer: "  "},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Answers) != 1 {
		t.Fatalf("answers = %+v, want the blank one dropped", run.Answers)
	}
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.run(t, run.ID)
	if got.Status != models.BrandDistillStatusReady || f.flows.extracts != 0 || len(got.Proposal.Audiences) != 1 {
		t.Fatalf("run = %s, extracts = %d, proposal = %+v", got.Status, f.flows.extracts, got.Proposal)
	}
	if f.charges(t) != 1 {
		t.Fatal("a questions run is charged once")
	}
}

func TestRequestChecks(t *testing.T) {
	f := newFixture(t)
	f.seedAsset(t, "broken", models.AssetStatusFailed, "")
	for _, tt := range []struct {
		name string
		in   branddistill.RequestInput
		code string
	}{
		{"nothing given", branddistill.RequestInput{}, branddistill.CodeEmptyRequest},
		{"unknown asset", branddistill.RequestInput{AssetIDs: []string{"nope"}}, branddistill.CodeUnknownAsset},
		{"failed asset", branddistill.RequestInput{AssetIDs: []string{"broken"}}, branddistill.CodeAssetFailed},
		{"bad url", branddistill.RequestInput{WebsiteURL: "ftp://acme.com"}, branddistill.CodeInvalidURL},
		{"answer without id", branddistill.RequestInput{Answers: []branddistill.Answer{{Answer: "x"}}}, branddistill.CodeInvalidAnswer},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.TenantID = tenant
			_, err := f.svc.Request(f.ctx, tt.in)
			if in, ok := errors.AsType[*branddistill.InputError](err); !ok || in.Code != tt.code {
				t.Fatalf("err = %v, want %s", err, tt.code)
			}
		})
	}

	t.Run("one open run per workspace", func(t *testing.T) {
		first, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Answers: []branddistill.Answer{{QuestionID: "q", Answer: "a"}}})
		if err != nil {
			t.Fatal(err)
		}
		open, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Answers: []branddistill.Answer{{QuestionID: "q", Answer: "b"}}})
		if !errors.Is(err, branddistill.ErrInProgress) || open == nil || open.ID != first.ID {
			t.Fatalf("second = %+v, %v", open, err)
		}
		if _, err := f.svc.Discard(f.ctx, tenant, first.ID); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Run(f.ctx, tenant, first.ID); err != nil || f.run(t, first.ID).Status != models.BrandDistillStatusDiscarded {
			t.Fatal("a discarded run was worked on")
		}
	})
}

func TestRefineComposesAgainFromTheSamePassages(t *testing.T) {
	f := newFixture(t)
	f.seedAsset(t, "deck", models.AssetStatusReady, "We write plainly. Nobody needs another adjective.")
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, AssetIDs: []string{"deck"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	parent := f.run(t, run.ID)
	q := parent.Proposal.Questions[0]

	child, err := f.svc.Refine(f.ctx, tenant, run.ID, new("user-1"), []branddistill.Answer{{QuestionID: q.ID, Question: q.Text, Answer: "Dark-roast fans."}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, child.ID); err != nil {
		t.Fatal(err)
	}
	got := f.run(t, child.ID)
	if got.Status != models.BrandDistillStatusReady || f.flows.extracts != 1 || len(f.flows.composes) != 2 {
		t.Fatalf("child = %s, extracts = %d, composes = %d", got.Status, f.flows.extracts, len(f.flows.composes))
	}
	if last := f.flows.composes[1]; len(last.Candidates) != len(parent.Candidates) || len(last.Answers) != 1 {
		t.Fatalf("refine composed from %d passages and %d answers", len(last.Candidates), len(last.Answers))
	}
	if got.Proposal.Voices[0].ProposalID != parent.Proposal.Voices[0].ProposalID || got.Proposal.Questions[0].ID != "r1_q1" {
		t.Fatalf("ids = %s / %s", got.Proposal.Voices[0].ProposalID, got.Proposal.Questions[0].ID)
	}
	if p := f.run(t, run.ID); p.Status != models.BrandDistillStatusSuperseded || p.ChildRunID == nil || *p.ChildRunID != child.ID {
		t.Fatalf("parent = %s %v", p.Status, p.ChildRunID)
	}
	if f.charges(t) != 1 {
		t.Fatal("a refine was charged")
	}
	if _, err := f.svc.Refine(f.ctx, tenant, run.ID, nil, []branddistill.Answer{{QuestionID: "x", Answer: "y"}}); !errors.Is(err, branddistill.ErrNotReady) {
		t.Fatalf("refining a superseded run: %v", err)
	}
}

func TestCommitAddsWithoutTouchingWhatIsThere(t *testing.T) {
	f := newFixture(t)
	brands := f.repos.Brands
	now := time.Now().UTC()
	plain := &models.BrandVoice{ID: "voice-plain", Name: "Plain", IsDefault: true, Samples: models.StringSlice{"x"}, ChannelNotes: models.StringMap{}, Origin: models.BrandOrigin{Kind: "blank"}, CreatedAt: now, UpdatedAt: now}
	if err := brands.CreateVoice(f.ctx, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := brands.SaveGuardrails(f.ctx, &models.BrandGuardrails{ID: "g", MayClaim: models.StringSlice{"Free delivery"}, NeverClaim: models.StringSlice{}, BannedWords: models.StringSlice{}, Disclaimer: "Ours.", UpdatedAt: now}, []string{"Founded in 2015."}, nil); err != nil {
		t.Fatal(err)
	}
	ev := []models.DistillEvidence{{Ref: "s1.1", AssetID: "deck", Label: "Brand book", Quote: "q"}}
	run := &models.BrandDistillRun{
		Status:  models.BrandDistillStatusReady,
		Sources: []models.DistillSource{{AssetID: "deck", Title: "Brand book", Role: models.DistillRoleAsset}},
		Proposal: &models.BrandProposal{
			Voices: []models.ProposedVoice{{ProposalID: "v1", Name: "Warm", IsDefault: true, Evidence: ev}},
			Guardrails: models.ProposedGuardrails{
				MayClaim:   []models.ProposedLine{{ProposalID: "g:mayClaim:1", Text: "Roasted to order", Evidence: ev}, {ProposalID: "g:mayClaim:2", Text: "free  DELIVERY", Evidence: ev}},
				Disclaimer: &models.ProposedDisclaimer{ProposalID: "g:disclaimer", Text: "Prices include VAT.", Evidence: ev},
			},
			Facts: []models.ProposedFact{
				{ProposalID: "f1", Statement: "Roasted weekly.", Evidence: ev},
				{ProposalID: "f2", Statement: "Founded in 2015.", Evidence: ev},
			},
		},
	}
	if err := f.repos.Runs.Create(f.ctx, f.db, run); err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Commit(f.ctx, branddistill.CommitInput{
		TenantID: tenant, RunID: run.ID, UserID: new("user-1"),
		Voices:   []branddistill.CommitVoice{{ProposalID: "v1", Name: "Warm, exacting", IsDefault: true, Samples: []string{"We roast light."}}},
		MayClaim: []branddistill.CommitLine{{ProposalID: "g:mayClaim:1", Text: "Roasted to order"}, {ProposalID: "g:mayClaim:2", Text: "free  DELIVERY"}},
		Facts: []branddistill.CommitFact{
			{ProposalID: "f1", Statement: "Roasted weekly.", Subject: models.FactSubjectUs, Kind: models.FactKindDocumented, Source: "Brand book"},
			{ProposalID: "f2", Statement: "Founded in 2015.", Subject: models.FactSubjectUs, Kind: models.FactKindDocumented},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.Status != models.BrandDistillStatusCommitted || len(res.Result.Voices) != 1 || len(res.Result.Facts) != 1 || len(res.Result.Skipped) != 2 {
		t.Fatalf("result = %+v", res.Result)
	}

	brand, _ := brands.GetAll(f.ctx)
	if len(brand.Voices) != 2 || !brand.Voices[0].IsDefault || brand.Voices[1].IsDefault {
		t.Fatalf("voices = %+v; the existing default must stay", brand.Voices)
	}
	if o := brand.Voices[1].Origin; o.Kind != models.BrandOriginDocument || o.AssetID != "deck" || o.RunID != run.ID {
		t.Fatalf("origin = %+v", o)
	}
	if strings.Join(brand.Guardrails.MayClaim, "|") != "Free delivery|Roasted to order" || brand.Guardrails.Disclaimer != "Ours." {
		t.Fatalf("guardrails = %+v; lines merge, the disclaimer stays unless sent", brand.Guardrails)
	}
	if len(brand.Facts) != 2 || brand.Facts[1].AddedAt == nil || brand.Facts[1].CreatedBy == nil {
		t.Fatalf("facts = %+v", brand.Facts)
	}

	if _, err := f.svc.Commit(f.ctx, branddistill.CommitInput{TenantID: tenant, RunID: run.ID, Facts: []branddistill.CommitFact{{ProposalID: "f1"}}}); !errors.Is(err, branddistill.ErrCommitted) {
		t.Fatalf("second commit: %v", err)
	}
}

func TestCommitChecksItems(t *testing.T) {
	f := newFixture(t)
	run := &models.BrandDistillRun{Status: models.BrandDistillStatusReady, Proposal: &models.BrandProposal{
		Voices: []models.ProposedVoice{{ProposalID: "v1", Name: "Warm"}},
	}}
	if err := f.repos.Runs.Create(f.ctx, f.db, run); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		in   branddistill.CommitInput
		code string
	}{
		{"nothing kept", branddistill.CommitInput{}, branddistill.CodeNothingToSave},
		{"unknown item", branddistill.CommitInput{Facts: []branddistill.CommitFact{{ProposalID: "f9"}}}, branddistill.CodeUnknownItem},
		{"broken item", branddistill.CommitInput{Voices: []branddistill.CommitVoice{{ProposalID: "v1", Name: " "}}}, branddistill.CodeInvalidItem},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.TenantID, tt.in.RunID = tenant, run.ID
			_, err := f.svc.Commit(f.ctx, tt.in)
			if in, ok := errors.AsType[*branddistill.InputError](err); !ok || in.Code != tt.code {
				t.Fatalf("err = %v, want %s", err, tt.code)
			}
		})
	}
	if got := f.run(t, run.ID); got.Status != models.BrandDistillStatusReady {
		t.Fatalf("a refused commit moved the run to %s", got.Status)
	}
}

func TestExpireDropsOldProposals(t *testing.T) {
	f := newFixture(t)
	run := &models.BrandDistillRun{Status: models.BrandDistillStatusReady, Proposal: &models.BrandProposal{}}
	if err := f.repos.Runs.Create(f.ctx, f.db, run); err != nil {
		t.Fatal(err)
	}
	if err := f.repos.Runs.Save(f.ctx, f.db, run, "status"); err != nil {
		t.Fatal(err)
	}
	if n, err := f.svc.Expire(f.ctx); err != nil || n != 0 {
		t.Fatalf("fresh proposal expired: %d %v", n, err)
	}
	f.now = f.now.Add(branddistill.ExpireAfter + time.Hour)
	if n, err := f.svc.Expire(f.ctx); err != nil || n != 1 {
		t.Fatalf("expired %d, %v", n, err)
	}
	if got := f.run(t, run.ID); got.Status != models.BrandDistillStatusExpired || got.Proposal != nil {
		t.Fatalf("run = %s", got.Status)
	}
}
