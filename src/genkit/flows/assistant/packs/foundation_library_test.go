package packs

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/brandfacts"
	"github.com/ogen-app/ogen/src/usecase/brandlib"
)

func TestLibrary_VoiceEdits(t *testing.T) {
	db, actions := undoStore(t)
	brand := repository.NewBrandRepository(db)
	d := Deps{Brand: brand, Actions: actions}
	lib := brandlib.New(brand)
	ctx, _ := turn(t, workspace, nil)

	plain, err := addVoice(ctx, d, lib, VoiceInput{Name: "Plain", IsDefault: true, Samples: []string{" We shipped. ", ""},
		Rules: VoiceRulesInput{Emoji: "never"}})
	if err != nil || plain.UndoID == "" || !slices.Equal(plain.Voice.Samples, []string{"We shipped."}) {
		t.Fatalf("added = %+v, %v", plain, err)
	}
	_, err = addVoice(ctx, d, lib, VoiceInput{Name: "Loud", Rules: VoiceRulesInput{Emoji: "always"}})
	refused(t, err)
	formal, err := addVoice(ctx, d, lib, VoiceInput{Name: "Formal"})
	if err != nil {
		t.Fatal(err)
	}

	// Without an id, the voice the screen shows is the one edited; only
	// the fields passed change.
	page, _ := assistant.NewViewTurnContext(tenantctx.With(context.WithValue(t.Context(), logging.UserIDKey, "u1"), "tenant-1"),
		viewcontext.ViewContext{Version: viewcontext.Version, Screen: "foundation.voice", Scope: workspace,
			Focus: &viewcontext.Ref{Kind: viewcontext.KindVoice, ID: plain.Voice.ID}}, nil)
	edited, err := updateVoice(page, d, lib, UpdateVoiceInput{AddSamples: []string{"Done is better."}, RemoveSamples: []string{"we shipped."},
		Rules: VoiceRulesInput{Length: "short"}, ChannelNotes: map[string]string{"LinkedIn": "No hashtags"}})
	if err != nil {
		t.Fatal(err)
	}
	v := edited.Voice
	if v.Name != "Plain" || !v.IsDefault || !slices.Equal(v.Samples, []string{"Done is better."}) ||
		v.Rules.Emoji != "never" || v.Rules.Length != "short" || v.ChannelNotes["linkedin"] != "No hashtags" {
		t.Fatalf("edited = %+v", v)
	}
	_, err = updateVoice(ctx, d, lib, UpdateVoiceInput{Name: "x"})
	refused(t, err)

	// Making Formal the default, then undoing it, gives the flag back.
	made, err := updateVoice(ctx, d, lib, UpdateVoiceInput{VoiceID: formal.Voice.ID, MakeDefault: true})
	if err != nil || !made.Voice.IsDefault {
		t.Fatalf("made default = %+v, %v", made, err)
	}
	svc := assistantundo.New(db, actions, nil).WithBrandLibrary(brand)
	if _, err := svc.Undo(ctx, made.UndoID, "u1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := brand.GetVoice(ctx, plain.Voice.ID); !got.IsDefault {
		t.Fatal("Plain didn't get the default back")
	}

	gone, err := removeVoice(ctx, d, lib, VoiceRef{VoiceID: formal.Voice.ID})
	if err != nil || gone.UndoID == "" {
		t.Fatalf("removed = %+v, %v", gone, err)
	}
	_, err = removeVoice(ctx, d, lib, VoiceRef{VoiceID: formal.Voice.ID})
	refused(t, err)
}

