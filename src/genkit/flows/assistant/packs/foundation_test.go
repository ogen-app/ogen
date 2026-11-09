package packs

import (
	"context"
	"database/sql"
	"errors"
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
	// data is what GetAll returns besides the facts; nil is one voice.
	data *models.BrandData
	// beforeWrite runs as a conditional update starts, to land a concurrent
	// edit between the tool's read and its write.
	beforeWrite func()
}

// GetAll returns data, or one default voice, with the ledger's facts.
func (b *memBrand) GetAll(ctx context.Context) (*models.BrandData, error) {
	out := models.BrandData{Voices: []models.BrandVoice{{ID: "v1", Name: "Plain", WhenToUse: "always", IsDefault: true}}}
	if b.data != nil {
		out = *b.data
	}
	facts, err := b.ListFacts(ctx)
	out.Facts = facts
	return &out, err
}

func (b *memBrand) ListFacts(context.Context) ([]models.BrandFact, error) {
	var out []models.BrandFact
	for _, f := range b.facts {
		out = append(out, *f)
	}
	return out, nil
}

// GetFact returns nil for a missing fact, as the repository does.
func (b *memBrand) GetFact(_ context.Context, id string) (*models.BrandFact, error) {
	if f, ok := b.facts[id]; ok {
		cp := *f
		return &cp, nil
	}
	return nil, nil
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

// DeleteFact returns nil for a missing fact, as the repository does.
func (b *memBrand) DeleteFact(_ context.Context, id string) (*models.BrandFact, error) {
	f, ok := b.facts[id]
	if !ok {
		return nil, nil
	}
	delete(b.facts, id)
	return f, nil
}

func (b *memBrand) UpdateFactIf(ctx context.Context, _ bun.IDB, f, was *models.BrandFact) (*models.BrandFact, error) {
	if b.beforeWrite != nil {
		b.beforeWrite()
	}
	cur, ok := b.facts[f.ID]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if !sameFact(cur, was) {
		return nil, repository.ErrFactChanged
	}
	return b.UpdateFact(ctx, f)
}

func (b *memBrand) DeleteFactIf(ctx context.Context, _ bun.IDB, id string, was *models.BrandFact) (*models.BrandFact, error) {
	cur, ok := b.facts[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	if !sameFact(cur, was) {
		return nil, repository.ErrFactChanged
	}
	return b.DeleteFact(ctx, id)
}

func (b *memBrand) RestoreFact(ctx context.Context, _ bun.IDB, f *models.BrandFact) error {
	if _, ok := b.facts[f.ID]; ok {
		return repository.ErrFactChanged
	}
	return b.CreateFact(ctx, f)
}

// sameFact compares the editable fields, as the repository does.
func sameFact(a, b *models.BrandFact) bool {
	day := func(d *models.CalendarDate) string {
		if d == nil {
			return ""
		}
		return d.String()
	}
	return a.Statement == b.Statement && a.Subject == b.Subject && a.Kind == b.Kind && a.Source == b.Source &&
		day(a.AddedAt) == day(b.AddedAt) && day(a.CheckedAt) == day(b.CheckedAt) && day(a.ExpiresAt) == day(b.ExpiresAt)
}

func TestFacts_EditAndUndo(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{}}
	db, actions := undoStore(t)
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

	// Undo the edit: the statement and its expiry come back, once.
	svc := assistantundo.New(db, actions, nil).WithBrand(brand)
	if _, err := svc.Undo(ctx, edited.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := brand.facts[f.ID]; got.Statement != "We serve 4,000 customers" || got.ExpiresAt.String() != "2027-06-30" {
		t.Fatalf("after undo = %+v", got)
	}
	if by := undone(t, ctx, actions, edited.UndoID); by != "u1" {
		t.Fatalf("undone by %q, want u1", by)
	}
	if _, err := svc.Undo(ctx, edited.UndoID, "u1"); !errors.Is(err, assistantundo.ErrAlreadyUndone) {
		t.Fatalf("second undo: err = %v, want ErrAlreadyUndone", err)
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

// undoStore is a database with turn's tenant and the action store on it, so
// an undo's record commits or rolls back with its restore.
func undoStore(t *testing.T) (*bun.DB, repository.AssistantActionRepository) {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	if _, err := db.NewInsert().Model(&models.Tenant{ID: "tenant-1", Name: "tenant-1", Slug: "tenant-1",
		TierID: models.DefaultTierID, CreatedAt: now, UpdatedAt: now}).Exec(t.Context()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return db, repository.NewAssistantActionRepository(db)
}

// undone reports who undid the action, or "" while it is still undoable.
func undone(t *testing.T, ctx context.Context, actions repository.AssistantActionRepository, id string) string {
	t.Helper()
	a, err := actions.GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.UndoneAt == nil {
		return ""
	}
	return a.UndoneBy
}

// A missing fact is refused, not dereferenced.
func TestFacts_MissingFactRefused(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{}}
	d := Deps{Brand: brand, Actions: &memActions{}}
	facts := brandfacts.New(brand)
	ctx, _ := turn(t, workspace, nil)

	_, err := updateFact(ctx, d, facts, UpdateFactInput{FactID: "nope", FactInput: FactInput{Statement: "x"}})
	refused(t, err)
	_, err = removeFact(ctx, d, facts, RemoveFactInput{FactID: "nope"})
	refused(t, err)
}

// A partial update merged onto a stale read is refused, so it can't put back
// a statement corrected since.
func TestFacts_StaleUpdateRefused(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{}}
	d := Deps{Brand: brand, Actions: &memActions{}}
	facts := brandfacts.New(brand)
	ctx, _ := turn(t, workspace, nil)
	added, err := addFact(ctx, d, facts, FactInput{Statement: "We serve 4,000 customers"})
	if err != nil {
		t.Fatal(err)
	}
	id := added.Fact.ID
	brand.beforeWrite = func() { brand.facts[id].Statement = "We serve 4,200 customers" }

	_, err = updateFact(ctx, d, facts, UpdateFactInput{FactID: id, FactInput: FactInput{CheckedAt: "2026-11-01"}})
	refused(t, err)
	if got := brand.facts[id]; got.Statement != "We serve 4,200 customers" || got.CheckedAt != nil {
		t.Fatalf("fact = %+v, want the concurrent correction kept", got)
	}
}

// Undoing an add or a correction after a later edit is refused, and the
// later edit stays.
func TestFacts_UndoRefusedAfterLaterEdit(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{}}
	db, actions := undoStore(t)
	d := Deps{Brand: brand, Actions: actions}
	facts := brandfacts.New(brand)
	ctx, _ := turn(t, workspace, nil)
	svc := assistantundo.New(db, actions, nil).WithBrand(brand)

	added, err := addFact(ctx, d, facts, FactInput{Statement: "We serve 4,000 customers"})
	if err != nil {
		t.Fatal(err)
	}
	id := added.Fact.ID
	edited, err := updateFact(ctx, d, facts, UpdateFactInput{FactID: id, FactInput: FactInput{Statement: "We serve 4,100 customers"}})
	if err != nil {
		t.Fatal(err)
	}
	// A later edit, made outside the assistant.
	brand.facts[id].Statement = "We serve 4,200 customers"

	for name, undoID := range map[string]string{"add": added.UndoID, "update": edited.UndoID} {
		if _, err := svc.Undo(ctx, undoID, "u1"); !errors.Is(err, assistantundo.ErrChangedSince) {
			t.Fatalf("undo %s: err = %v, want ErrChangedSince", name, err)
		}
		if got := brand.facts[id]; got == nil || got.Statement != "We serve 4,200 customers" {
			t.Fatalf("undo %s: fact = %+v, want the later edit kept", name, got)
		}
		if by := undone(t, ctx, actions, undoID); by != "" {
			t.Fatalf("undo %s: a refused undo was recorded by %q", name, by)
		}
	}
}

// Undoing a removal puts the fact back with its author and creation time,
// not the remover's.
func TestFacts_UndoRemoveKeepsAuthor(t *testing.T) {
	created := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	brand := &memBrand{facts: map[string]*models.BrandFact{
		"f1": {ID: "f1", Statement: "NPS is 62", Subject: models.FactSubjectUs, Kind: models.FactKindMeasured,
			CreatedBy: new("u2"), CreatedByName: "Ann", CreatedAt: created, UpdatedAt: created},
	}}
	db, actions := undoStore(t)
	d := Deps{Brand: brand, Actions: actions}
	ctx, _ := turn(t, workspace, nil)

	removed, err := removeFact(ctx, d, brandfacts.New(brand), RemoveFactInput{FactID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assistantundo.New(db, actions, nil).WithBrand(brand).Undo(ctx, removed.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	got := brand.facts["f1"]
	if got == nil || got.CreatedBy == nil || *got.CreatedBy != "u2" || got.CreatedByName != "Ann" || !got.CreatedAt.Equal(created) {
		t.Fatalf("restored = %+v, want u2/Ann at %v", got, created)
	}
}

func TestGetBrand_FullRead(t *testing.T) {
	brand := &memBrand{facts: map[string]*models.BrandFact{
		"f1": {ID: "f1", Statement: "We serve 4,000 customers", ExpiresAt: new(models.CalendarDateOf(time.Now().AddDate(0, 0, -1)))},
		"f2": {ID: "f2", Statement: "Founded in 2019"},
	}, data: &models.BrandData{
		Voices: []models.BrandVoice{{ID: "v1", Name: "Formal", WhenToUse: "LinkedIn", Summary: "Measured, plain.",
			Rules: models.VoiceRules{Formality: "formal"}, Samples: models.StringSlice{"We shipped it."},
			ChannelNotes: models.StringMap{"linkedin": "No emoji"}}},
		Look: &models.BrandLook{Logos: models.BrandLogos{{Job: "profile", URL: "https://x/logo.png"}},
			Palette: models.BrandPalette{{Role: "primary", Hex: "#112233"}}, ReferenceImages: models.StringSlice{"https://x/a.png"}},
		Templates: []models.BrandTemplate{{ID: "t1", Name: "Quote card", Role: "foreground",
			Ratios: models.TemplateRatios{{Ratio: "1:1", URL: "https://x/t.png"}}}},
	}}
	out, err := getBrand(t.Context(), Deps{Brand: brand}, brandfacts.New(brand))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Facts) != 1 || out.Facts[0].ID != "f2" || len(out.ExpiredFacts) != 1 || out.ExpiredFacts[0].ID != "f1" {
		t.Fatalf("facts = %+v, expired = %+v", out.Facts, out.ExpiredFacts)
	}
	v := out.Voices[0]
	if v.Summary != "Measured, plain." || v.Rules.Formality != "formal" || len(v.Samples) != 1 || v.ChannelNotes["linkedin"] != "No emoji" {
		t.Fatalf("voice = %+v", v)
	}
	if out.Look == nil || !slices.Equal(out.Look.Logos, []string{"profile"}) || out.Look.Palette[0].Hex != "#112233" || out.Look.ReferenceImages != 1 {
		t.Fatalf("look = %+v", out.Look)
	}
	if len(out.Templates) != 1 || !slices.Equal(out.Templates[0].Ratios, []string{"1:1"}) || out.Templates[0].Platforms == nil {
		t.Fatalf("templates = %+v", out.Templates)
	}
}
