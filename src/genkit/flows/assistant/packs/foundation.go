package packs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/brandfacts"
	"github.com/ogen-app/ogen/src/usecase/brandlib"
)

const foundationPackName = "foundation"

// foundationPack reads the brand foundation — voices, audiences,
// guardrails, facts — and keeps the fact ledger, each change undoable.
func foundationPack(d Deps) assistant.Pack {
	facts := brandfacts.New(d.Brand)
	lib := brandlib.New(d.Brand)
	return assistant.Pack{
		Name:    foundationPackName,
		Summary: "read the brand foundation (voices, audiences, guardrails, facts, look, templates); add, edit or remove voices, audiences and facts; edit the guardrails",
		Screens: []viewcontext.Screen{
			"foundation", "foundation.voices", "foundation.voice", "foundation.audiences", "foundation.audience",
			"foundation.facts", "foundation.guardrails", "foundation.look", "foundation.questions",
			"foundation.series", "foundation.series_item", "foundation.templates",
		},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			get := genkit.DefineTool(g, "getBrand",
				"Returns the brand foundation: its voices (when to use each, our summary, rules, samples, per-channel notes), audiences, guardrails (may and never claim, banned words), the facts still current and those expired, the look (logo jobs, palette, typefaces) and the templates.",
				assistantkit.Wrap(func(ctx context.Context, _ struct{}) (*BrandOut, error) { return getBrand(ctx, d, facts) }), ai.WithStrictSchema(false))
			add := genkit.DefineTool(g, "addFact",
				"Adds a fact to the ledger: a claim posts may make, with its kind and source. Returns an undo id.",
				assistantkit.Wrap(func(ctx context.Context, in FactInput) (*FactOut, error) { return addFact(ctx, d, facts, in) }), ai.WithStrictSchema(false))
			edit := genkit.DefineTool(g, "updateFact",
				"Corrects a fact in the ledger: only the fields passed change. Returns an undo id.",
				assistantkit.Wrap(func(ctx context.Context, in UpdateFactInput) (*FactOut, error) { return updateFact(ctx, d, facts, in) }), ai.WithStrictSchema(false))
			remove := genkit.DefineTool(g, "removeFact",
				"Removes a fact from the ledger, so no post claims it any more. Returns an undo id.",
				assistantkit.Wrap(func(ctx context.Context, in RemoveFactInput) (*FactOut, error) { return removeFact(ctx, d, facts, in) }), ai.WithStrictSchema(false))
			lspecs := librarySpecs(g, d, lib)
			return append([]assistantkit.Spec{
				spec(foundationPackName, get, assistantkit.TierRead,
					`Call this when the user asks about their brand — "what's our voice for LinkedIn?", "summarise our formal voice", "which facts have expired?", "what colours do we use?" — or before editing it.`),
				spec(foundationPackName, add, assistantkit.TierReversible,
					`Call this when the user gives a fact to remember — "we have 4,000 customers", "our NPS is 62 as of March". kind is measured, documented (default), commitment or judgement; subject is us (default), problem or opportunity. Pass expiresAt (YYYY-MM-DD) for anything that goes stale. It can be undone.`),
				spec(foundationPackName, edit, assistantkit.TierReversible,
					`Call this when the user corrects a fact or confirms it is still true (checkedAt today). To renew an expired fact, pass checkedAt today and a new expiresAt, or clearExpiry when it no longer goes stale. Pass factId from getBrand. It can be undone.`),
				spec(foundationPackName, remove, assistantkit.TierReversible,
					`Call this when the user says a fact is no longer true or shouldn't be claimed. It can be undone.`),
			}, lspecs...), nil
		},
	}
}

// BrandOut is the foundation.
type BrandOut struct {
	Voices     []BrandVoiceOut         `json:"voices"`
	Audiences  []BrandAudienceOut      `json:"audiences"`
	Guardrails *models.BrandGuardrails `json:"guardrails,omitempty"`
	Facts      []models.BrandFact      `json:"facts"`
	// ExpiredFacts are facts past their expiry: kept in the ledger but not
	// claimed, until renewed with updateFact.
	ExpiredFacts []models.BrandFact `json:"expiredFacts"`
	Look         *BrandLookOut      `json:"look,omitempty"`
	Templates    []BrandTemplateOut `json:"templates"`
}

