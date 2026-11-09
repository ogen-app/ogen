package asset_compare

import (
	"context"
	"strings"
	"testing"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/fakemodel"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

var sources = []Source{
	{ID: "a1", Title: "Villa Aurora brochure", From: FromAnalysis, Text: "Reopens 14 March. Spa open 9:00–21:00."},
	{ID: "a2", Title: "Press kit", From: FromPassages, Text: "Reopening 14 March. New head chef Marta Ruiz."},
}

func run(t *testing.T, req Request, answer string) (*Result, *fakemodel.Model) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	g := genkit.Init(ctx)
	m := fakemodel.Define(g, "compare", fakemodel.Answer(answer))
	fakemodel.Route(ctx, m, fakemodel.Slot{Flow: modelconfig.FlowAssistant, Slot: modelconfig.SlotCompare})
	res, err := New(g, llm.NewProvider(), nil)(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return res, m
}

func TestCompare_KeepsClaimsThatCiteTheSources(t *testing.T) {
	res, m := run(t, Request{Mode: ModeCompare, Focus: "the reopening", Sources: sources}, `{
		"summary": "Both announce the reopening.",
		"shared": [{"text": "Reopening on 14 March", "assetIds": ["a1", "a2", "a1"]}],
		"partial": [{"text": "A new head chef, Marta Ruiz", "assetIds": ["a2"]}, {"text": "Rooftop bar", "assetIds": ["a9"]}],
		"conflicts": [{"text": " ", "assetIds": ["a1"]}]
	}`)
	if len(res.Shared) != 1 || strings.Join(res.Shared[0].AssetIDs, ",") != "a1,a2" {
		t.Fatalf("shared = %+v; want each source once", res.Shared)
	}
	if len(res.Partial) != 1 || res.Partial[0].Text != "A new head chef, Marta Ruiz" {
		t.Fatalf("partial = %+v; a claim citing no given source is dropped", res.Partial)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v; an empty claim is dropped", res.Conflicts)
	}

	prompt := m.Requests()[0].Messages[len(m.Requests()[0].Messages)-1].Text()
	for _, want := range []string{"Compare these sources. Only what they say about: the reopening.",
		`<source id="a1" title="Villa Aurora brochure" from="its analysis">`, `<source id="a2" title="Press kit" from="passages from it">`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
}

func TestSummarise_HasNoClaims(t *testing.T) {
	res, _ := run(t, Request{Mode: ModeSummarise, Sources: sources}, `{"summary": " Both announce the 14 March reopening. ", "shared": [{"text": "x", "assetIds": ["a1"]}], "partial": [], "conflicts": []}`)
	if res.Summary != "Both announce the 14 March reopening." || len(res.Shared) != 0 || res.Partial == nil {
		t.Fatalf("summary = %+v", res)
	}
}

func TestNoSources(t *testing.T) {
	if _, err := New(nil, llm.NewProvider(), nil)(t.Context(), Request{Mode: ModeCompare}); err == nil {
		t.Fatal("ran without sources")
	}
}
