package packs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
	"github.com/ogen-app/ogen/src/usecase/brandlib"
)

// VoiceRulesInput are a voice's rules; empty fields are unset, or kept on an
// edit.
type VoiceRulesInput struct {
	Emoji     string `json:"emoji,omitempty"     jsonschema:"description=never, sparingly or freely"`
	Hashtags  string `json:"hashtags,omitempty"  jsonschema:"description=never, few or many"`
	Formality string `json:"formality,omitempty" jsonschema:"description=casual, neutral or formal"`
	Person    string `json:"person,omitempty"    jsonschema:"description=i, we or third"`
	Length    string `json:"length,omitempty"    jsonschema:"description=short, medium or long"`
	Opening   string `json:"opening,omitempty"   jsonschema:"description=How posts open"`
	Closing   string `json:"closing,omitempty"   jsonschema:"description=How posts close"`
}

func (in VoiceRulesInput) over(r models.VoiceRules) models.VoiceRules {
	r.Emoji = cmp.Or(strings.TrimSpace(in.Emoji), r.Emoji)
	r.Hashtags = cmp.Or(strings.TrimSpace(in.Hashtags), r.Hashtags)
	r.Formality = cmp.Or(strings.TrimSpace(in.Formality), r.Formality)
	r.Person = cmp.Or(strings.TrimSpace(in.Person), r.Person)
	r.Length = cmp.Or(strings.TrimSpace(in.Length), r.Length)
	r.Opening = cmp.Or(strings.TrimSpace(in.Opening), r.Opening)
	r.Closing = cmp.Or(strings.TrimSpace(in.Closing), r.Closing)
	return r
}

// VoiceInput is a new voice.
type VoiceInput struct {
	Name      string          `json:"name"`
	WhenToUse string          `json:"whenToUse,omitempty"`
	IsDefault bool            `json:"isDefault,omitempty" jsonschema:"description=Make it the voice posts use when none is chosen"`
	Samples   []string        `json:"samples,omitempty"   jsonschema:"description=Real posts written in it, verbatim"`
	Rules     VoiceRulesInput `json:"rules,omitzero"`
	// ChannelNotes are notes for one platform, by platform slug.
	ChannelNotes map[string]string `json:"channelNotes,omitempty"`
}

// UpdateVoiceInput edits a voice; empty fields keep their value.
type UpdateVoiceInput struct {
	VoiceID       string          `json:"voiceId,omitempty" jsonschema:"description=The voice; the open one when omitted"`
	Name          string          `json:"name,omitempty"`
	WhenToUse     string          `json:"whenToUse,omitempty"`
	MakeDefault   bool            `json:"makeDefault,omitempty"`
	AddSamples    []string        `json:"addSamples,omitempty"`
	RemoveSamples []string        `json:"removeSamples,omitempty" jsonschema:"description=Samples to drop, verbatim from getBrand"`
	Rules         VoiceRulesInput `json:"rules,omitzero"`
	// ChannelNotes sets notes by platform slug; an empty note removes one.
	ChannelNotes map[string]string `json:"channelNotes,omitempty"`
}

// VoiceRef names a voice.
type VoiceRef struct {
	VoiceID string `json:"voiceId,omitempty" jsonschema:"description=The voice; the open one when omitted"`
}

// VoiceOut is a voice changed and the undo handle.
type VoiceOut struct {
	Voice BrandVoiceOut `json:"voice"`
	// Unbound counts the campaigns and posts a removed voice was set on.
	Unbound int    `json:"unbound,omitzero"`
	UndoID  string `json:"undoId,omitempty"`
}

// AudienceInput is a new audience.
type AudienceInput struct {
	Name            string `json:"name"`
	Who             string `json:"who,omitempty"`
	ReadsOn         string `json:"readsOn,omitempty"`
	ScrollsPastWhen string `json:"scrollsPastWhen,omitempty"`
	BelievesWhen    string `json:"believesWhen,omitempty"`
}

