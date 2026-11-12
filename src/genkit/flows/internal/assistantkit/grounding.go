package assistantkit

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/firebase/genkit/go/ai"
)

// GroundingMode says whether generated content came from the user's own
// material.
type GroundingMode string

const (
	// Grounded content drew on the listed sources.
	Grounded GroundingMode = "grounded"
	// Exploratory content came from general knowledge; the UI labels it
	// "not grounded in your sources".
	Exploratory GroundingMode = "exploratory"
)

// SourcePost is a source that is one of the workspace's posts, e.g. the
// example a "more like these" idea was written from.
const SourcePost = "post"

// Source is one piece of the user's material generation drew on: an asset,
// and, when known, the analysis and the idea it reached the output through,
// so the output traces back to its idea and asset. Kind is empty for an
// asset, or SourcePost.
type Source struct {
	// ID is the asset's, or the post's.
	ID         string `json:"id"`
	Title      string `json:"title"`
	Kind       string `json:"kind,omitempty"`
	AnalysisID string `json:"analysisId,omitempty"`
	IdeaID     string `json:"ideaId,omitempty"`
}

// The options of the choice generation without sources becomes: add a
// document, import a page, or go ahead from general knowledge.
const (
	OptionAddDocument = "add_document"
	OptionImportPage  = "import_page"
	OptionExploratory = "exploratory"
)

// AwaitingSource are the options that go to add a source: an offer
// confirmed with one waits for it, and is offered again once it is ready.
var AwaitingSource = []string{OptionAddDocument, OptionImportPage}

// UngroundedReason says why output came from general knowledge.
type UngroundedReason string

const (
	// NoSources: there was nothing of the user's ready to draw on.
	NoSources UngroundedReason = "no_sources"
	// ChoseWithout: the user chose to go ahead without sources.
	ChoseWithout UngroundedReason = "chosen"
	// NoneRelevant: sources were there, but none of them was about it.
	NoneRelevant UngroundedReason = "none_relevant"
)

// ungroundedNotes is the sentence the UI shows after "Not grounded" for
// each reason.
var ungroundedNotes = map[UngroundedReason]string{
	NoSources:    "there are no documents to draw on yet",
	ChoseWithout: "you chose to go ahead without sources",
	NoneRelevant: "nothing in the documents is about this",
}

// Grounding is attached to every generating tool's result, so the UI can
// badge exploratory output without relying on how the model words its reply.
type Grounding struct {
	Mode    GroundingMode `json:"mode"`
	Sources []Source      `json:"sources,omitempty"`
	// Reason and Note say why exploratory output is: a code, and the
	// sentence that follows "Not grounded". Unset when grounded, or when
	// why isn't known.
	Reason UngroundedReason `json:"reason,omitempty"`
	Note   string           `json:"note,omitempty"`
}

// GroundingFrom is grounded when any asset informed the output. Output that
// cites only posts stays exploratory: a post isn't vetted material, though
// the card still names it.
func GroundingFrom(sources []Source) Grounding {
	if len(sources) == 0 {
		return Grounding{Mode: Exploratory}
	}
	mode := Exploratory
	if slices.ContainsFunc(sources, func(s Source) bool { return s.Kind == "" }) {
		mode = Grounded
	}
	return Grounding{Mode: mode, Sources: sources}
}

// Because says why exploratory output is; grounded output is left as it is.
func (g Grounding) Because(reason UngroundedReason) Grounding {
	if g.Mode == Exploratory {
		g.Reason, g.Note = reason, ungroundedNotes[reason]
	}
	return g
}

// UngroundedBy is why output drew on none of the offered sources: there
// were none, or none was about it.
func UngroundedBy(offered int) UngroundedReason {
	if offered == 0 {
		return NoSources
	}
	return NoneRelevant
}

