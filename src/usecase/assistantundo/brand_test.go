package assistantundo_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

// factFixture stores a fact by user-1 and returns the brand repository and
// the service that undoes fact changes.
func factFixture(t *testing.T, f *fixture) (repository.BrandRepository, *assistantundo.Service, *models.BrandFact) {
	t.Helper()
	brand := repository.NewBrandRepository(f.db)
	created := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	fact := &models.BrandFact{ID: "fact-1", Statement: "NPS is 62", Subject: models.FactSubjectUs, Kind: models.FactKindMeasured,
		CreatedBy: new("user-1"), CreatedAt: created, UpdatedAt: created}
	if err := brand.CreateFact(f.ctx, fact); err != nil {
		t.Fatal(err)
	}
	return brand, assistantundo.New(f.db, f.actions, f.campaigns).WithBrand(brand), fact
}

// logFact logs a fact change, as the foundation tools do.
func logFact(t *testing.T, f *fixture, before, after *models.BrandFact) string {
	t.Helper()
	id, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{
		ScopeKind: models.AssistantScopeWorkspace, Kind: models.AssistantActionBrandFact, EntityID: "fact-1",
		Before: assistantundo.BrandFact(before), After: assistantundo.BrandFact(after), CreatedBy: "user-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func loadFact(t *testing.T, f *fixture, brand repository.BrandRepository) *models.BrandFact {
	t.Helper()
	got, err := brand.GetFact(f.ctx, "fact-1")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Undoing an add after a later edit is refused: the edit is kept and the
// change is not recorded as undone.
func TestUndo_FactAddRefusedAfterLaterEdit(t *testing.T) {
	f := newFixture(t)
	brand, svc, fact := factFixture(t, f)
	id := logFact(t, f, nil, fact)

	edit := *fact
	edit.Statement = "NPS is 64"
	if _, err := brand.UpdateFact(f.ctx, &edit); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("err = %v, want ErrChangedSince", err)
	}
	if got := loadFact(t, f, brand); got == nil || got.Statement != "NPS is 64" {
		t.Fatalf("fact = %+v, want the later edit kept", got)
	}
	if f.undone(t, id) {
		t.Fatal("a refused undo was recorded")
	}
}

// An undo waiting on a concurrent edit's row lock sees the edit, and leaves it.
func TestUndo_FactWaitsForAConcurrentEdit(t *testing.T) {
	f := newFixture(t)
	brand, svc, fact := factFixture(t, f)
	before := *fact
	before.Statement = "NPS is 60"
	id := logFact(t, f, &before, fact)

	tx, err := f.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.NewUpdate().Model((*models.BrandFact)(nil)).Set("statement = ?", "NPS is 64").
		Where("id = ?", "fact-1").Exec(f.ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := svc.Undo(f.ctx, id, "user-1")
		done <- err
	}()
	// Give the undo time to block on the edit's row lock, then commit it.
	time.Sleep(300 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("err = %v, want ErrChangedSince", err)
	}
	if got := loadFact(t, f, brand); got.Statement != "NPS is 64" {
		t.Fatalf("statement = %q, the concurrent edit was overwritten", got.Statement)
	}
}

// failAfterRestore restores the fact on the undo's transaction, then fails,
// as a dropped connection would.
type failAfterRestore struct{ repository.BrandRepository }

func (b failAfterRestore) RestoreFact(ctx context.Context, db bun.IDB, f *models.BrandFact) error {
	if err := b.BrandRepository.RestoreFact(ctx, db, f); err != nil {
		return err
	}
	return errors.New("connection lost")
}

// Undoing a removal puts the fact back with its author and creation time, in
// the transaction that records the undo: a failed undo leaves it removed.
func TestUndo_FactRestoreCommitsWithRecord(t *testing.T) {
	f := newFixture(t)
	brand, _, fact := factFixture(t, f)
	gone, err := brand.DeleteFact(f.ctx, fact.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := logFact(t, f, gone, nil)

	failing := assistantundo.New(f.db, f.actions, f.campaigns).WithBrand(failAfterRestore{brand})
	if _, err := failing.Undo(f.ctx, id, "user-1"); err == nil {
		t.Fatal("undo succeeded with a failed restore")
	}
	if got := loadFact(t, f, brand); got != nil {
		t.Fatalf("fact = %+v, the restore outlived the rolled-back undo", got)
	}
	if f.undone(t, id) {
		t.Fatal("the failed undo was recorded")
	}

	svc := assistantundo.New(f.db, f.actions, f.campaigns).WithBrand(brand)
	if _, err := svc.Undo(f.ctx, id, "user-2"); err != nil {
		t.Fatal(err)
	}
	got := loadFact(t, f, brand)
	if got == nil || got.CreatedBy == nil || *got.CreatedBy != "user-1" || got.CreatedByName != "Jane" || !got.CreatedAt.Equal(fact.CreatedAt) {
		t.Fatalf("restored = %+v, want Jane's fact from %v", got, fact.CreatedAt)
	}
}
