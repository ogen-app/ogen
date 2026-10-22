package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestBrandDistillOneOpenRunAndItsLifecycle(t *testing.T) {
	f := newAnalysisFixture(t)
	runs := repository.NewBrandDistillRepository(f.db)

	first := &models.BrandDistillRun{Status: models.BrandDistillStatusPending, WebsiteURL: new("https://acme.com")}
	if err := runs.Create(f.ctx, f.db, first); err != nil {
		t.Fatal(err)
	}
	second := &models.BrandDistillRun{Status: models.BrandDistillStatusPending}
	if err := runs.Create(f.ctx, f.db, second); !errors.Is(err, repository.ErrBrandDistillOpen) {
		t.Fatalf("second open run: err = %v", err)
	}
	open, err := runs.Open(f.ctx)
	if err != nil || open.ID != first.ID {
		t.Fatalf("Open = %+v, %v", open, err)
	}

	first.Status = models.BrandDistillStatusReady
	first.Sources = []models.DistillSource{{AssetID: "a1", Title: "About", Role: models.DistillRoleWebsite, Status: models.DistillSourceReady}}
	first.Proposal = &models.BrandProposal{Voices: []models.ProposedVoice{{ProposalID: "v1", Name: "Plain"}}}
	first.Candidates = []models.DistillCandidate{{Kind: "voice_passage", Ref: "a1#0", Quote: "We roast light."}}
	if err := runs.Save(f.ctx, f.db, first, "status", "sources", "proposal", "candidates"); err != nil {
		t.Fatal(err)
	}
	got, err := runs.Get(f.ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.FinishedAt == nil || got.Proposal == nil || got.Proposal.Voices[0].Name != "Plain" || len(got.Candidates) != 1 {
		t.Fatalf("saved run = %+v", got)
	}
	if _, err := runs.Open(f.ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Open after ready: err = %v, want ErrNoRows", err)
	}
	if err := runs.Create(f.ctx, f.db, second); err != nil {
		t.Fatalf("a new run once the first is ready: %v", err)
	}

	listed, err := runs.List(f.ctx, 10)
	if err != nil || len(listed) != 2 || listed[0].ID != second.ID || listed[1].Proposal != nil {
		t.Fatalf("List = %+v, %v; want newest first without the proposal", listed, err)
	}

	t.Run("expiry drops the working state of old ready runs only", func(t *testing.T) {
		n, err := runs.ExpireReadyBefore(f.ctx, time.Now().Add(time.Minute))
		if err != nil || n != 1 {
			t.Fatalf("expired %d, %v", n, err)
		}
		got, _ := runs.Get(f.ctx, first.ID)
		if got.Status != models.BrandDistillStatusExpired || got.Proposal != nil || got.Candidates != nil {
			t.Fatalf("expired run = %s %+v", got.Status, got.Proposal)
		}
		if again, _ := runs.Get(f.ctx, second.ID); again.Status != models.BrandDistillStatusPending {
			t.Fatalf("open run = %s, want untouched", again.Status)
		}
	})
}

func TestBrandDistillCharges(t *testing.T) {
	f := newAnalysisFixture(t)
	runs := repository.NewBrandDistillRepository(f.db)
	id, err := runs.RecordCharge(f.ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runs.RecordCharge(f.ctx, "run-2"); err != nil {
		t.Fatal(err)
	}
	if err := runs.DeleteCharge(f.ctx, id); err != nil {
		t.Fatal(err)
	}
	n, err := runs.CountChargesSince(f.ctx, time.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("charges = %d, %v", n, err)
	}
}

func TestBrandDistillStampsSourceRunOnce(t *testing.T) {
	f := newAnalysisFixture(t)
	runs := repository.NewBrandDistillRepository(f.db)
	f.seedAsset(t, "deck")
	f.seedAsset(t, "own")
	if _, err := f.db.NewUpdate().Model((*models.Asset)(nil)).
		Set("origin = ?", models.AssetOriginBrandDistill).
		Set("origin_ref = ?", `{"role":"document"}`).
		Where("id = ?", "deck").Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := runs.StampSourceRun(f.ctx, "run-1", []string{"deck", "own"}); err != nil {
		t.Fatal(err)
	}
	if err := runs.StampSourceRun(f.ctx, "run-2", []string{"deck"}); err != nil {
		t.Fatal(err)
	}
	deck, err := f.assets.GetByID(f.ctx, "deck")
	if err != nil {
		t.Fatal(err)
	}
	if deck.OriginRef == nil || deck.OriginRef.RunID != "run-1" || deck.OriginRef.Role != models.AssetRoleDocument {
		t.Fatalf("deck origin_ref = %+v, want the first run", deck.OriginRef)
	}
	own, _ := f.assets.GetByID(f.ctx, "own")
	if own.OriginRef != nil {
		t.Fatalf("an unmarked asset got %+v", own.OriginRef)
	}
}
