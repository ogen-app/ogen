package assistantkit

import (
	"fmt"
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

// Source is one piece of the user's material generation drew on: an asset,
// and, when known, the analysis and the idea it reached the output through,
// so the output traces back to its idea and asset.
type Source struct {
	// ID is the asset's.
	ID         string `json:"id"`
	Title      string `json:"title"`
	AnalysisID string `json:"analysisId,omitempty"`
	IdeaID     string `json:"ideaId,omitempty"`
}

// Grounding is attached to every generating tool's result, so the UI can
// badge exploratory output without relying on how the model words its reply.
type Grounding struct {
	Mode    GroundingMode `json:"mode"`
	Sources []Source      `json:"sources,omitempty"`
}

// GroundingFrom is grounded when any source informed the output.
func GroundingFrom(sources []Source) Grounding {
	if len(sources) == 0 {
		return Grounding{Mode: Exploratory}
	}
	return Grounding{Mode: Grounded, Sources: sources}
}

// WithAnalyses fills in the analysis each source's asset has, from lens:
// asset ids to their analysis's id. A source that already names one keeps
// it, and a failed lookup leaves the sources as they are.
func (g Grounding) WithAnalyses(lens func(assetIDs []string) (map[string]string, error)) Grounding {
	var ids []string
	for _, s := range g.Sources {
		if s.AnalysisID == "" && !slices.Contains(ids, s.ID) {
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
		if s.AnalysisID == "" {
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
