package branddistill_test

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/brand_distill"
	"github.com/ogen-app/ogen/src/usecase/branddistill"
)

func TestAFailedRefineHandsTheProposalBack(t *testing.T) {
	f := newFixture(t)
	f.seedAsset(t, "deck", models.AssetStatusReady, "We write plainly. Nobody needs another adjective.")
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, AssetIDs: []string{"deck"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	child, err := f.svc.Refine(f.ctx, tenant, run.ID, nil, []branddistill.Answer{{QuestionID: "r0_q1", Answer: "Dark-roast fans."}})
	if err != nil {
		t.Fatal(err)
	}
	if p := f.run(t, run.ID); p.Status != models.BrandDistillStatusReady || p.ChildRunID == nil {
		t.Fatalf("parent while its refine composes = %s %v; want ready and linked", p.Status, p.ChildRunID)
	}
	if _, err := f.svc.Commit(f.ctx, branddistill.CommitInput{TenantID: tenant, RunID: run.ID, Facts: []branddistill.CommitFact{{ProposalID: "f1"}}}); !errors.Is(err, branddistill.ErrInProgress) {
		t.Fatalf("committing a run whose refine composes: %v", err)
	}

	f.flows.composeErr = &brand_distill.AIError{Msg: "no", Refused: true}
	if err := f.svc.Run(f.ctx, tenant, child.ID); err != nil {
		t.Fatal(err)
	}
	if c := f.run(t, child.ID); c.Status != models.BrandDistillStatusFailed {
		t.Fatalf("child = %s", c.Status)
	}
	p := f.run(t, run.ID)
	if p.Status != models.BrandDistillStatusReady || p.ChildRunID != nil || p.Proposal == nil {
		t.Fatalf("parent after a failed refine = %s %v; want ready, unlinked, proposal kept", p.Status, p.ChildRunID)
	}
	if len(p.Proposal.Voices) == 0 {
		t.Fatal("the parent's proposal has no voice to keep")
	}
	v := p.Proposal.Voices[0]
	res, err := f.svc.Commit(f.ctx, branddistill.CommitInput{TenantID: tenant, RunID: run.ID, Voices: []branddistill.CommitVoice{
		{ProposalID: v.ProposalID, Name: v.Name, Samples: []string{v.Samples[0].Text}},
	}})
	if err != nil || res.Run.Status != models.BrandDistillStatusCommitted {
		t.Fatalf("committing the handed-back proposal: %v", err)
	}
}

func TestADiscardedRefineHandsTheProposalBack(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Answers: []branddistill.Answer{{QuestionID: "q_offer", Answer: "Coffee."}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatal(err)
	}
	child, err := f.svc.Refine(f.ctx, tenant, run.ID, nil, []branddistill.Answer{{QuestionID: "r0_q1", Answer: "Nobody else."}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Discard(f.ctx, tenant, child.ID); err != nil {
		t.Fatal(err)
	}
	if p := f.run(t, run.ID); p.Status != models.BrandDistillStatusReady || p.ChildRunID != nil {
		t.Fatalf("parent after a discarded refine = %s %v", p.Status, p.ChildRunID)
	}
}

func TestDiscardRefundsOnlyARunThatNeverFinished(t *testing.T) {
	f := newFixture(t)
	ready, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Answers: []branddistill.Answer{{QuestionID: "q_offer", Answer: "Coffee."}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Run(f.ctx, tenant, ready.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Discard(f.ctx, tenant, ready.ID); err != nil {
		t.Fatal(err)
	}
	if f.charges(t) != 1 {
		t.Fatal("discarding a finished proposal gave the read back")
	}

	open := &models.BrandDistillRun{Status: models.BrandDistillStatusComposing, Charged: true}
	if err := f.repos.Runs.Create(f.ctx, f.db, open); err != nil {
		t.Fatal(err)
	}
	if err := f.repos.Runs.RecordCharge(f.ctx, open.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Discard(f.ctx, tenant, open.ID); err != nil {
		t.Fatal(err)
	}
	if f.charges(t) != 1 {
		t.Fatal("discarding an unfinished run kept its charge")
	}
}

func TestARunDiscardedWhileItWorksStaysDiscarded(t *testing.T) {
	f := newFixture(t)
	run, err := f.svc.Request(f.ctx, branddistill.RequestInput{TenantID: tenant, Answers: []branddistill.Answer{{QuestionID: "q_offer", Answer: "Coffee."}}})
	if err != nil {
		t.Fatal(err)
	}
	f.flows.during = func() {
		if _, err := f.svc.Discard(f.ctx, tenant, run.ID); err != nil {
			t.Error(err)
		}
	}
	if err := f.svc.Run(f.ctx, tenant, run.ID); err != nil {
		t.Fatalf("a run closed under the job should end it quietly: %v", err)
	}
	if got := f.run(t, run.ID); got.Status != models.BrandDistillStatusDiscarded || got.Proposal != nil {
		t.Fatalf("run = %s; the job wrote over the discard", got.Status)
	}
}