// BrandVoiceOut is a voice: when to use it, our summary of its samples, its
// rules, samples and per-channel notes.
type BrandVoiceOut struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	WhenToUse    string            `json:"whenToUse"`
	IsDefault    bool              `json:"isDefault,omitzero"`
	Summary      string            `json:"summary,omitempty"`
	Rules        models.VoiceRules `json:"rules"`
	Samples      []string          `json:"samples"`
	ChannelNotes map[string]string `json:"channelNotes,omitempty"`
}

// BrandAudienceOut is an audience.
type BrandAudienceOut struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Who             string `json:"who"`
	ReadsOn         string `json:"readsOn,omitempty"`
	ScrollsPastWhen string `json:"scrollsPastWhen,omitempty"`
	BelievesWhen    string `json:"believesWhen,omitempty"`
}

// BrandLookOut is the look: the logos' jobs, the palette and the type,
// without the images.
type BrandLookOut struct {
	Logos           []string            `json:"logos"`
	Palette         []models.BrandColor `json:"palette"`
	Typefaces       []string            `json:"typefaces"`
	ReferenceImages int                 `json:"referenceImages,omitzero"`
}

// BrandTemplateOut is a template, without its images.
type BrandTemplateOut struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Ratios    []string `json:"ratios"`
	Platforms []string `json:"platforms"`
	IsDefault bool     `json:"isDefault,omitzero"`
}

func getBrand(ctx context.Context, d Deps, facts *brandfacts.Service) (*BrandOut, error) {
	all, err := d.Brand.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	out := &BrandOut{Guardrails: all.Guardrails, Voices: []BrandVoiceOut{}, Audiences: []BrandAudienceOut{}, Templates: []BrandTemplateOut{}}
	out.Facts, out.ExpiredFacts = facts.Split(all.Facts)
	for _, v := range all.Voices {
		out.Voices = append(out.Voices, voiceOut(&v))
	}
	for _, a := range all.Audiences {
		out.Audiences = append(out.Audiences, audienceOut(&a))
	}
	if l := all.Look; l != nil {
		look := &BrandLookOut{Logos: []string{}, Palette: l.Palette, Typefaces: l.Typefaces, ReferenceImages: len(l.ReferenceImages)}
		for _, lg := range l.Logos {
			look.Logos = append(look.Logos, lg.Job)
		}
		if look.Palette == nil {
			look.Palette = []models.BrandColor{}
		}
		if look.Typefaces == nil {
			look.Typefaces = []string{}
		}
		out.Look = look
	}
	for _, t := range all.Templates {
		tpl := BrandTemplateOut{ID: t.ID, Name: t.Name, Role: t.Role, Ratios: []string{}, Platforms: t.Platforms, IsDefault: t.IsDefault}
		for _, r := range t.Ratios {
			tpl.Ratios = append(tpl.Ratios, r.Ratio)
		}
		if tpl.Platforms == nil {
			tpl.Platforms = []string{}
		}
		out.Templates = append(out.Templates, tpl)
	}
	return out, nil
}

func voiceOut(v *models.BrandVoice) BrandVoiceOut {
	samples := []string(v.Samples)
	if samples == nil {
		samples = []string{}
	}
	return BrandVoiceOut{ID: v.ID, Name: v.Name, WhenToUse: v.WhenToUse, IsDefault: v.IsDefault, Summary: v.Summary,
		Rules: v.Rules, Samples: samples, ChannelNotes: v.ChannelNotes}
}

func audienceOut(a *models.BrandAudience) BrandAudienceOut {
	return BrandAudienceOut{ID: a.ID, Name: a.Name, Who: a.Who, ReadsOn: a.ReadsOn, ScrollsPastWhen: a.ScrollsPastWhen, BelievesWhen: a.BelievesWhen}
}

// FactInput is a new fact.
type FactInput struct {
	Statement string `json:"statement"`
	Subject   string `json:"subject,omitempty"   jsonschema:"description=us (default), problem or opportunity"`
	Kind      string `json:"kind,omitempty"      jsonschema:"description=measured, documented (default), commitment or judgement"`
	Source    string `json:"source,omitempty"    jsonschema:"description=Where it comes from, e.g. a report or URL"`
	CheckedAt string `json:"checkedAt,omitempty" jsonschema:"description=When it was last confirmed, YYYY-MM-DD"`
	ExpiresAt string `json:"expiresAt,omitempty" jsonschema:"description=When it goes stale, YYYY-MM-DD"`
}