// WithAnalyses fills in the analysis each source's asset has, from lens:
// asset ids to their analysis's id. A source that already names one keeps
// it, and a failed lookup leaves the sources as they are.
func (g Grounding) WithAnalyses(lens func(assetIDs []string) (map[string]string, error)) Grounding {
	var ids []string
	for _, s := range g.Sources {
		if s.Kind == "" && s.AnalysisID == "" && !slices.Contains(ids, s.ID) {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) == 0 {
		return g
	}
	found, err := lens(ids)
	if err != nil {
		return g
	}
	sources := slices.Clone(g.Sources)
	for i, s := range sources {
		if s.Kind == "" && s.AnalysisID == "" {
			sources[i].AnalysisID = found[s.ID]
		}
	}
	g.Sources = sources
	return g
}

// checkGrounding holds a generating tool to saying what its output is
// grounded in: a "grounding" property somewhere in its output schema.
func (s Spec) checkGrounding() error {
	if !s.Generates {
		return nil
	}
	tool, ok := s.Tool.(ai.Tool)
	if !ok || tool.Definition() == nil || !hasProperty(tool.Definition().OutputSchema, "grounding") {
		return fmt.Errorf("tool %q: a generating tool's output must carry grounding", s.Name)
	}
	return nil
}

// hasProperty reports whether a JSON schema, or any schema nested in it,
// has a property named name.
func hasProperty(schema any, name string) bool {
	switch v := schema.(type) {
	case map[string]any:
		if props, ok := v["properties"].(map[string]any); ok {
			if _, ok := props[name]; ok {
				return true
			}
		}
		for _, child := range v {
			if hasProperty(child, name) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, func(child any) bool { return hasProperty(child, name) })
	}
	return false
}

// Cited is the assets a saved turn drew on, each once, in the order they
// came: its ask (the campaign assistant's own), then every pack's results,
// packs and their fields by name: the passages an ask quoted, the analysis
// one read, and the grounding of what a tool compared or generated, with
// the analysis and idea each came through. None for a turn that isn't JSON
// or drew on nothing.
func Cited(turn []byte) []Source {
	var env struct {
		Ask     any `json:"ask"`
		Results any `json:"results"`
	}
	if json.Unmarshal(turn, &env) != nil {
		return nil
	}
	var c citations
	c.walk("ask", env.Ask)
	c.walk("", env.Results)
	return c.out
}

// citations collects sources, one per asset: a later mention of an asset
// fills in the analysis or idea an earlier one lacked.
type citations struct{ out []Source }

func (c *citations) add(s Source) {
	if s.ID == "" {
		return
	}
	i := slices.IndexFunc(c.out, func(o Source) bool { return o.ID == s.ID })
	if i < 0 {
		c.out = append(c.out, s)
		return
	}
	o := &c.out[i]
	o.Title = cmp.Or(o.Title, s.Title)
	o.AnalysisID = cmp.Or(o.AnalysisID, s.AnalysisID)
	o.IdeaID = cmp.Or(o.IdeaID, s.IdeaID)
}

// walk reads v, found under key, for what it cites, and its children.
func (c *citations) walk(key string, v any) {
	switch v := v.(type) {
	case map[string]any:
		if c.cites(key, v) {
			return
		}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			c.walk(k, v[k])
		}
	case []any:
		for _, child := range v {
			c.walk(key, child)
		}
	}
}

// cites adds what v, found under key, names as a source, and reports
// whether key is one that names them.
func (c *citations) cites(key string, v map[string]any) bool {
	switch key {
	case "ask":
		for _, e := range objects(v["excerpts"]) {
			c.add(Source{ID: str(e["assetId"]), Title: str(e["title"])})
		}
	case "analysis":
		c.add(Source{ID: str(v["assetId"]), Title: str(v["title"])})
	case "grounding":
		for _, s := range objects(v["sources"]) {
			// A post names where output came from but isn't an asset drawn on.
			if str(s["kind"]) != "" {
				continue
			}
			c.add(Source{ID: str(s["id"]), Title: str(s["title"]), AnalysisID: str(s["analysisId"]), IdeaID: str(s["ideaId"])})
		}
	default:
		return false
	}
	return true
}

// objects is the JSON objects of a decoded list; none when v isn't one.
func objects(v any) []map[string]any {
	list, _ := v.([]any)
	var out []map[string]any
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
