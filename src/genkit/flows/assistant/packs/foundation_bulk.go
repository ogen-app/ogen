package packs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/brandfacts"
	"github.com/ogen-app/ogen/src/usecase/brandlib"
)

// EditBrandInput is several brand changes at once.
type EditBrandInput struct {
	AddFacts        []FactInput           `json:"addFacts,omitempty"`
	UpdateFacts     []UpdateFactInput     `json:"updateFacts,omitempty"`
	RemoveFacts     []string              `json:"removeFacts,omitempty"     jsonschema:"description=Fact ids"`
	AddVoices       []VoiceInput          `json:"addVoices,omitempty"`
	UpdateVoices    []UpdateVoiceInput    `json:"updateVoices,omitempty"`
	RemoveVoices    []string              `json:"removeVoices,omitempty"    jsonschema:"description=Voice ids"`
	AddAudiences    []AudienceInput       `json:"addAudiences,omitempty"`
	UpdateAudiences []UpdateAudienceInput `json:"updateAudiences,omitempty"`
	RemoveAudiences []string              `json:"removeAudiences,omitempty" jsonschema:"description=Audience ids"`
	// Kept is reported, not changed.
	Kept []KeptFact `json:"kept,omitempty" jsonschema:"description=Facts the request would have changed but you keep, each with why, e.g. a newer document still states it"`
}

// KeptFact is a fact a bulk edit left alone on purpose, and why.
type KeptFact struct {
	FactID string `json:"factId"`
	Reason string `json:"reason"`
	// Statement is filled in on the result.
	Statement string `json:"statement,omitempty"`
}

// brandEdit is one change of a bulk edit: what it is, for the report, and
// how to make it, returning its undo id.
type brandEdit struct {
	what string
	// kind is fact, voice or audience; action add, update or remove.
	kind, action string
	// keep adds the edit to a call that makes it again.
	keep func(*EditBrandInput)
	run  func(ctx context.Context) (string, error)
}