// UpdateAudienceInput edits an audience; empty fields keep their value.
type UpdateAudienceInput struct {
	AudienceID string `json:"audienceId,omitempty" jsonschema:"description=The audience; the open one when omitted"`
	AudienceInput
}

// AudienceRef names an audience.
type AudienceRef struct {
	AudienceID string `json:"audienceId,omitempty" jsonschema:"description=The audience; the open one when omitted"`
}

// AudienceOut is an audience changed and the undo handle.
type AudienceOut struct {
	Audience BrandAudienceOut `json:"audience"`
	Unbound  int              `json:"unbound,omitzero"`
	UndoID   string           `json:"undoId,omitempty"`
}

// GuardrailsInput edits the guardrails: lines added or removed, the
// disclaimer set or cleared.
type GuardrailsInput struct {
	AddMayClaim       []string `json:"addMayClaim,omitempty"`
	RemoveMayClaim    []string `json:"removeMayClaim,omitempty"`
	AddNeverClaim     []string `json:"addNeverClaim,omitempty"`
	RemoveNeverClaim  []string `json:"removeNeverClaim,omitempty"`
	AddBannedWords    []string `json:"addBannedWords,omitempty"`
	RemoveBannedWords []string `json:"removeBannedWords,omitempty"`
	Disclaimer        string   `json:"disclaimer,omitempty"`
	ClearDisclaimer   bool     `json:"clearDisclaimer,omitempty"`
}

// GuardrailsOut is the guardrails changed and the undo handle.
type GuardrailsOut struct {
	Guardrails *models.BrandGuardrails `json:"guardrails"`
	UndoID     string                  `json:"undoId,omitempty"`
}

func libErr(err error, missing string) error {
	switch {
	case brandlib.IsRule(err):
		return assistantkit.Refused(err.Error())
	case errors.Is(err, repository.ErrBrandChanged):
		return assistantkit.Refused("it was changed while you were editing it; read it again with getBrand")
	case errors.Is(err, sql.ErrNoRows):
		return assistantkit.Refused(missing)
	}
	return err
}

const (
	noVoice    = "that voice doesn't exist"
	noAudience = "that audience doesn't exist"
)