// UpdateFactInput corrects a fact; empty fields keep their value.
type UpdateFactInput struct {
	FactID string `json:"factId"`
	FactInput
	// ClearExpiry removes the expiry date.
	ClearExpiry bool `json:"clearExpiry,omitempty"`
}

// RemoveFactInput names the fact to remove.
type RemoveFactInput struct {
	FactID string `json:"factId"`
}

// FactOut is the fact and the undo handle.
type FactOut struct {
	Fact   *models.BrandFact `json:"fact"`
	UndoID string            `json:"undoId,omitempty"`
}

func factErr(err error) error {
	switch {
	case brandfacts.IsRule(err):
		return assistantkit.Refused(err.Error())
	case errors.Is(err, repository.ErrFactDuplicate):
		return assistantkit.Refused("that fact is already in the ledger")
	case errors.Is(err, repository.ErrFactLimit):
		return assistantkit.Refused(fmt.Sprintf("the ledger holds at most %d facts; remove one first", repository.MaxBrandFacts))
	case errors.Is(err, sql.ErrNoRows):
		return assistantkit.Refused(noFact)
	case errors.Is(err, repository.ErrFactChanged):
		return assistantkit.Refused("that fact was changed while you were editing it; read it again with getBrand")
	}
	return err
}

const noFact = "that fact doesn't exist"

func setDates(in FactInput, into *brandfacts.Input) error {
	var err error
	if in.CheckedAt != "" {
		if into.CheckedAt, err = calendarDate(in.CheckedAt); err != nil {
			return err
		}
	}
	if in.ExpiresAt != "" {
		if into.ExpiresAt, err = calendarDate(in.ExpiresAt); err != nil {
			return err
		}
	}
	return nil
}

func addFact(ctx context.Context, d Deps, facts *brandfacts.Service, in FactInput) (*FactOut, error) {
	fi := brandfacts.Input{Statement: in.Statement, Subject: models.FactSubject(in.Subject), Kind: models.FactKind(in.Kind), Source: in.Source}
	if err := setDates(in, &fi); err != nil {
		return nil, err
	}
	by := actor(ctx)
	f, err := facts.Create(ctx, fi, &by)
	if err != nil {
		return nil, factErr(err)
	}
	assistantkit.Touch(ctx, "fact", f.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandFact, f.ID, assistantundo.BrandFact(nil), assistantundo.BrandFact(f))
	return &FactOut{Fact: f, UndoID: undo}, nil
}

func updateFact(ctx context.Context, d Deps, facts *brandfacts.Service, in UpdateFactInput) (*FactOut, error) {
	cur, err := d.Brand.GetFact(ctx, strings.TrimSpace(in.FactID))
	if err != nil {
		return nil, factErr(err)
	}
	if cur == nil {
		return nil, assistantkit.Refused(noFact)
	}
	fi := brandfacts.InputOf(cur)
	if in.Statement != "" {
		fi.Statement = in.Statement
	}
	if in.Subject != "" {
		fi.Subject = models.FactSubject(in.Subject)
	}
	if in.Kind != "" {
		fi.Kind = models.FactKind(in.Kind)
	}
	if in.Source != "" {
		fi.Source = in.Source
	}
	if in.ClearExpiry {
		fi.ExpiresAt = nil
	}
	if err := setDates(in.FactInput, &fi); err != nil {
		return nil, err
	}
	// The fields not passed were merged from cur: write only while the fact
	// still holds it, so a correction made since isn't reverted.
	before, after, err := facts.UpdateIf(ctx, cur.ID, fi, cur)
	if err != nil {
		return nil, factErr(err)
	}
	assistantkit.Touch(ctx, "fact", after.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandFact, after.ID, assistantundo.BrandFact(before), assistantundo.BrandFact(after))
	return &FactOut{Fact: after, UndoID: undo}, nil
}

func removeFact(ctx context.Context, d Deps, facts *brandfacts.Service, in RemoveFactInput) (*FactOut, error) {
	gone, err := facts.Delete(ctx, strings.TrimSpace(in.FactID))
	if err != nil {
		return nil, factErr(err)
	}
	if gone == nil {
		return nil, assistantkit.Refused(noFact)
	}
	assistantkit.Touch(ctx, "fact", gone.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandFact, gone.ID, assistantundo.BrandFact(gone), assistantundo.BrandFact(nil))
	return &FactOut{Fact: gone, UndoID: undo}, nil
}
