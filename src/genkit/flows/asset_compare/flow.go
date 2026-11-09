// Package asset_compare summarises or compares the assets a user picked,
// from what each one's analysis says or, without one, its passages. Every
// claim names the assets it came from, so the reply can cite them.
package asset_compare

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

const logComponent = "genkit.asset_compare"

// maxOutputTokens leaves room for the claims and the thinking Claude 5.x
// models do by default.
const maxOutputTokens int64 = 8192

// Modes of a request.
const (
	ModeSummarise = "summarise"
	ModeCompare   = "compare"
)

// Where a source's text came from.
const (
	FromAnalysis = "analysis"
	FromPassages = "passages"
)

// Source is one asset's material: its analysis summary, or its passages.
type Source struct {
	ID    string
	Title string
	From  string
	Text  string
}

// Request is the assets to summarise or compare.
type Request struct {
	Mode string
	// Focus narrows the reading to what the user asked about; "" for all.
	Focus   string
	Sources []Source
}

// Claim is one statement and the assets it rests on.
type Claim struct {
	Text     string   `json:"text"`
	AssetIDs []string `json:"assetIds"`
}

// Result is the summary, or the comparison: what every asset says, what
// only some say and where they disagree.
type Result struct {
	Summary   string  `json:"summary,omitempty"`
	Shared    []Claim `json:"shared"`
	Partial   []Claim `json:"partial"`
	Conflicts []Claim `json:"conflicts"`
}

// Run is the flow: Request in, Result out.
type Run func(ctx context.Context, req Request) (*Result, error)

type output struct {
	Summary   string  `json:"summary" jsonschema:"description=Summarise: one paragraph over all the sources. Compare: one sentence on how they relate."`
	Shared    []Claim `json:"shared" jsonschema:"description=Compare: what every source says. Empty when summarising."`
	Partial   []Claim `json:"partial" jsonschema:"description=Compare: what only some sources say, with those sources. Empty when summarising."`
	Conflicts []Claim `json:"conflicts" jsonschema:"description=Compare: where sources disagree, both sides in the text, with the sources that disagree. Empty when summarising."`
}

const systemPrompt = `You read a user's own documents and report what they say, for a content team.
Use only the sources given. Never add facts, numbers or names that aren't in them.
Every claim lists the ids of the sources it rests on, exactly as given; a claim no source supports is left out.
Keep each claim to one short sentence in plain words, specific: names, dates, numbers.
When summarising, write one paragraph of at most five sentences and leave the claim lists empty.
When comparing, sort what you find: shared (every source says it), partial (only some do), conflicts (they say different things about the same point: give both sides in the text). At most six claims per list, the most useful first.
Write in the language of the sources.`

// New returns the flow, metered as feature assistant_compare.
func New(g *genkit.Genkit, provider *llm.Provider, recorder *usage.Recorder) Run {
	return func(ctx context.Context, req Request) (*Result, error) {
		if len(req.Sources) == 0 {
			return nil, fmt.Errorf("asset compare: no sources")
		}
		start := time.Now()
		mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotCompare)
		record := func(ctx context.Context, resp *ai.ModelResponse) {
			recorder.RecordResp(ctx, mc.Vendor, mc.Model, "assistant_compare", resp)
		}
		out, resp, err := genkit.GenerateData[output](ctx, g,
			ai.WithModelName(mc.Ref),
			ai.WithSystem("%s", systemPrompt),
			ai.WithPrompt("%s", prompt(req)),
			ai.WithMiddleware(provider.CallMiddleware(modelconfig.FlowAssistant, record)),
			provider.CallConfig(mc.Model, maxOutputTokens),
		)
		if err != nil {
			slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, "sources", len(req.Sources),
				"duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
			return nil, err
		}
		record(ctx, resp)
		return result(req, out), nil
	}
}

// prompt lays the sources out by id, each saying where its text came from.
func prompt(req Request) string {
	var b strings.Builder
	if req.Mode == ModeCompare {
		b.WriteString("Compare these sources.")
	} else {
		b.WriteString("Summarise these sources together.")
	}
	if f := strings.TrimSpace(req.Focus); f != "" {
		fmt.Fprintf(&b, " Only what they say about: %s.", f)
	}
	b.WriteString("\n")
	for _, s := range req.Sources {
		from := "its analysis"
		if s.From == FromPassages {
			from = "passages from it"
		}
		fmt.Fprintf(&b, "\n<source id=%q title=%q from=%q>\n%s\n</source>\n", s.ID, s.Title, from, strings.TrimSpace(s.Text))
	}
	return b.String()
}

// result keeps the claims that cite the request's sources, each source
// once, and drops the claim lists of a summary.
func result(req Request, out *output) *Result {
	known := make([]string, len(req.Sources))
	for i, s := range req.Sources {
		known[i] = s.ID
	}
	res := &Result{Summary: strings.TrimSpace(out.Summary), Shared: []Claim{}, Partial: []Claim{}, Conflicts: []Claim{}}
	if req.Mode != ModeCompare {
		return res
	}
	keep := func(claims []Claim) []Claim {
		kept := []Claim{}
		for _, c := range claims {
			var ids []string
			for _, id := range c.AssetIDs {
				if slices.Contains(known, id) && !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
			if text := strings.TrimSpace(c.Text); text != "" && len(ids) > 0 {
				kept = append(kept, Claim{Text: text, AssetIDs: ids})
			}
		}
		return kept
	}
	res.Shared, res.Partial, res.Conflicts = keep(out.Shared), keep(out.Partial), keep(out.Conflicts)
	return res
}
