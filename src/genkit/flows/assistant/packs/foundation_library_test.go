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
	if want := []FieldChange{{Field: "length", After: "short"}, {Field: "samples", Added: []string{"Done is better."}, Removed: []string{"We shipped."}},
		{Field: "channelNotes.linkedin", After: "No hashtags"}}; !slices.EqualFunc(edited.Changes, want, sameChange) {
		t.Fatalf("changes = %+v, want %+v", edited.Changes, want)
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
	if want := []FieldChange{{Field: "believesWhen", After: "They see numbers"}}; !slices.EqualFunc(edited.Changes, want, sameChange) {
		t.Fatalf("changes = %+v", edited.Changes)
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
	// Only the change, not the never-claim line already there.
	if want := []FieldChange{{Field: "bannedWords", Removed: []string{"synergy"}}, {Field: "disclaimer", After: "Not financial advice."}}; !slices.EqualFunc(g.Changes, want, sameChange) {
		t.Fatalf("changes = %+v", g.Changes)
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
	seedOwner(t, db)

	in := EditBrandInput{
		AddFacts:     []FactInput{{Statement: "Founded in 2019"}, {Statement: "Founded in 2019"}, {Statement: "Offices in Kyiv and Lisbon"}},
		AddAudiences: []AudienceInput{{Name: "CTOs"}},
		RemoveVoices: []string{"gone"},
		Kept:         []KeptFact{{FactID: "missing", Reason: "dropped"}},
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
	if want := (BrandEditCounts{Facts: EditCounts{Added: 2}, Audiences: EditCounts{Added: 1}}); out.Made != want || out.Kept != nil {
		t.Fatalf("made = %+v, kept = %+v", out.Made, out.Kept)
	}

	// Removing facts counts as removed; a kept fact is reported with its
	// statement and isn't touched.
	all, _ := facts.Current(ctx)
	removed, err := editBrand(ctx, d, facts, lib, EditBrandInput{RemoveFacts: []string{all[0].ID},
		Kept: []KeptFact{{FactID: all[1].ID, Reason: "a 2026 document also states it"}}})
	if err != nil || removed.Made != (BrandEditCounts{Facts: EditCounts{Removed: 1}}) ||
		len(removed.Kept) != 1 || removed.Kept[0].Statement != all[1].Statement || removed.Kept[0].Reason != "a 2026 document also states it" {
		t.Fatalf("removed = %+v, %v", removed, err)
	}
	if left, _ := facts.Current(ctx); len(left) != 1 || left[0].ID != all[1].ID {
		t.Fatalf("left = %+v", left)
	}
	if err := (EditBrandInput{}).precheck(); err == nil {
		t.Fatal("an empty edit passed")
	}
	refused(t, EditBrandInput{UpdateVoices: []UpdateVoiceInput{{Name: "x"}}}.precheck())
	refused(t, EditBrandInput{RemoveFacts: []string{"f1"}, Kept: []KeptFact{{FactID: " f1", Reason: "still true"}}}.precheck())
}

// The bulk edit offer's card has a row per kind of change naming its first
// few items, then the facts kept and why; one kind of change names the
// button.
func TestEditBrandCard(t *testing.T) {
	db, actions := undoStore(t)
	brand := repository.NewBrandRepository(db)
	d := Deps{Brand: brand, Actions: actions}
	facts := brandfacts.New(brand)
	ctx, _ := turn(t, workspace, nil)
	seedOwner(t, db)
	var ids []string
	for _, s := range []string{"Price 1", "Price 2", "Price 3", "Price 4", "Price 5", "Price 6", "Price 7"} {
		f, err := addFact(ctx, d, facts, FactInput{Statement: s})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, f.Fact.ID)
	}
	voice, err := addVoice(ctx, d, brandlib.New(brand), VoiceInput{Name: "Formal"})
	if err != nil {
		t.Fatal(err)
	}

	card := EditBrandInput{RemoveFacts: append(slices.Clone(ids[:5]), "gone"), Kept: []KeptFact{{FactID: ids[6], Reason: "a 2026 document states it"}}}.card(ctx, d)
	if card.Verb != "Remove 6 facts" || card.Action != assistantkit.CardActionRemove || card.Item != assistantkit.CardItemFact || card.Count != 6 || len(card.Rows) != 2 {
		t.Fatalf("card = %+v", card)
	}
	// The row names the first five; the missing sixth is counted, not named.
	if r := card.Rows[0]; r.Kind != assistantkit.CardRowChange || r.Value != "Remove 6 facts" || r.Count != 6 ||
		!slices.Equal(r.Names, []string{"Price 1", "Price 2", "Price 3", "Price 4", "Price 5"}) {
		t.Fatalf("change row = %+v", r)
	}
	if r := card.Rows[1]; r.Kind != assistantkit.CardRowKept || r.Value != "Price 7" || r.Note != "a 2026 document states it" || r.Item != assistantkit.CardItemFact {
		t.Fatalf("kept row = %+v", r)
	}

	mixed := EditBrandInput{AddFacts: []FactInput{{Statement: " Founded in 2019 "}}, UpdateFacts: []UpdateFactInput{{FactID: ids[0]}},
		RemoveVoices: []string{voice.Voice.ID}, AddAudiences: []AudienceInput{{Name: "CTOs"}, {Name: "CFOs"}}}.card(ctx, d)
	if mixed.Verb != "Make 5 brand changes" || mixed.Action != assistantkit.CardActionEditBrand || mixed.Item != "" || mixed.Count != 5 || len(mixed.Rows) != 4 {
		t.Fatalf("mixed = %+v", mixed)
	}
	for i, want := range []struct {
		value, item, action string
		names               []string
	}{
		{"Add 1 fact", assistantkit.CardItemFact, assistantkit.CardActionAdd, []string{"Founded in 2019"}},
		{"Update 1 fact", assistantkit.CardItemFact, assistantkit.CardActionUpdate, []string{"Price 1"}},
		{"Remove 1 voice", assistantkit.CardItemVoice, assistantkit.CardActionRemove, []string{"Formal"}},
		{"Add 2 audiences", assistantkit.CardItemAudience, assistantkit.CardActionAdd, []string{"CTOs", "CFOs"}},
	} {
		if r := mixed.Rows[i]; r.Value != want.value || r.Item != want.item || r.Action != want.action || !slices.Equal(r.Names, want.names) {
			t.Fatalf("row %d = %+v, want %+v", i, r, want)
		}
	}
	if card := (EditBrandInput{}).card(ctx, d); card != nil {
		t.Fatalf("empty card = %+v", card)
	}
}

// A bulk edit stopped by an error retries the changes from the one it
// failed on, and reports the refused ones without retrying them.
func TestBrandRetry(t *testing.T) {
	in := EditBrandInput{
		AddFacts:     []FactInput{{Statement: "Founded in 2019"}},
		RemoveVoices: []string{"v1"},
		AddAudiences: []AudienceInput{{Name: "CTOs"}},
	}
	edits := in.edits(Deps{}, nil, nil)
	out := &EditBrandOutput{Done: 1, Failed: []BrandEditFailure{{What: "x", Reason: "already there"}}, Stopped: "remove voice v1 failed: timeout", left: edits[1:]}
	in.Kept = []KeptFact{{FactID: "f1", Reason: "still true"}}
	retry, notMade, made, ok := brandRetry(in, out)
	if !ok || len(retry.Kept) != 1 || len(retry.AddFacts) != 0 || !slices.Equal(retry.RemoveVoices, []string{"v1"}) || len(retry.AddAudiences) != 1 || len(notMade) != 3 || made != 1 {
		t.Fatalf("retry = %+v, notMade = %+v", retry, notMade)
	}
	if _, _, _, ok := brandRetry(in, &EditBrandOutput{Done: 3}); ok {
		t.Fatal("a whole edit offered a retry")
	}
}

func sameChange(a, b FieldChange) bool {
	return a.Field == b.Field && a.Before == b.Before && a.After == b.After && slices.Equal(a.Added, b.Added) && slices.Equal(a.Removed, b.Removed)
}

// A voice's changes name each field that moved, with its old and new value,
// and the lines a list gained and lost; nothing for what stayed.
func TestVoiceChanges(t *testing.T) {
	before := &models.BrandVoice{Name: "Warm host", Rules: models.VoiceRules{Formality: "neutral"}, Samples: models.StringSlice{"A", "B"},
		ChannelNotes: models.StringMap{"instagram": "emoji ok", "x": "short"}}
	after := &models.BrandVoice{Name: "Warm host", IsDefault: true, Rules: models.VoiceRules{Formality: "casual"}, Samples: models.StringSlice{"a", "C"},
		ChannelNotes: models.StringMap{"instagram": "emoji ok", "linkedin": "no emoji"}}
	want := []FieldChange{
		{Field: "isDefault", Before: "false", After: "true"},
		{Field: "formality", Before: "neutral", After: "casual"},
		{Field: "samples", Added: []string{"C"}, Removed: []string{"B"}},
		{Field: "channelNotes.linkedin", After: "no emoji"},
		{Field: "channelNotes.x", Before: "short"},
	}
	if got := voiceChanges(before, after); !slices.EqualFunc(got, want, sameChange) {
		t.Fatalf("changes = %+v\nwant %+v", got, want)
	}
	if got := guardrailsChanges(nil, &models.BrandGuardrails{BannedWords: models.StringSlice{"luxury"}}); !slices.EqualFunc(got,
		[]FieldChange{{Field: "bannedWords", Added: []string{"luxury"}}}, sameChange) {
		t.Fatalf("guardrails from none = %+v", got)
	}
}
