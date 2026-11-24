package branddistill_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

const deckText = "We roast light. We say why. We never burn a bean. We write like we talk."

func readsOf(t *testing.T, f *fixture, runID string) []models.BrandDistillRun {
	t.Helper()
	reads, err := f.svc.Reads(f.ctx, tenant, runID)
	if err != nil {
		t.Fatal(err)
	}
	return reads
}

func TestIncrementalRunReadsEachSourceOnItsOwn(t *testing.T) {
	f := newFixture(t)
	f.seedAsset(t, "deck", models.AssetStatusReady, deckText)
	f.seedAsset(t, "notes", models.AssetStatusReady, "We ship within 48 hours. Our beans come from Kenya.")
	f.seedAsset(t, "slow", models.AssetStatusPending, "")

	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{
		TenantID: tenant, UserID: new("user-1"), Incremental: true, AssetIDs: []string{"deck"},
		Answers: []branddistill.Answer{{QuestionID: "q_three_words", Question: "Three words", Answer: "Warm, plain, exact"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.BrandDistillStatusCollecting || !run.Incremental {
		t.Fatalf("run = %s incremental=%v", run.Status, run.Incremental)
	}
	reads := readsOf(t, f, run.ID)
	if len(reads) != 2 || reads[0].ReadKind() != models.DistillReadAsset || reads[1].ReadKind() != models.DistillReadAnswers {
		t.Fatalf("reads = %+v", reads)
	}
	if reads[1].Status != models.BrandDistillStatusReady {
		t.Fatalf("answers read = %s, want ready at once", reads[1].Status)
	}
	if len(f.jobs.runs) != 1 || f.jobs.runs[0] != reads[0].ID {
		t.Fatalf("jobs = %v, want only the asset read", f.jobs.runs)
	}
	if c := f.run(t, run.ID).Coverage[branddistill.SectionVoices]; c.Level != branddistill.LevelThin || c.Note != "1 answer." {
		t.Fatalf("coverage after the answers = %+v", c)
	}

	// The asset read finishes on its own and the run's coverage moves.
	if err := f.svc.Run(f.ctx, tenant, reads[0].ID); err != nil {
		t.Fatal(err)
	}
	deck := f.run(t, reads[0].ID)
	if deck.Status != models.BrandDistillStatusReady || deck.Proposal != nil || len(deck.Candidates) == 0 {
		t.Fatalf("deck read = %s, proposal = %v, candidates = %d", deck.Status, deck.Proposal, len(deck.Candidates))
	}
	if deck.Coverage[branddistill.SectionVoices].Level == branddistill.LevelNone {
		t.Fatalf("the deck read added nothing: %+v", deck.Coverage)
	}
	if got := f.run(t, run.ID).Coverage[branddistill.SectionVoices].Note; !strings.Contains(got, "1 answer") || !strings.Contains(got, "passage") {
		t.Fatalf("run coverage note = %q, want the passages and the answer", got)
	}
	if f.charges(t) != 1 {
		t.Fatalf("charges = %d after the first read, want 1", f.charges(t))
	}

	notes, err := f.svc.AddRead(f.ctx, branddistill.ReadInput{TenantID: tenant, RunID: run.ID, AssetID: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, notes.ID); err != nil {
		t.Fatal(err)
	}
	if f.charges(t) != 1 {
		t.Fatalf("charges = %d after a second read, want the run charged once", f.charges(t))
	}

	t.Run("one source is read once", func(t *testing.T) {
		_, err := f.svc.AddRead(f.ctx, branddistill.ReadInput{TenantID: tenant, RunID: run.ID, AssetID: "notes"})
		if in, ok := errors.AsType[*branddistill.InputError](err); !ok || in.Code != branddistill.CodeAlreadyRead {
			t.Fatalf("err = %v", err)
		}
		_, err = f.svc.AddRead(f.ctx, branddistill.ReadInput{TenantID: tenant, RunID: run.ID, AssetID: "deck", WebsiteURL: "https://acme.com"})
		if in, ok := errors.AsType[*branddistill.InputError](err); !ok || in.Code != branddistill.CodeOneSource {
			t.Fatalf("err = %v", err)
		}
	})

	slow, err := f.svc.AddRead(f.ctx, branddistill.ReadInput{TenantID: tenant, RunID: run.ID, AssetID: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, slow.ID); !errors.Is(err, branddistill.ErrWaiting) {
		t.Fatalf("err = %v, want the slow read waiting", err)
	}
	if _, err := f.svc.Compose(f.ctx, tenant, run.ID); !errors.Is(err, branddistill.ErrReadsOpen) {
		t.Fatalf("compose with a read open: err = %v", err)
	}
	if _, err := f.svc.StopRead(f.ctx, tenant, run.ID, slow.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, slow.ID); got.Status != models.BrandDistillStatusDiscarded {
		t.Fatalf("stopped read = %s", got.Status)
	}
	if err := f.svc.Run(f.ctx, tenant, slow.ID); err != nil {
		t.Fatalf("a stopped read's job: %v", err)
	}

	composed, err := f.svc.Compose(f.ctx, tenant, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if composed.Status != models.BrandDistillStatusPending || f.jobs.runs[len(f.jobs.runs)-1] != run.ID {
		t.Fatalf("composed = %s, jobs = %v", composed.Status, f.jobs.runs)
	}
	if _, err := f.svc.AddRead(f.ctx, branddistill.ReadInput{TenantID: tenant, RunID: run.ID, AssetID: "slow"}); !errors.Is(err, branddistill.ErrNotCollecting) {
		t.Fatalf("a read after compose: err = %v", err)
	}
	extracts := f.flows.extracts
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	got := f.run(t, run.ID)
	if got.Status != models.BrandDistillStatusReady || got.Proposal == nil {
		t.Fatalf("run = %s %s", got.Status, got.FailureReason)
	}
	if f.flows.extracts != extracts {
		t.Fatal("composing read the sources again")
	}
	req := f.flows.composes[len(f.flows.composes)-1]
	refs := map[string]bool{}
	assets := map[string]bool{}
	for _, c := range req.Candidates {
		if refs[c.Ref] {
			t.Fatalf("ref %s is used twice: %+v", c.Ref, req.Candidates)
		}
		refs[c.Ref], assets[c.AssetID] = true, true
	}
	if !assets["deck"] || !assets["notes"] || len(req.Answers) != 1 {
		t.Fatalf("composed from %v and %d answers", assets, len(req.Answers))
	}
	if len(got.Sources) != 2 {
		t.Fatalf("run sources = %+v, want the two read ones", got.Sources)
	}
	if f.charges(t) != 1 {
		t.Fatalf("charges = %d, want one for the whole run", f.charges(t))
	}
}

func TestIncrementalRunHoldsTheSlot(t *testing.T) {
	f := newFixture(t)
	f.seedAsset(t, "deck", models.AssetStatusPending, "")
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, AssetIDs: []string{"deck"}}); !errors.Is(err, branddistill.ErrInProgress) {
		t.Fatalf("a second run: err = %v", err)
	}
	read, err := f.svc.AddRead(f.ctx, branddistill.ReadInput{TenantID: tenant, RunID: run.ID, AssetID: "deck"})
	if err != nil {
		t.Fatal(err)
	}
	if runs, _ := f.svc.List(f.ctx, tenant); len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("list = %+v, want the run without its reads", runs)
	}
	if _, err := f.svc.Compose(f.ctx, tenant, read.ID); !errors.Is(err, branddistill.ErrNotCollecting) {
		t.Fatalf("composing a read: err = %v", err)
	}

	if _, err := f.svc.Discard(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.run(t, read.ID); got.Status != models.BrandDistillStatusDiscarded {
		t.Fatalf("an open read of a discarded run = %s", got.Status)
	}
	if _, err := f.svc.Compose(f.ctx, tenant, run.ID); !errors.Is(err, branddistill.ErrNotCollecting) {
		t.Fatalf("composing a discarded run: err = %v", err)
	}
}

func TestComposeNeedsSomethingRead(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Compose(f.ctx, tenant, run.ID)
	if in, ok := errors.AsType[*branddistill.InputError](err); !ok || in.Code != branddistill.CodeNothingRead {
		t.Fatalf("err = %v", err)
	}
}
