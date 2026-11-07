package packs

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/brandfacts"
)

// memBrand is a fact ledger in memory.
type memBrand struct {
	repository.BrandRepository
	facts map[string]*models.BrandFact
}

func (b *memBrand) GetAll(context.Context) (*models.BrandData, error) {
	return &models.BrandData{Voices: []models.BrandVoice{{ID: "v1", Name: "Plain", WhenToUse: "always", IsDefault: true}}}, nil
}

func (b *memBrand) ListFacts(context.Context) ([]models.BrandFact, error) {
	var out []models.BrandFact
	for _, f := range b.facts {
		out = append(out, *f)
	}
	return out, nil
}

func (b *memBrand) GetFact(_ context.Context, id string) (*models.BrandFact, error) {
	if f, ok := b.facts[id]; ok {
		cp := *f
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (b *memBrand) CreateFact(_ context.Context, f *models.BrandFact) error {
	for _, x := range b.facts {
		if x.Statement == f.Statement {
			return repository.ErrFactDuplicate
		}
	}
	cp := *f
	b.facts[f.ID] = &cp
	return nil
}

func (b *memBrand) UpdateFact(_ context.Context, f *models.BrandFact) (*models.BrandFact, error) {
	before, ok := b.facts[f.ID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := *f
	b.facts[f.ID] = &cp
	return before, nil
}

func (b *memBrand) DeleteFact(_ context.Context, id string) (*models.BrandFact, error) {
	f, ok := b.facts[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	delete(b.facts, id)
	return f, nil
}

func TestFacts_EditAndUndo(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{}}
	actions := &undoActions{}
	d := Deps{Brand: brand, Actions: actions}
	facts := brandfacts.New(brand)
	ctx, touched := turn(t, workspace, nil)

	added, err := addFact(ctx, d, facts, FactInput{Statement: "We serve 4,000 customers", Kind: "measured", ExpiresAt: "2027-06-30"})
	if err != nil {
		t.Fatal(err)
	}
	f := added.Fact
	if f.Subject != models.FactSubjectUs || f.ExpiresAt.String() != "2027-06-30" || added.UndoID == "" || *f.CreatedBy != "u1" {
		t.Fatalf("added = %+v", added)
	}
	_, err = addFact(ctx, d, facts, FactInput{Statement: "We serve 4,000 customers"})
	refused(t, err)
	_, err = addFact(ctx, d, facts, FactInput{Statement: "   "})
	refused(t, err)

	edited, err := updateFact(ctx, d, facts, UpdateFactInput{FactID: f.ID, FactInput: FactInput{Statement: "We serve 4,200 customers"}, ClearExpiry: true})
	if err != nil || edited.Fact.Statement != "We serve 4,200 customers" || edited.Fact.ExpiresAt != nil || edited.Fact.Kind != models.FactKindMeasured {
		t.Fatalf("edited = %+v, %v; want the other fields kept", edited, err)
	}

	// Undo the edit: the statement and its expiry come back.
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	svc := assistantundo.New(db, actions, nil).WithBrand(brand)
	if _, err := svc.Undo(ctx, edited.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := brand.facts[f.ID]; got.Statement != "We serve 4,000 customers" || got.ExpiresAt.String() != "2027-06-30" {
		t.Fatalf("after undo = %+v", got)
	}

	removed, err := removeFact(ctx, d, facts, RemoveFactInput{FactID: f.ID})
	if err != nil || len(brand.facts) != 0 {
		t.Fatalf("removed = %+v, %v", removed, err)
	}
	// Undo the removal: it is back.
	if _, err := svc.Undo(ctx, removed.UndoID, "u1"); err != nil || brand.facts[f.ID] == nil {
		t.Fatalf("restore: %v, facts = %v", err, brand.facts)
	}
	// Undo the add: gone again (the fact matches what the add left once restored).
	if _, err := svc.Undo(ctx, added.UndoID, "u1"); err != nil || len(brand.facts) != 0 {
		t.Fatalf("undo add: %v, facts = %d", err, len(brand.facts))
	}
	if !slices.Equal(touched(), []assistantkit.Touched{{Kind: "fact", ID: f.ID}}) {
		t.Fatalf("touched = %v", touched())
	}

	out, err := getBrand(ctx, d, facts)
	if err != nil || len(out.Voices) != 1 || out.Facts == nil {
		t.Fatalf("brand = %+v, %v", out, err)
	}
}

// undoActions keeps logged changes for the undo service.
type undoActions struct {
	repository.AssistantActionRepository
	rows map[string]*models.AssistantAction
}

func (u *undoActions) Create(_ context.Context, a *models.AssistantAction) error {
	if u.rows == nil {
		u.rows = map[string]*models.AssistantAction{}
	}
	u.rows[a.ID] = a
	return nil
}

func (u *undoActions) GetByID(_ context.Context, id string) (*models.AssistantAction, error) {
	if a, ok := u.rows[id]; ok {
		cp := *a
		return &cp, nil
	}
	return nil, sql.ErrNoRows
}

func (u *undoActions) MarkUndone(context.Context, bun.IDB, string, string, time.Time) (bool, error) {
	return true, nil
}
