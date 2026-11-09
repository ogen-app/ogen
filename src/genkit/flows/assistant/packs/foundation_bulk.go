package packs

import (
	"context"
	"errors"
	"fmt"
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
}

// brandEdit is one change of a bulk edit: what it is, for the report, and
// how to make it, returning its undo id.
type brandEdit struct {
	what string
	run  func(ctx context.Context) (string, error)
}

func (in EditBrandInput) edits(d Deps, facts *brandfacts.Service, lib *brandlib.Service) []brandEdit {
	var out []brandEdit
	add := func(what string, run func(ctx context.Context) (string, error)) {
		out = append(out, brandEdit{what: what, run: run})
	}
	for _, f := range in.AddFacts {
		add("add fact “"+f.Statement+"”", func(ctx context.Context) (string, error) {
			r, err := addFact(ctx, d, facts, f)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, f := range in.UpdateFacts {
		add("update fact "+f.FactID, func(ctx context.Context) (string, error) {
			r, err := updateFact(ctx, d, facts, f)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, id := range in.RemoveFacts {
		add("remove fact "+id, func(ctx context.Context) (string, error) {
			r, err := removeFact(ctx, d, facts, RemoveFactInput{FactID: id})
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, v := range in.AddVoices {
		add("add voice "+v.Name, func(ctx context.Context) (string, error) {
			r, err := addVoice(ctx, d, lib, v)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, v := range in.UpdateVoices {
		add("update voice "+v.VoiceID, func(ctx context.Context) (string, error) {
			r, err := updateVoice(ctx, d, lib, v)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, id := range in.RemoveVoices {
		add("remove voice "+id, func(ctx context.Context) (string, error) {
			r, err := removeVoice(ctx, d, lib, VoiceRef{VoiceID: id})
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, a := range in.AddAudiences {
		add("add audience "+a.Name, func(ctx context.Context) (string, error) {
			r, err := addAudience(ctx, d, lib, a)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, a := range in.UpdateAudiences {
		add("update audience "+a.AudienceID, func(ctx context.Context) (string, error) {
			r, err := updateAudience(ctx, d, lib, a)
			return undoOf(r, err, func() string { return r.UndoID })
		})
	}
	for _, id := range in.RemoveAudiences {
		add("remove audience "+id, func(ctx context.Context) (string, error) {
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
	return nil
}

func (in EditBrandInput) summary() string {
	var parts []string
	for _, c := range []struct {
		n          int
		verb, what string
	}{
		{len(in.AddFacts), "add", "fact"}, {len(in.UpdateFacts), "update", "fact"}, {len(in.RemoveFacts), "remove", "fact"},
		{len(in.AddVoices), "add", "voice"}, {len(in.UpdateVoices), "update", "voice"}, {len(in.RemoveVoices), "remove", "voice"},
		{len(in.AddAudiences), "add", "audience"}, {len(in.UpdateAudiences), "update", "audience"}, {len(in.RemoveAudiences), "remove", "audience"},
	} {
		if c.n > 0 {
			parts = append(parts, c.verb+" "+plural(c.n, c.what, c.what+"s"))
		}
	}
	s := strings.Join(parts, ", ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// BrandEditFailure is a change a bulk edit couldn't make.
type BrandEditFailure struct {
	What   string `json:"what"`
	Reason string `json:"reason"`
}

// EditBrandOutput is what a bulk edit did: each change made has its own
// undo id.
type EditBrandOutput struct {
	Done    int                `json:"done"`
	UndoIDs []string           `json:"undoIds"`
	Failed  []BrandEditFailure `json:"failed,omitempty"`
	// Stopped is why the edit stopped part way; the changes after it were
	// not made.
	Stopped string `json:"stopped,omitempty"`
}

func editBrand(ctx context.Context, d Deps, facts *brandfacts.Service, lib *brandlib.Service, in EditBrandInput) (*EditBrandOutput, error) {
	if err := in.precheck(); err != nil {
		return nil, err
	}
	out := &EditBrandOutput{UndoIDs: []string{}}
	for _, e := range in.edits(d, facts, lib) {
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
			return out, nil
		}
		out.Done++
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