func (in EditBrandInput) edits(d Deps, facts *brandfacts.Service, lib *brandlib.Service) []brandEdit {
	var out []brandEdit
	add := func(what string, keep func(*EditBrandInput), run func(ctx context.Context) (string, error)) {
		action, rest, _ := strings.Cut(what, " ")
		kind, _, _ := strings.Cut(rest, " ")
		out = append(out, brandEdit{what: what, kind: kind, action: action, keep: keep, run: run})
	}
	for _, f := range in.AddFacts {
		add("add fact “"+f.Statement+"”", func(r *EditBrandInput) { r.AddFacts = append(r.AddFacts, f) }, func(ctx context.Context) (string, error) {
			r, err := addFact(ctx, d, facts, f)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, f := range in.UpdateFacts {
		add("update fact "+f.FactID, func(r *EditBrandInput) { r.UpdateFacts = append(r.UpdateFacts, f) }, func(ctx context.Context) (string, error) {
			r, err := updateFact(ctx, d, facts, f)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, id := range in.RemoveFacts {
		add("remove fact "+id, func(r *EditBrandInput) { r.RemoveFacts = append(r.RemoveFacts, id) }, func(ctx context.Context) (string, error) {
			r, err := removeFact(ctx, d, facts, RemoveFactInput{FactID: id})
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, v := range in.AddVoices {
		add("add voice "+v.Name, func(r *EditBrandInput) { r.AddVoices = append(r.AddVoices, v) }, func(ctx context.Context) (string, error) {
			r, err := addVoice(ctx, d, lib, v)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, v := range in.UpdateVoices {
		add("update voice "+v.VoiceID, func(r *EditBrandInput) { r.UpdateVoices = append(r.UpdateVoices, v) }, func(ctx context.Context) (string, error) {
			r, err := updateVoice(ctx, d, lib, v)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, id := range in.RemoveVoices {
		add("remove voice "+id, func(r *EditBrandInput) { r.RemoveVoices = append(r.RemoveVoices, id) }, func(ctx context.Context) (string, error) {
			r, err := removeVoice(ctx, d, lib, VoiceRef{VoiceID: id})
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, a := range in.AddAudiences {
		add("add audience "+a.Name, func(r *EditBrandInput) { r.AddAudiences = append(r.AddAudiences, a) }, func(ctx context.Context) (string, error) {
			r, err := addAudience(ctx, d, lib, a)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, a := range in.UpdateAudiences {
		add("update audience "+a.AudienceID, func(r *EditBrandInput) { r.UpdateAudiences = append(r.UpdateAudiences, a) }, func(ctx context.Context) (string, error) {
			r, err := updateAudience(ctx, d, lib, a)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, id := range in.RemoveAudiences {
		add("remove audience "+id, func(r *EditBrandInput) { r.RemoveAudiences = append(r.RemoveAudiences, id) }, func(ctx context.Context) (string, error) {
			r, err := removeAudience(ctx, d, lib, AudienceRef{AudienceID: id})
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	return out
}

func undoOf[T any](r *T, err error, id func() string) (string, error) {
	if err != nil {
		return "", err
	}
	return id(), nil
}

// precheck refuses an empty edit, and an update or removal that names no
// item: a bulk edit runs off the screen, so nothing defaults to the open one.
func (in EditBrandInput) precheck() error {
	if in.size() == 0 {
		return assistantkit.Refused("no changes given")
	}
	ids := append(append(append([]string{}, in.RemoveFacts...), in.RemoveVoices...), in.RemoveAudiences...)
	for _, f := range in.UpdateFacts {
		ids = append(ids, f.FactID)
	}
	for _, v := range in.UpdateVoices {
		ids = append(ids, v.VoiceID)
	}
	for _, a := range in.UpdateAudiences {
		ids = append(ids, a.AudienceID)
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return assistantkit.Refused("every update or removal needs its id from getBrand")
		}
	}
	// A fact kept on purpose isn't also changed by the same edit.
	changed := map[string]bool{}
	for _, id := range in.RemoveFacts {
		changed[strings.TrimSpace(id)] = true
	}
	for _, f := range in.UpdateFacts {
		changed[strings.TrimSpace(f.FactID)] = true
	}
	for _, k := range in.Kept {
		if changed[strings.TrimSpace(k.FactID)] {
			return assistantkit.Refused("fact " + k.FactID + " is both kept and changed; leave it out of one")
		}
	}
	return nil
}

// brandChanges is one kind of change of a bulk edit: N items of a kind
// added, updated or removed.
type brandChanges struct {
	item, action string
	n            int
	// name reads the i-th item's name; empty when it can't be read.
	name func(ctx context.Context, i int) string
}

func (c brandChanges) String() string {
	return c.action + " " + plural(c.n, c.item, c.item+"s")
}

// changes is the edit's changes by kind, in the order they are made, kinds
// with none left out.
func (in EditBrandInput) changes(d Deps) []brandChanges {
	fact := func(id func(int) string) func(context.Context, int) string {
		return func(ctx context.Context, i int) string {
			if f, err := d.Brand.GetFact(ctx, strings.TrimSpace(id(i))); err == nil && f != nil {
				return f.Statement
			}
			return ""
		}
	}
	voice := func(id func(int) string) func(context.Context, int) string {
		return func(ctx context.Context, i int) string {
			if v, err := d.Brand.GetVoice(ctx, strings.TrimSpace(id(i))); err == nil && v != nil {
				return v.Name
			}
			return ""
		}
	}
	audience := func(id func(int) string) func(context.Context, int) string {
		return func(ctx context.Context, i int) string {
			if a, err := d.Brand.GetAudience(ctx, strings.TrimSpace(id(i))); err == nil && a != nil {
				return a.Name
			}
			return ""
		}
	}
	all := []brandChanges{
		{assistantkit.CardItemFact, assistantkit.CardActionAdd, len(in.AddFacts),
			func(_ context.Context, i int) string { return strings.TrimSpace(in.AddFacts[i].Statement) }},
		{assistantkit.CardItemFact, assistantkit.CardActionUpdate, len(in.UpdateFacts), fact(func(i int) string { return in.UpdateFacts[i].FactID })},
		{assistantkit.CardItemFact, assistantkit.CardActionRemove, len(in.RemoveFacts), fact(func(i int) string { return in.RemoveFacts[i] })},
		{assistantkit.CardItemVoice, assistantkit.CardActionAdd, len(in.AddVoices),
			func(_ context.Context, i int) string { return strings.TrimSpace(in.AddVoices[i].Name) }},
		{assistantkit.CardItemVoice, assistantkit.CardActionUpdate, len(in.UpdateVoices), voice(func(i int) string { return in.UpdateVoices[i].VoiceID })},
		{assistantkit.CardItemVoice, assistantkit.CardActionRemove, len(in.RemoveVoices), voice(func(i int) string { return in.RemoveVoices[i] })},
		{assistantkit.CardItemAudience, assistantkit.CardActionAdd, len(in.AddAudiences),
			func(_ context.Context, i int) string { return strings.TrimSpace(in.AddAudiences[i].Name) }},
		{assistantkit.CardItemAudience, assistantkit.CardActionUpdate, len(in.UpdateAudiences),
			audience(func(i int) string { return in.UpdateAudiences[i].AudienceID })},
		{assistantkit.CardItemAudience, assistantkit.CardActionRemove, len(in.RemoveAudiences), audience(func(i int) string { return in.RemoveAudiences[i] })},
	}
	return slices.DeleteFunc(all, func(c brandChanges) bool { return c.n == 0 })
}

func (in EditBrandInput) summary() string {
	var parts []string
	for _, c := range in.changes(Deps{}) {
		parts = append(parts, c.String())
	}
	return capitalize(strings.Join(parts, ", "))
}

// cardNames is how many items a change row names.
const cardNames = 5

// card is the bulk edit offer's card: a row per kind of change naming its
// first few items, then the facts kept and why.
func (in EditBrandInput) card(ctx context.Context, d Deps) *assistantkit.Card {
	changes := in.changes(d)
	if len(changes) == 0 {
		return nil
	}
	c := &assistantkit.Card{}
	for _, ch := range changes {
		row := assistantkit.CardRow{Kind: assistantkit.CardRowChange, Value: capitalize(ch.String()), Item: ch.item, Action: ch.action, Count: ch.n}
		for i := range min(ch.n, cardNames) {
			if name := ch.name(ctx, i); name != "" {
				row.Names = append(row.Names, name)
			}
		}
		c.Rows = append(c.Rows, row)
	}
	if len(changes) == 1 {
		r := c.Rows[0]
		c.Verb, c.Action, c.Item, c.Count = r.Value, r.Action, r.Item, r.Count
	} else {
		c.Verb, c.Action, c.Count = "Make "+plural(in.size(), "brand change", "brand changes"), assistantkit.CardActionEditBrand, in.size()
	}
	if kept, err := keptFacts(ctx, d, in.Kept); err == nil {
		for _, k := range kept {
			c.Rows = append(c.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowKept, Value: k.Statement, Item: assistantkit.CardItemFact, Note: k.Reason})
		}
	}
	return c
}

// BrandEditFailure is a change a bulk edit couldn't make.
type BrandEditFailure struct {
	What   string `json:"what"`
	Reason string `json:"reason"`
}

// EditCounts counts the changes made to one kind of item.
type EditCounts struct {
	Added   int `json:"added,omitzero"`
	Updated int `json:"updated,omitzero"`
	Removed int `json:"removed,omitzero"`
}

func (c *EditCounts) count(action string) {
	switch action {
	case "add":
		c.Added++
	case "update":
		c.Updated++
	case "remove":
		c.Removed++
	}
}

// BrandEditCounts is what a bulk edit made, by kind of item.
type BrandEditCounts struct {
	Facts     EditCounts `json:"facts,omitzero"`
	Voices    EditCounts `json:"voices,omitzero"`
	Audiences EditCounts `json:"audiences,omitzero"`
}

func (c *BrandEditCounts) count(e brandEdit) {
	switch e.kind {
	case "fact":
		c.Facts.count(e.action)
	case "voice":
		c.Voices.count(e.action)
	case "audience":
		c.Audiences.count(e.action)
	}
}

// EditBrandOutput is what a bulk edit did: each change made has its own
// undo id.
type EditBrandOutput struct {
	Done int `json:"done"`
	// Made counts the changes made by kind and action, e.g. 6 facts removed.
	Made    BrandEditCounts `json:"made"`
	UndoIDs []string        `json:"undoIds"`
	// Kept is the facts left alone on purpose.
	Kept   []KeptFact         `json:"kept,omitempty"`
	Failed []BrandEditFailure `json:"failed,omitempty"`
	// Stopped is why the edit stopped part way; the changes after it were
	// not made.
	Stopped string `json:"stopped,omitempty"`
	// left is the changes the stop left, from the one it failed on.
	left []brandEdit
	assistantkit.Partial
}

func editBrand(ctx context.Context, d Deps, facts *brandfacts.Service, lib *brandlib.Service, in EditBrandInput) (*EditBrandOutput, error) {
	if err := in.precheck(); err != nil {
		return nil, err
	}
	kept, err := keptFacts(ctx, d, in.Kept)
	if err != nil {
		return nil, err
	}
	out := &EditBrandOutput{UndoIDs: []string{}, Kept: kept}
	edits := in.edits(d, facts, lib)
	for i, e := range edits {
		undo, err := e.run(ctx)
		if r, ok := errors.AsType[*assistantkit.Refusal](err); ok {
			out.Failed = append(out.Failed, BrandEditFailure{What: e.what, Reason: r.Reason})
			continue
		}
		if err != nil {
			if out.Done == 0 {
				return nil, err
			}
			out.Stopped = fmt.Sprintf("%s failed: %v", e.what, err)
			out.left = edits[i:]
			return out, nil
		}
		out.Done++
		out.Made.count(e)
		if undo != "" {
			out.UndoIDs = append(out.UndoIDs, undo)
		}
	}
	return out, nil
}

// size counts the changes.
func (in EditBrandInput) size() int {
	return len(in.AddFacts) + len(in.UpdateFacts) + len(in.RemoveFacts) + len(in.AddVoices) + len(in.UpdateVoices) +
		len(in.RemoveVoices) + len(in.AddAudiences) + len(in.UpdateAudiences) + len(in.RemoveAudiences)
}

// brandRetry reads a bulk edit: the changes refused, and the ones a stop
// left, which the retried call makes again.
func brandRetry(in EditBrandInput, out *EditBrandOutput) (EditBrandInput, []assistantkit.NotMade, int, bool) {
	var notMade []assistantkit.NotMade
	for _, f := range out.Failed {
		notMade = append(notMade, assistantkit.NotMade{ID: f.What, Title: f.What, Reason: f.Reason})
	}
	retry := EditBrandInput{Kept: in.Kept}
	for _, e := range out.left {
		notMade = append(notMade, assistantkit.NotMade{ID: e.what, Title: e.what, Reason: out.Stopped})
		e.keep(&retry)
	}
	return retry, notMade, out.Done, len(out.left) > 0
}

// keptFacts are the facts kept, with their statements; one that doesn't
// exist is dropped.
func keptFacts(ctx context.Context, d Deps, kept []KeptFact) ([]KeptFact, error) {
	var out []KeptFact
	for _, k := range kept {
		f, err := d.Brand.GetFact(ctx, strings.TrimSpace(k.FactID))
		if err != nil {
			return nil, err
		}
		if f == nil {
			continue
		}
		out = append(out, KeptFact{FactID: f.ID, Reason: strings.TrimSpace(k.Reason), Statement: f.Statement})
	}
	return out, nil
}
