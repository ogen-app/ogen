package assistantkit

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
)

type groundedOut struct {
	Posts []struct {
		Grounding Grounding `json:"grounding"`
	} `json:"posts"`
}

// A generating tool must say what its output is grounded in, however deep
// in its output the grounding sits.
func TestRegistry_GeneratingToolCarriesGrounding(t *testing.T) {
	g := genkit.Init(t.Context())
	bare := Spec{Name: "bare", Pack: "p", Scopes: []Scope{ScopeCampaign}, Tier: TierRead, Guide: "g", Tool: tool(g, "bare"), Generates: true}
	if _, err := NewRegistry(bare); err == nil || !strings.Contains(err.Error(), "must carry grounding") {
		t.Fatalf("generating tool without grounding: %v", err)
	}
	nested := genkit.DefineTool(g, "nested", "nested", func(*ai.ToolContext, noIn) (Outcome[groundedOut], error) {
		return OK(groundedOut{}), nil
	})
	ok := Spec{Name: "nested", Pack: "p", Scopes: []Scope{ScopeCampaign}, Tier: TierRead, Guide: "g", Tool: nested, Generates: true}
	if _, err := NewRegistry(ok); err != nil {
		t.Fatalf("grounding in a result's list: %v", err)
	}
	// A tool that doesn't generate needs none.
	bare.Generates = false
	if _, err := NewRegistry(bare); err != nil {
		t.Fatal(err)
	}
}

// WithAnalyses fills in each source's analysis once per asset, keeps one
// already named, and leaves the sources as they are when the lookup fails.
func TestGrounding_WithAnalyses(t *testing.T) {
	g := GroundingFrom([]Source{{ID: "a1"}, {ID: "a2", AnalysisID: "own"}, {ID: "a1", IdeaID: "i1"}, {ID: "a3"}})
	var asked []string
	got := g.WithAnalyses(func(ids []string) (map[string]string, error) {
		asked = ids
		return map[string]string{"a1": "an1", "a2": "other"}, nil
	})
	want := []Source{{ID: "a1", AnalysisID: "an1"}, {ID: "a2", AnalysisID: "own"}, {ID: "a1", IdeaID: "i1", AnalysisID: "an1"}, {ID: "a3"}}
	if !slices.Equal(got.Sources, want) || !slices.Equal(asked, []string{"a1", "a3"}) || got.Mode != Grounded {
		t.Fatalf("sources = %+v, asked %v", got.Sources, asked)
	}
	if g.Sources[0].AnalysisID != "" {
		t.Fatal("the original grounding was changed")
	}
	failed := g.WithAnalyses(func([]string) (map[string]string, error) { return nil, errors.New("down") })
	if !slices.Equal(failed.Sources, g.Sources) {
		t.Fatalf("failed lookup = %+v", failed.Sources)
	}
	if none := GroundingFrom(nil).WithAnalyses(func([]string) (map[string]string, error) {
		t.Fatal("looked up nothing")
		return nil, nil
	}); none.Mode != Exploratory {
		t.Fatalf("exploratory = %+v", none)
	}
}

// A saved turn's sources are what its tools quoted, read, compared or
// generated from, each once with the analysis and idea it came through; a
// turn that isn't JSON has none.
func TestCited(t *testing.T) {
	turn := `{"explanation":"r","ask":{"excerpts":[{"assetId":"a1","title":"Brochure"}]},
		"results":{"assets":{"ask":{"excerpts":[{"assetId":"a1","title":"Brochure"}]},"analysis":{"assetId":"a2","title":"Deck"},
		"compare":{"grounding":{"mode":"grounded","sources":[{"id":"a3","title":"Notes","analysisId":"an3"}]}}},
		"campaign":{"action":"answered"},
		"ideas":{"compileIdeas":{"grounding":{"mode":"grounded","sources":[{"id":"a1","title":"Brochure","analysisId":"an1","ideaId":"i1"}]}},
			"draftIdeas":{"posts":[{"grounding":{"mode":"grounded","sources":[{"id":"a4","title":"Menu","ideaId":"i4"}]}}]}}}}`
	want := []Source{{ID: "a1", Title: "Brochure", AnalysisID: "an1", IdeaID: "i1"}, {ID: "a2", Title: "Deck"}, {ID: "a3", Title: "Notes", AnalysisID: "an3"}, {ID: "a4", Title: "Menu", IdeaID: "i4"}}
	if got := Cited([]byte(turn)); !slices.Equal(got, want) {
		t.Fatalf("cited = %+v", got)
	}
	if got := Cited([]byte("plain answer")); got != nil {
		t.Fatalf("plain text cited %+v", got)
	}
}