// pick is id, or the item of kind the screen shows.
func pick(ctx context.Context, id string, kind viewcontext.EntityKind, what string) (string, error) {
	if id = cmp.Or(strings.TrimSpace(id), assistant.Focused(ctx, kind)); id == "" {
		return "", assistantkit.Refused("say which " + what)
	}
	return id, nil
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// edit adds and removes lines, keeping list's order and dropping repeats.
func edit(list, add, remove []string) models.StringSlice {
	drop := map[string]bool{}
	for _, s := range trimAll(remove) {
		drop[strings.ToLower(s)] = true
	}
	out := models.StringSlice{}
	seen := map[string]bool{}
	for _, s := range slices.Concat(list, trimAll(add)) {
		k := strings.ToLower(s)
		if drop[k] || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

// defaultVoice is the id of the workspace's default voice; "" for none.
func defaultVoice(ctx context.Context, d Deps) (string, error) {
	all, err := d.Brand.GetAll(ctx)
	if err != nil {
		return "", err
	}
	for _, v := range all.Voices {
		if v.IsDefault {
			return v.ID, nil
		}
	}
	return "", nil
}

func addVoice(ctx context.Context, d Deps, lib *brandlib.Service, in VoiceInput) (*VoiceOut, error) {
	replaced := ""
	if in.IsDefault {
		var err error
		if replaced, err = defaultVoice(ctx, d); err != nil {
			return nil, err
		}
	}
	v := &models.BrandVoice{Name: strings.TrimSpace(in.Name), WhenToUse: strings.TrimSpace(in.WhenToUse), IsDefault: in.IsDefault,
		Samples: trimAll(in.Samples), Rules: in.Rules.over(models.VoiceRules{}), ChannelNotes: channelNotes(nil, in.ChannelNotes)}
	created, err := lib.CreateVoice(ctx, v)
	if err != nil {
		return nil, libErr(err, noVoice)
	}
	assistantkit.Touch(ctx, "voice", created.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandVoice, created.ID,
		assistantundo.BrandVoice(nil, nil, ""), assistantundo.BrandVoice(created, nil, replaced))
	return &VoiceOut{Voice: voiceOut(created), UndoID: undo}, nil
}

func channelNotes(have models.StringMap, set map[string]string) models.StringMap {
	out := maps.Clone(have)
	if out == nil {
		out = models.StringMap{}
	}
	for k, v := range set {
		k = strings.ToLower(strings.TrimSpace(k))
		if v = strings.TrimSpace(v); v == "" {
			delete(out, k)
		} else if k != "" {
			out[k] = v
		}
	}
	return out
}

func updateVoice(ctx context.Context, d Deps, lib *brandlib.Service, in UpdateVoiceInput) (*VoiceOut, error) {
	id, err := pick(ctx, in.VoiceID, viewcontext.KindVoice, "voice")
	if err != nil {
		return nil, err
	}
	cur, err := d.Brand.GetVoice(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, assistantkit.Refused(noVoice)
	}
	replaced := ""
	if in.MakeDefault && !cur.IsDefault {
		if replaced, err = defaultVoice(ctx, d); err != nil {
			return nil, err
		}
	}
	v := *cur
	v.Name = cmp.Or(strings.TrimSpace(in.Name), cur.Name)
	v.WhenToUse = cmp.Or(strings.TrimSpace(in.WhenToUse), cur.WhenToUse)
	v.IsDefault = cur.IsDefault || in.MakeDefault
	v.Samples = edit(cur.Samples, in.AddSamples, in.RemoveSamples)
	v.Rules = in.Rules.over(cur.Rules)
	v.ChannelNotes = channelNotes(cur.ChannelNotes, in.ChannelNotes)
	// The fields not passed were merged from cur: write only while the voice
	// still holds it, so an edit made since isn't reverted.
	before, after, err := lib.UpdateVoiceIf(ctx, &v, cur)
	if err != nil {
		return nil, libErr(err, noVoice)
	}
	assistantkit.Touch(ctx, "voice", after.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandVoice, after.ID,
		assistantundo.BrandVoice(before, nil, ""), assistantundo.BrandVoice(after, nil, replaced))
	return &VoiceOut{Voice: voiceOut(after), UndoID: undo}, nil
}

func removeVoice(ctx context.Context, d Deps, lib *brandlib.Service, in VoiceRef) (*VoiceOut, error) {
	id, err := pick(ctx, in.VoiceID, viewcontext.KindVoice, "voice")
	if err != nil {
		return nil, err
	}
	gone, bound, err := lib.RemoveVoice(ctx, id)
	if err != nil {
		return nil, libErr(err, noVoice)
	}
	if gone == nil {
		return nil, assistantkit.Refused(noVoice)
	}
	assistantkit.Touch(ctx, "voice", gone.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandVoice, gone.ID,
		assistantundo.BrandVoice(gone, &bound, ""), assistantundo.BrandVoice(nil, nil, ""))
	return &VoiceOut{Voice: voiceOut(gone), Unbound: len(bound.Campaigns) + len(bound.Posts), UndoID: undo}, nil
}

func addAudience(ctx context.Context, d Deps, lib *brandlib.Service, in AudienceInput) (*AudienceOut, error) {
	a := &models.BrandAudience{}
	in.over(a)
	created, err := lib.CreateAudience(ctx, a)
	if err != nil {
		return nil, libErr(err, noAudience)
	}
	assistantkit.Touch(ctx, "audience", created.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandAudience, created.ID,
		assistantundo.BrandAudience(nil, nil), assistantundo.BrandAudience(created, nil))
	return &AudienceOut{Audience: audienceOut(created), UndoID: undo}, nil
}

func (in AudienceInput) over(a *models.BrandAudience) {
	a.Name = cmp.Or(strings.TrimSpace(in.Name), a.Name)
	a.Who = cmp.Or(strings.TrimSpace(in.Who), a.Who)
	a.ReadsOn = cmp.Or(strings.TrimSpace(in.ReadsOn), a.ReadsOn)
	a.ScrollsPastWhen = cmp.Or(strings.TrimSpace(in.ScrollsPastWhen), a.ScrollsPastWhen)
	a.BelievesWhen = cmp.Or(strings.TrimSpace(in.BelievesWhen), a.BelievesWhen)
}

func updateAudience(ctx context.Context, d Deps, lib *brandlib.Service, in UpdateAudienceInput) (*AudienceOut, error) {
	id, err := pick(ctx, in.AudienceID, viewcontext.KindAudience, "audience")
	if err != nil {
		return nil, err
	}
	cur, err := d.Brand.GetAudience(ctx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, assistantkit.Refused(noAudience)
	}
	a := *cur
	in.over(&a)
	before, after, err := lib.UpdateAudienceIf(ctx, &a, cur)
	if err != nil {
		return nil, libErr(err, noAudience)
	}
	assistantkit.Touch(ctx, "audience", after.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandAudience, after.ID,
		assistantundo.BrandAudience(before, nil), assistantundo.BrandAudience(after, nil))
	return &AudienceOut{Audience: audienceOut(after), UndoID: undo}, nil
}

func removeAudience(ctx context.Context, d Deps, lib *brandlib.Service, in AudienceRef) (*AudienceOut, error) {
	id, err := pick(ctx, in.AudienceID, viewcontext.KindAudience, "audience")
	if err != nil {
		return nil, err
	}
	gone, bound, err := lib.RemoveAudience(ctx, id)
	if err != nil {
		return nil, libErr(err, noAudience)
	}
	if gone == nil {
		return nil, assistantkit.Refused(noAudience)
	}
	assistantkit.Touch(ctx, "audience", gone.ID)
	undo := recordUndo(ctx, d, models.AssistantActionBrandAudience, gone.ID,
		assistantundo.BrandAudience(gone, &bound), assistantundo.BrandAudience(nil, nil))
	return &AudienceOut{Audience: audienceOut(gone), Unbound: len(bound.Campaigns) + len(bound.Posts), UndoID: undo}, nil
}

func updateGuardrails(ctx context.Context, d Deps, lib *brandlib.Service, in GuardrailsInput) (*GuardrailsOut, error) {
	cur, err := d.Brand.GetGuardrails(ctx)
	if err != nil {
		return nil, err
	}
	g := &models.BrandGuardrails{}
	if cur != nil {
		g.MayClaim, g.NeverClaim, g.BannedWords, g.Disclaimer = cur.MayClaim, cur.NeverClaim, cur.BannedWords, cur.Disclaimer
	}
	g.MayClaim = edit(g.MayClaim, in.AddMayClaim, in.RemoveMayClaim)
	g.NeverClaim = edit(g.NeverClaim, in.AddNeverClaim, in.RemoveNeverClaim)
	g.BannedWords = edit(g.BannedWords, in.AddBannedWords, in.RemoveBannedWords)
	if in.ClearDisclaimer {
		g.Disclaimer = ""
	}
	g.Disclaimer = cmp.Or(strings.TrimSpace(in.Disclaimer), g.Disclaimer)
	if cur != nil && repository.SameGuardrails(cur, g) {
		return &GuardrailsOut{Guardrails: cur}, nil
	}
	if len(g.MayClaim) == 0 && len(g.NeverClaim) == 0 && len(g.BannedWords) == 0 && g.Disclaimer == "" {
		return nil, assistantkit.Refused("that would leave the guardrails empty; clear them on the Guardrails screen instead")
	}
	before, after, err := lib.SaveGuardrailsIf(ctx, g, cur)
	if err != nil {
		return nil, libErr(err, "")
	}
	assistantkit.Touch(ctx, "guardrails", "")
	undo := recordUndo(ctx, d, models.AssistantActionBrandGuardrails, "guardrails",
		assistantundo.BrandGuardrails(before), assistantundo.BrandGuardrails(after))
	return &GuardrailsOut{Guardrails: after, UndoID: undo}, nil
}

// librarySpecs are the voice, audience and guardrail edits, each undoable.
func librarySpecs(g *genkit.Genkit, d Deps, lib *brandlib.Service) []assistantkit.Spec {
	addV := genkit.DefineTool(g, "addVoice", "Adds a voice: a name, when to use it, real samples, rules and per-channel notes. Returns an undo id.",
		assistantkit.Wrap(func(ctx context.Context, in VoiceInput) (*VoiceOut, error) { return addVoice(ctx, d, lib, in) }), ai.WithStrictSchema(false))
	editV := genkit.DefineTool(g, "updateVoice", "Edits a voice: only the fields passed change. Returns an undo id.",
		assistantkit.Wrap(func(ctx context.Context, in UpdateVoiceInput) (*VoiceOut, error) { return updateVoice(ctx, d, lib, in) }), ai.WithStrictSchema(false))
	rmV := genkit.DefineTool(g, "removeVoice", "Removes a voice; campaigns and posts set to it fall back to the default. Returns an undo id.",
		assistantkit.Wrap(func(ctx context.Context, in VoiceRef) (*VoiceOut, error) { return removeVoice(ctx, d, lib, in) }), ai.WithStrictSchema(false))
	addA := genkit.DefineTool(g, "addAudience", "Adds an audience: who they are, where they read, what makes them scroll past and what they need to believe. Returns an undo id.",
		assistantkit.Wrap(func(ctx context.Context, in AudienceInput) (*AudienceOut, error) { return addAudience(ctx, d, lib, in) }), ai.WithStrictSchema(false))
	editA := genkit.DefineTool(g, "updateAudience", "Edits an audience: only the fields passed change. Returns an undo id.",
		assistantkit.Wrap(func(ctx context.Context, in UpdateAudienceInput) (*AudienceOut, error) {
			return updateAudience(ctx, d, lib, in)
		}), ai.WithStrictSchema(false))
	rmA := genkit.DefineTool(g, "removeAudience", "Removes an audience. Returns an undo id.",
		assistantkit.Wrap(func(ctx context.Context, in AudienceRef) (*AudienceOut, error) {
			return removeAudience(ctx, d, lib, in)
		}), ai.WithStrictSchema(false))
	guard := genkit.DefineTool(g, "updateGuardrails", "Edits the guardrails: adds or removes may-claim and never-claim lines and banned words, sets or clears the disclaimer. Returns an undo id.",
		assistantkit.Wrap(func(ctx context.Context, in GuardrailsInput) (*GuardrailsOut, error) {
			return updateGuardrails(ctx, d, lib, in)
		}), ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(foundationPackName, addV, assistantkit.TierReversible,
			`Call this when the user asks for a new voice — "add a casual voice for Instagram". Put real posts they give in samples, verbatim; never invent samples. It can be undone.`),
		spec(foundationPackName, editV, assistantkit.TierReversible,
			`Call this to change one voice — its name, when to use it, rules, samples, channel notes — or to make it the default. Pass voiceId from getBrand, or leave it out on the voice's own screen. It can be undone.`),
		spec(foundationPackName, rmV, assistantkit.TierReversible,
			`Call this when the user asks to delete a voice. It can be undone.`),
		spec(foundationPackName, addA, assistantkit.TierReversible,
			`Call this when the user describes a new audience to write for. It can be undone.`),
		spec(foundationPackName, editA, assistantkit.TierReversible,
			`Call this to change one audience. Pass audienceId from getBrand, or leave it out on the audience's own screen. It can be undone.`),
		spec(foundationPackName, rmA, assistantkit.TierReversible,
			`Call this when the user asks to delete an audience. It can be undone.`),
		spec(foundationPackName, guard, assistantkit.TierReversible,
			`Call this when the user asks to ban a word, add or drop a claim posts may or may never make, or change the disclaimer. Facts posts can claim go in the fact ledger (addFact), not here. It can be undone.`),
	}
}
