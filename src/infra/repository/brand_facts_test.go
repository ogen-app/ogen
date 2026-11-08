package repository_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// UpdateFactIf and DeleteFactIf write only while the stored fact still holds
// the expected editable fields.
func TestBrandFacts_ConditionalWrites(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewBrandRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-facts-if")

	now := time.Now().UTC().Truncate(time.Microsecond)
	stored := &models.BrandFact{ID: "fact-if-1", Statement: "We serve 4,000 customers", Subject: models.FactSubjectUs,
		Kind: models.FactKindMeasured, CreatedAt: now, UpdatedAt: now}
	if err := repo.CreateFact(ctx, stored); err != nil {
		t.Fatal(err)
	}
	stale := *stored
	stale.Statement = "We serve 3,900 customers"
	edit := &models.BrandFact{ID: stored.ID, Statement: "We serve 4,200 customers", Subject: models.FactSubjectUs,
		Kind: models.FactKindMeasured, UpdatedAt: now}

	if _, err := repo.UpdateFactIf(ctx, nil, edit, &stale); !errors.Is(err, repository.ErrFactChanged) {
		t.Fatalf("stale update: err = %v, want ErrFactChanged", err)
	}
	if _, err := repo.DeleteFactIf(ctx, nil, stored.ID, &stale); !errors.Is(err, repository.ErrFactChanged) {
		t.Fatalf("stale delete: err = %v, want ErrFactChanged", err)
	}
	if got, err := repo.GetFact(ctx, stored.ID); err != nil || got == nil || got.Statement != stored.Statement {
		t.Fatalf("after stale writes = %+v, %v; want it untouched", got, err)
	}

	before, err := repo.UpdateFactIf(ctx, nil, edit, stored)
	if err != nil || before.Statement != stored.Statement {
		t.Fatalf("update = %+v, %v", before, err)
	}
	if _, err := repo.DeleteFactIf(ctx, nil, stored.ID, stored); !errors.Is(err, repository.ErrFactChanged) {
		t.Fatalf("delete expecting the old statement: err = %v, want ErrFactChanged", err)
	}
	if _, err := repo.DeleteFactIf(ctx, nil, stored.ID, edit); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.DeleteFactIf(ctx, nil, stored.ID, edit); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delete a gone fact: err = %v, want sql.ErrNoRows", err)
	}
}

// RestoreFact keeps the fact's creation time and author name; an author who
// is no longer a user is kept by name alone.
func TestBrandFacts_RestoreKeepsAuthor(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewBrandRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-facts-restore")
	seedUser(t, db, "u-restore-1", "tenant-facts-restore", "ann@example.com")

	created := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, f := range []*models.BrandFact{
		{ID: "fact-restore-1", Statement: "NPS is 62", CreatedBy: new("u-restore-1"), CreatedByName: "Ann at the time"},
		{ID: "fact-restore-2", Statement: "We ship weekly", CreatedBy: new("u-deleted"), CreatedByName: "Bob"},
	} {
		f.Subject, f.Kind, f.CreatedAt, f.UpdatedAt = models.FactSubjectUs, models.FactKindDocumented, created, now
		if err := repo.RestoreFact(ctx, nil, f); err != nil {
			t.Fatal(err)
		}
	}
	kept, err := repo.GetFact(ctx, "fact-restore-1")
	if err != nil || kept.CreatedBy == nil || *kept.CreatedBy != "u-restore-1" || kept.CreatedByName != "Ann at the time" || !kept.CreatedAt.Equal(created) {
		t.Fatalf("restored = %+v, %v", kept, err)
	}
	orphan, err := repo.GetFact(ctx, "fact-restore-2")
	if err != nil || orphan.CreatedBy != nil || orphan.CreatedByName != "Bob" {
		t.Fatalf("restored with a deleted author = %+v, %v", orphan, err)
	}
	// A fact whose id is back already is not restored over.
	again := *kept
	again.Statement = "NPS is 70"
	if err := repo.RestoreFact(ctx, nil, &again); !errors.Is(err, repository.ErrFactChanged) {
		t.Fatalf("restore over a stored fact: err = %v, want ErrFactChanged", err)
	}
}

// On a full ledger, a restore over a stored id is still a change since, and
// a restore of a new one is the limit.
func TestBrandFacts_RestoreOnAFullLedger(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewBrandRepository(db)
	ctx := tenantctx.With(t.Context(), "tenant-facts-full")
	now := time.Now().UTC().Truncate(time.Microsecond)
	facts := make([]models.BrandFact, repository.MaxBrandFacts)
	for i := range facts {
		facts[i] = models.BrandFact{ID: fmt.Sprintf("fact-full-%d", i), Statement: fmt.Sprintf("Fact %d", i),
			Subject: models.FactSubjectUs, Kind: models.FactKindDocumented, CreatedAt: now, UpdatedAt: now}
	}
	if _, err := db.NewInsert().Model(&facts).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	again := facts[0]
	if err := repo.RestoreFact(ctx, nil, &again); !errors.Is(err, repository.ErrFactChanged) {
		t.Fatalf("restore over a stored fact: err = %v, want ErrFactChanged", err)
	}
	fresh := models.BrandFact{ID: "fact-full-new", Statement: "One more", Subject: models.FactSubjectUs,
		Kind: models.FactKindDocumented, CreatedAt: now, UpdatedAt: now}
	if err := repo.RestoreFact(ctx, nil, &fresh); !errors.Is(err, repository.ErrFactLimit) {
		t.Fatalf("restore a new fact: err = %v, want ErrFactLimit", err)
	}
}
