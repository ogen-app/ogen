package packs

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

// memDistill is one ready run, w1, superseding w0.
type memDistill struct {
	requested  *branddistill.RequestInput
	refined    []branddistill.Answer
	refinedRun string
	committed  *branddistill.CommitInput
}

var distillProposal = &models.BrandProposal{
	Voices:    []models.ProposedVoice{{ProposalID: "pv1", Name: "Warm", WhenToUse: "Instagram", Samples: []models.ProposedSample{{Text: "Welcome home."}}}},
	Audiences: []models.ProposedAudience{{ProposalID: "pa1", Name: "Families", Who: "Parents booking summer"}},
	Guardrails: models.ProposedGuardrails{BannedWords: []models.ProposedLine{{ProposalID: "pb1", Text: "cheap"}},
		Disclaimer: &models.ProposedDisclaimer{ProposalID: "pd1", Text: "Prices vary.", Current: new("Terms apply.")}},
	Facts:     []models.ProposedFact{{ProposalID: "pf1", Statement: "Open since 1998"}},
	Questions: []models.DistillQuestion{{ID: "q1", Text: "Who books most?", Section: "audiences"}},
}

func (m *memDistill) Request(_ context.Context, in branddistill.RequestInput) (*models.BrandDistillRun, error) {
	m.requested = &in
	return &models.BrandDistillRun{ID: "w2", Status: models.BrandDistillStatusPending}, nil
}

func (m *memDistill) Get(_ context.Context, _, id string) (*models.BrandDistillRun, error) {
	return &models.BrandDistillRun{ID: id, Status: models.BrandDistillStatusReady, Proposal: distillProposal}, nil
}

func (m *memDistill) List(context.Context, string) ([]models.BrandDistillRun, error) {
	return []models.BrandDistillRun{{ID: "w0", Status: models.BrandDistillStatusSuperseded}, {ID: "w1", Status: models.BrandDistillStatusReady}}, nil
}

func (m *memDistill) Refine(_ context.Context, _, runID string, _ *string, in []branddistill.Answer) (*models.BrandDistillRun, error) {
	m.refinedRun, m.refined = runID, in
	return &models.BrandDistillRun{ID: "w3", Status: models.BrandDistillStatusPending}, nil
}

func (m *memDistill) Commit(_ context.Context, in branddistill.CommitInput) (*branddistill.CommitResult, error) {
	m.committed = &in
	return &branddistill.CommitResult{Result: &repository.DistilledResult{Voices: map[string]string{"pv1": "v1"}}}, nil
}

func TestDistill(t *testing.T) {
	m := &memDistill{}
	d := Deps{Distill: m}
	ctx, _ := turn(t, workspace, nil)

	got, err := getBrandDistill(ctx, d, DistillRef{})
	if err != nil || got.RunID != "w1" || got.Counts.Voices != 1 || len(got.Items) != 5 || len(got.Questions) != 1 {
		t.Fatalf("latest = %+v, %v", got, err)
	}

	// On an asset page with nothing else given, the open asset is read.
	page, _ := assistant.NewViewTurnContext(tenantctx.With(t.Context(), "tenant-1"), viewcontext.ViewContext{
		Version: viewcontext.Version, Screen: "asset", Scope: workspace, Focus: &viewcontext.Ref{Kind: viewcontext.KindAsset, ID: "a1"}}, nil)
	if _, err := startBrandDistill(page, d, StartDistillInput{}); err != nil || !slices.Equal(m.requested.AssetIDs, []string{"a1"}) {
		t.Fatalf("start = %+v, %v", m.requested, err)
	}

	answered, err := answerDistill(ctx, d, AnswerDistillInput{Answers: []DistillAnswerInput{
		{QuestionID: "q1", Answer: "Families", From: " the brochure, p. 2 ", AssetIDs: []string{"a1", " a1"}}}})
	if err != nil || len(m.refined) != 1 || m.refined[0].Question != "Who books most?" {
		t.Fatalf("refined = %+v, %v", m.refined, err)
	}
	// The result says what was answered and from what, citing each asset once.
	if want := []DistillAnswered{{QuestionID: "q1", Question: "Who books most?", Answer: "Families", From: "the brochure, p. 2"}}; !slices.Equal(answered.Answered, want) ||
		answered.Grounding == nil || len(answered.Grounding.Sources) != 1 || answered.Grounding.Sources[0].ID != "a1" {
		t.Fatalf("answered = %+v, grounding %+v", answered.Answered, answered.Grounding)
	}
	_, err = answerDistill(ctx, d, AnswerDistillInput{Answers: []DistillAnswerInput{{QuestionID: "nope", Answer: "x"}}})
	refused(t, err)

	// Keeping everything leaves the workspace's own disclaimer alone.
	saved, err := commitDistill(ctx, d, CommitDistillInput{Skip: []string{"pf1"}})
	if err != nil || saved.Voices != 1 {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	c := m.committed
	if c.RunID != "w1" || len(c.Voices) != 1 || len(c.Voices[0].Samples) != 1 || len(c.Audiences) != 1 || len(c.BannedWords) != 1 ||
		c.Disclaimer != nil || len(c.Facts) != 0 {
		t.Fatalf("commit = %+v", c)
	}
	// Naming it saves it.
	if _, err := commitDistill(ctx, d, CommitDistillInput{Keep: []string{"pd1"}}); err != nil || m.committed.Disclaimer == nil || len(m.committed.Voices) != 0 {
		t.Fatalf("commit = %+v, %v", m.committed, err)
	}

	// On an open reading, that reading is the one read, answered and saved,
	// not the latest; a run named outright still wins.
	read, _ := assistant.NewViewTurnContext(tenantctx.With(t.Context(), "tenant-1"), viewcontext.ViewContext{
		Version: viewcontext.Version, Screen: "foundation.read", Scope: workspace, Focus: &viewcontext.Ref{Kind: viewcontext.KindDistill, ID: "w9"}}, nil)
	if got, err := getBrandDistill(read, d, DistillRef{}); err != nil || got.RunID != "w9" {
		t.Fatalf("open = %+v, %v", got, err)
	}
	if _, err := answerDistill(read, d, AnswerDistillInput{Answers: []DistillAnswerInput{{QuestionID: "q1", Answer: "Families"}}}); err != nil || m.refinedRun != "w9" {
		t.Fatalf("refined %q, %v", m.refinedRun, err)
	}
	if _, err := commitDistill(read, d, CommitDistillInput{}); err != nil || m.committed.RunID != "w9" {
		t.Fatalf("commit = %+v, %v", m.committed, err)
	}
	if got, err := getBrandDistill(read, d, DistillRef{RunID: "w1"}); err != nil || got.RunID != "w1" {
		t.Fatalf("named = %+v, %v", got, err)
	}
}

// A run counts what it reads: the website by its address and the pages read
// off it, and the assets.
func TestDistillSources(t *testing.T) {
	run := &models.BrandDistillRun{WebsiteURL: new("villa-aurora.example"), Sources: []models.DistillSource{
		{Role: models.DistillRoleWebsite}, {Role: models.DistillRoleWebsite}, {Role: models.DistillRoleAsset}}}
	if got := distillSources(run); got != (DistillSources{Website: "villa-aurora.example", Pages: 2, Assets: 1}) {
		t.Fatalf("sources = %+v", got)
	}
	mapped := &models.BrandDistillRun{Website: &models.DistillWebsite{URL: "https://villa-aurora.example"}}
	if got := distillOut(mapped).Sources; got != (DistillSources{Website: "https://villa-aurora.example"}) {
		t.Fatalf("mapped = %+v", got)
	}
}