func TestLibrary_AudienceAndGuardrails(t *testing.T) {
	db, actions := undoStore(t)
	brand := repository.NewBrandRepository(db)
	d := Deps{Brand: brand, Actions: actions}
	lib := brandlib.New(brand)
	ctx, touched := turn(t, workspace, nil)

	a, err := addAudience(ctx, d, lib, AudienceInput{Name: "CTOs", Who: "Engineering leads"})
	if err != nil {
		t.Fatal(err)
	}
	edited, err := updateAudience(ctx, d, lib, UpdateAudienceInput{AudienceID: a.Audience.ID, AudienceInput: AudienceInput{BelievesWhen: "They see numbers"}})
	if err != nil || edited.Audience.Who != "Engineering leads" || edited.Audience.BelievesWhen != "They see numbers" {
		t.Fatalf("edited = %+v, %v", edited, err)
	}
	if _, err := removeAudience(ctx, d, lib, AudienceRef{AudienceID: a.Audience.ID}); err != nil {
		t.Fatal(err)
	}

	g, err := updateGuardrails(ctx, d, lib, GuardrailsInput{AddBannedWords: []string{"synergy", "Synergy"}, AddNeverClaim: []string{"Best in class"}})
	if err != nil || !slices.Equal(g.Guardrails.BannedWords, []string{"synergy"}) || g.UndoID == "" {
		t.Fatalf("guardrails = %+v, %v", g, err)
	}
	g, err = updateGuardrails(ctx, d, lib, GuardrailsInput{RemoveBannedWords: []string{"SYNERGY"}, Disclaimer: "Not financial advice."})
	if err != nil || len(g.Guardrails.BannedWords) != 0 || g.Guardrails.Disclaimer != "Not financial advice." {
		t.Fatalf("guardrails = %+v, %v", g, err)
	}
	// Nothing changes: no write, no undo.
	if same, err := updateGuardrails(ctx, d, lib, GuardrailsInput{AddNeverClaim: []string{"best in class"}}); err != nil || same.UndoID != "" {
		t.Fatalf("unchanged = %+v, %v", same, err)
	}
	_, err = updateGuardrails(ctx, d, lib, GuardrailsInput{RemoveNeverClaim: []string{"Best in class"}, ClearDisclaimer: true})
	refused(t, err)
	if !slices.ContainsFunc(touched(), func(t assistantkit.Touched) bool { return t.Kind == "guardrails" }) {
		t.Fatalf("touched = %v", touched())
	}
}

func TestEditBrand(t *testing.T) {
	db, actions := undoStore(t)
	brand := repository.NewBrandRepository(db)
	d := Deps{Brand: brand, Actions: actions, ProposalThreshold: 2}
	lib := brandlib.New(brand)
	facts := brandfacts.New(brand)
	ctx, _ := turn(t, workspace, nil)
	for _, m := range []any{
		&models.Account{ID: "acc-1", Email: "jane@example.com", Name: "Jane"},
		&models.User{ID: "u1", AccountID: "acc-1", TenantID: "tenant-1", Name: "Jane", Email: "jane@example.com", Role: models.RoleOwner},
	} {
		if _, err := db.NewInsert().Model(m).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	in := EditBrandInput{
		AddFacts:     []FactInput{{Statement: "Founded in 2019"}, {Statement: "Founded in 2019"}, {Statement: "Offices in Kyiv and Lisbon"}},
		AddAudiences: []AudienceInput{{Name: "CTOs"}},
		RemoveVoices: []string{"gone"},
	}
	if in.size() != 5 || !overN(d, in.size()) || in.summary() != "Add 3 facts, remove 1 voice, add 1 audience" {
		t.Fatalf("size = %d, summary = %q", in.size(), in.summary())
	}
	out, err := editBrand(ctx, d, facts, lib, in)
	if err != nil {
		t.Fatal(err)
	}
	// The repeated fact and the missing voice are reported; the rest is
	// made, each undoable on its own.
	if out.Done != 3 || len(out.UndoIDs) != 3 || len(out.Failed) != 2 {
		t.Fatalf("out = %+v", out)
	}
	if err := (EditBrandInput{}).precheck(); err == nil {
		t.Fatal("an empty edit passed")
	}
	refused(t, EditBrandInput{UpdateVoices: []UpdateVoiceInput{{Name: "x"}}}.precheck())
}
