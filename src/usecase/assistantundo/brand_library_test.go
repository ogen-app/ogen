package assistantundo_test

import (
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/brandlib"
)

func libFixture(t *testing.T, f *fixture) (repository.BrandRepository, *brandlib.Service, *assistantundo.Service) {
	t.Helper()
	brand := repository.NewBrandRepository(f.db)
	return brand, brandlib.New(brand), assistantundo.New(f.db, f.actions, f.campaigns).WithBrandLibrary(brand)
}

func logBrand(t *testing.T, f *fixture, kind models.AssistantActionKind, id string, before, after models.JSONMap) string {
	t.Helper()
	undo, err := assistantundo.Record(f.ctx, f.actions, &models.AssistantAction{
		ScopeKind: models.AssistantScopeWorkspace, Kind: kind, EntityID: id, Before: before, After: after, CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return undo
}

func voice(t *testing.T, f *fixture, brand repository.BrandRepository, id string) *models.BrandVoice {
	t.Helper()
	v, err := brand.GetVoice(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestUndo_VoiceMadeDefaultHandsTheFlagBack(t *testing.T) {
	f := newFixture(t)
	brand, lib, svc := libFixture(t, f)
	v1, err := lib.CreateVoice(f.ctx, &models.BrandVoice{Name: "Plain", IsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := lib.CreateVoice(f.ctx, &models.BrandVoice{Name: "Formal"})
	if err != nil {
		t.Fatal(err)
	}
	cur := voice(t, f, brand, v2.ID)
	edit := *cur
	edit.IsDefault, edit.WhenToUse = true, "LinkedIn"
	before, after, err := lib.UpdateVoiceIf(f.ctx, &edit, cur)
	if err != nil {
		t.Fatal(err)
	}
	if voice(t, f, brand, v1.ID).IsDefault {
		t.Fatal("the old default kept the flag")
	}
	id := logBrand(t, f, models.AssistantActionBrandVoice, v2.ID,
		assistantundo.BrandVoice(before, nil, ""), assistantundo.BrandVoice(after, nil, v1.ID))
	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if got := voice(t, f, brand, v2.ID); got.IsDefault || got.WhenToUse != "" {
		t.Fatalf("v2 = %+v, want it as it was", got)
	}
	if !voice(t, f, brand, v1.ID).IsDefault {
		t.Fatal("v1 didn't get the default back")
	}
}

func TestUndo_RemovedVoiceComesBackBound(t *testing.T) {
	f := newFixture(t)
	brand, lib, svc := libFixture(t, f)
	v, err := lib.CreateVoice(f.ctx, &models.BrandVoice{Name: "Plain", IsDefault: true, Samples: models.StringSlice{"We shipped."}})
	if err != nil {
		t.Fatal(err)
	}
	f.seedCampaign(t, func(c *models.Campaign) { c.BrandVoiceID = &v.ID })

	gone, bound, err := lib.RemoveVoice(f.ctx, v.ID)
	if err != nil || gone == nil || len(bound.Campaigns) != 1 {
		t.Fatalf("removed = %+v, %+v, %v", gone, bound, err)
	}
	if f.load(t).BrandVoiceID != nil {
		t.Fatal("the campaign kept the removed voice")
	}
	id := logBrand(t, f, models.AssistantActionBrandVoice, v.ID,
		assistantundo.BrandVoice(gone, &bound, ""), assistantundo.BrandVoice(nil, nil, ""))
	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	back := voice(t, f, brand, v.ID)
	if back == nil || !back.IsDefault || len(back.Samples) != 1 {
		t.Fatalf("restored = %+v", back)
	}
	if c := f.load(t); c.BrandVoiceID == nil || *c.BrandVoiceID != v.ID {
		t.Fatalf("campaign voice = %v, want it bound again", c.BrandVoiceID)
	}
}

func TestUndo_AddedAudienceInUseIsKept(t *testing.T) {
	f := newFixture(t)
	brand, lib, svc := libFixture(t, f)
	a, err := lib.CreateAudience(f.ctx, &models.BrandAudience{Name: "CTOs", Who: "Engineering leads"})
	if err != nil {
		t.Fatal(err)
	}
	id := logBrand(t, f, models.AssistantActionBrandAudience, a.ID,
		assistantundo.BrandAudience(nil, nil), assistantundo.BrandAudience(a, nil))
	// Set on a campaign since: undoing the add would unset it.
	f.seedCampaign(t, func(c *models.Campaign) { c.BrandAudienceID = &a.ID })
	if _, err := svc.Undo(f.ctx, id, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("err = %v, want ErrChangedSince", err)
	}
	if got, _ := brand.GetAudience(f.ctx, a.ID); got == nil {
		t.Fatal("the audience was removed")
	}
}

func TestUndo_EditedAudienceIsRestoredUnlessChangedSince(t *testing.T) {
	f := newFixture(t)
	brand, lib, svc := libFixture(t, f)
	a, err := lib.CreateAudience(f.ctx, &models.BrandAudience{Name: "CTOs", Who: "Engineering leads"})
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := brand.GetAudience(f.ctx, a.ID)
	edit := *cur
	edit.Who = "Heads of platform"
	before, after, err := lib.UpdateAudienceIf(f.ctx, &edit, cur)
	if err != nil {
		t.Fatal(err)
	}
	id := logBrand(t, f, models.AssistantActionBrandAudience, a.ID,
		assistantundo.BrandAudience(before, nil), assistantundo.BrandAudience(after, nil))

	// A stale write is refused.
	stale := *cur
	stale.Name = "Other"
	if _, _, err := lib.UpdateAudienceIf(f.ctx, &stale, cur); !errors.Is(err, repository.ErrBrandChanged) {
		t.Fatalf("stale write: err = %v, want ErrBrandChanged", err)
	}
	if _, err := svc.Undo(f.ctx, id, "user-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := brand.GetAudience(f.ctx, a.ID); got.Who != "Engineering leads" {
		t.Fatalf("who = %q, want it restored", got.Who)
	}
}

func TestUndo_Guardrails(t *testing.T) {
	f := newFixture(t)
	brand, lib, svc := libFixture(t, f)
	_, first, err := lib.SaveGuardrailsIf(f.ctx, &models.BrandGuardrails{BannedWords: models.StringSlice{"synergy"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	added := logBrand(t, f, models.AssistantActionBrandGuardrails, "guardrails",
		assistantundo.BrandGuardrails(nil), assistantundo.BrandGuardrails(first))
	cur, _ := brand.GetGuardrails(f.ctx)
	before, after, err := lib.SaveGuardrailsIf(f.ctx, &models.BrandGuardrails{BannedWords: models.StringSlice{"synergy", "leverage"}}, cur)
	if err != nil {
		t.Fatal(err)
	}
	edited := logBrand(t, f, models.AssistantActionBrandGuardrails, "guardrails",
		assistantundo.BrandGuardrails(before), assistantundo.BrandGuardrails(after))

	// The first change no longer holds: the rules were changed since.
	if _, err := svc.Undo(f.ctx, added, "user-1"); !errors.Is(err, assistantundo.ErrChangedSince) {
		t.Fatalf("err = %v, want ErrChangedSince", err)
	}
	if _, err := svc.Undo(f.ctx, edited, "user-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := brand.GetGuardrails(f.ctx); len(got.BannedWords) != 1 {
		t.Fatalf("banned = %v, want the edit undone", got.BannedWords)
	}
	// Now the first change holds again: undoing it removes the rules.
	if _, err := svc.Undo(f.ctx, added, "user-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := brand.GetGuardrails(f.ctx); got != nil {
		t.Fatalf("guardrails = %+v, want none", got)
	}
}
