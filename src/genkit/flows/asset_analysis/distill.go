package asset_analysis

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// defaultDistillMaxOutputTokens fits ten rated ideas with room to spare, and
// keeps the call well under the size the Anthropic SDK refuses to block on.
const defaultDistillMaxOutputTokens = int64(8192)

// coveredCitations is how many existing ideas must cite a segment before the
// prompt calls it well covered.
const coveredCitations = 2

// distillSystemData is the distill system prompt's input. Count changes per
// run, so unlike the analysis system prompt it is not one cacheable prefix
// per variant.
type distillSystemData struct {
	systemData
	Count int
}

type distillRequestData struct {
	Prior   []PriorIdea
	Covered []string
	Focus   string
	Count   int
	// Partial is set when only part of the source is sent; Summary and
	// KeyPoints then say what the whole is about.
	Partial   bool
	Summary   string
	KeyPoints []string
}

// distill asks for Count more ideas from a source, shown the ideas the team
// already has. It is one model call: a source too long for one is cut to the
// parts the existing ideas cite least.
func (r *runner) distill(ctx context.Context, req DistillRequest) (*DistillResponse, error) {
	start := time.Now()
	if err := r.cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	count := min(max(req.Count, 1), maxIdeas)
	kept, tokens, _ := budgeted(req.Source.Segments, cmp.Or(r.cfg.MaxSourceTokens, defaultMaxSourceTokens))
	if len(kept) == 0 {
		return nil, &ValidationError{Msg: "source has no text to distill"}
	}
	model := r.resolve(ctx, modelconfig.FlowIdeaDistill, modelconfig.SlotDistill)
	resp := &DistillResponse{Strategy: StrategyFull, ModelID: model.Model}
	cites := citations(req.Prior)
	sent := kept
	if limit := cmp.Or(r.cfg.FullContextMaxTokens, defaultFullContextMaxTokens); tokens > limit {
		sent, tokens = leastCited(kept, cites, cmp.Or(r.cfg.MapGroupTokens, defaultMapGroupTokens), limit)
		resp.Strategy = StrategyCoverage
	}
	resp.SourceTokens = tokens

	call, err := r.distillCall(req, sent, cites, count, resp.Strategy == StrategyCoverage)
	if err != nil {
		return nil, err
	}
	call.Model = model
	var out struct {
		Ideas []Idea `json:"ideas"`
	}
	if err := r.generateJSON(ctx, call, &out); err != nil {
		return nil, err
	}
	known := AnalyzeRequest{Source: req.Source, Lens: req.Lens}
	known.Lens.ExistingIdeas = req.KnownTitles
	ideas := r.cleaner(known, sent).ideas(out.Ideas)
	resp.Ideas = ideas[:min(len(ideas), count)]
	resp.Dropped = len(out.Ideas) - len(resp.Ideas)
	slog.InfoContext(ctx, "distilled ideas", logging.AttrComponent, logComponent,
		"asset_id", req.Source.AssetID, "campaign_id", req.CampaignID, "strategy", resp.Strategy,
		"source_tokens", tokens, "prior_ideas", len(req.Prior), "asked", count,
		"kept", len(resp.Ideas), "dropped", resp.Dropped, "duration_ms", time.Since(start).Milliseconds())
	return resp, nil
}

// distillCall renders the prompts of a distill run over the sent segments.
func (r *runner) distillCall(req DistillRequest, sent []Segment, cites map[string]int, count int, partial bool) (modelCall, error) {
	campaign := req.Lens.Campaign != nil
	system, err := flowkit.RenderTemplate(r.tpl.distillSystem, distillSystemData{
		systemData: systemData{
			Recording: req.Source.Kind == SourceKindRecording,
			Campaign:  campaign,
			Rubric:    rubric(campaign, hasPhases(req.Lens)),
		},
		Count: count,
	})
	if err != nil {
		return modelCall{}, fmt.Errorf("render distill system prompt: %w", err)
	}
	lens := req.Lens
	lens.ExistingIdeas = nil
	lensText, err := flowkit.RenderTemplate(r.tpl.lens, lensData{Lens: lens, Language: outputLanguage(lens)})
	if err != nil {
		return modelCall{}, fmt.Errorf("render lens: %w", err)
	}
	data := distillRequestData{
		Prior:   req.Prior,
		Covered: covered(sent, cites),
		Focus:   sanitizeFocus(req.Focus),
		Count:   count,
		Partial: partial,
	}
	if partial {
		data.Summary, data.KeyPoints = req.Summary, req.KeyPoints
	}
	requestText, err := flowkit.RenderTemplate(r.tpl.distillRequest, data)
	if err != nil {
		return modelCall{}, fmt.Errorf("render distill request: %w", err)
	}
	source, err := flowkit.RenderTemplate(r.tpl.source, sourceData{Title: req.Source.Title, Segments: sent})
	if err != nil {
		return modelCall{}, fmt.Errorf("render source: %w", err)
	}
	return modelCall{
		System:    system,
		Prompt:    lensText + "\n\n" + requestText + "\n\n" + source,
		Flow:      modelconfig.FlowIdeaDistill,
		MaxTokens: cmp.Or(r.cfg.DistillMaxOutputTokens, defaultDistillMaxOutputTokens),
	}, nil
}

// citations counts how many prior ideas cite each segment label.
func citations(prior []PriorIdea) map[string]int {
	out := map[string]int{}
	for _, p := range prior {
		for _, label := range p.Sources {
			out[label]++
		}
	}
	return out
}

// covered lists, in source order, the sent labels at least coveredCitations
// prior ideas cite.
func covered(sent []Segment, cites map[string]int) []string {
	var out []string
	for _, s := range sent {
		if s.Label != "" && cites[s.Label] >= coveredCitations && !slices.Contains(out, s.Label) {
			out = append(out, s.Label)
		}
	}
	return out
}

// leastCited splits segments into groups of about size tokens and keeps the
// groups the prior ideas cite least, earlier groups first on a tie, until
// limit tokens are used. The kept groups stay in source order. At least one
// group is always kept.
func leastCited(segments []Segment, cites map[string]int, size, limit int) ([]Segment, int) {
	parts := groups(segments, size)
	type ranked struct{ index, cites, tokens int }
	order := make([]ranked, len(parts))
	for i, part := range parts {
		order[i].index = i
		for _, s := range part {
			order[i].cites += cites[s.Label]
			order[i].tokens += segmentTokens(s)
		}
	}
	slices.SortStableFunc(order, func(a, b ranked) int { return cmp.Compare(a.cites, b.cites) })
	var picked []int
	used := 0
	for _, p := range order {
		if len(picked) > 0 && used+p.tokens > limit {
			continue
		}
		picked = append(picked, p.index)
		used += p.tokens
	}
	slices.Sort(picked)
	var out []Segment
	for _, i := range picked {
		out = append(out, parts[i]...)
	}
	return out, used
}

// focusTags keeps a focus from closing or opening the prompt's own tags.
var focusTags = strings.NewReplacer("<", "‹", ">", "›")

func sanitizeFocus(focus string) string {
	return focusTags.Replace(strings.TrimSpace(focus))
}
