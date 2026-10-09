package asset_analysis

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const logComponent = "genkit.asset_analysis"

const (
	defaultFullContextMaxTokens = 150_000
	defaultMaxSourceTokens      = 1_000_000
	defaultMapGroupTokens       = 30_000
	defaultMaxParallelMaps      = 4
	defaultMaxOutputTokens      = int64(32768)
	// minIdeas is the fewest ideas a source of shortSourceWords or more should
	// yield.
	minIdeas         = 5
	shortSourceWords = 300
)

// modelCall is one generation the runner asks for.
type modelCall struct {
	Model  modelconfig.Resolved
	System string
	Prompt string
}

// generator runs one model call and returns its text. Production wraps
// genkit.Generate with metering; tests stub it.
type generator func(ctx context.Context, call modelCall) (string, error)

type prompts struct {
	system, lens, source *template.Template
	mapSystem, mapSource *template.Template
}

type runner struct {
	cfg FlowConfig
	tpl prompts
	gen generator
	// resolve picks a slot's model; modelconfig.Resolve in production.
	resolve func(ctx context.Context, flowKey, slotKey string) modelconfig.Resolved
}

// lensData is the lens template's input: the lens plus the output language.
type lensData struct {
	Lens
	Language string
}

type sourceData struct {
	Title    string
	Notes    bool
	Segments []Segment
}

type mapSourceData struct {
	Title       string
	Part, Parts int
	Segments    []Segment
}

// systemData selects the system prompt variant. It holds only booleans so
// every run of a variant shares one cacheable prefix.
type systemData struct {
	Recording bool
	Campaign  bool
}

func (r *runner) analyze(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error) {
	start := time.Now()
	if err := r.cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	kept, tokens, truncated := budgeted(req.Source.Segments, cmp.Or(r.cfg.MaxSourceTokens, defaultMaxSourceTokens))
	if len(kept) == 0 {
		return nil, &ValidationError{Msg: "source has no text to analyse"}
	}
	resp := &AnalyzeResponse{Strategy: StrategyFull, Truncated: truncated, SourceTokens: tokens}
	// Both models are resolved once so every call of the run uses the same
	// pick even if an operator changes it mid-run.
	analyzeModel := r.resolve(ctx, modelconfig.FlowAssetAnalysis, modelconfig.SlotAnalyze)
	resp.ModelID = analyzeModel.Model

	body := sourceData{Title: req.Source.Title, Segments: kept}
	if tokens > cmp.Or(r.cfg.FullContextMaxTokens, defaultFullContextMaxTokens) {
		mapModel := r.resolve(ctx, modelconfig.FlowAssetAnalysis, modelconfig.SlotMap)
		notes, err := r.mapParts(ctx, req.Source, kept, mapModel)
		if err != nil {
			return nil, err
		}
		body = sourceData{Title: req.Source.Title, Notes: true, Segments: notes}
		resp.Strategy, resp.MapModelID = StrategyMapReduce, mapModel.Model
	}

	analysis, err := r.reduce(ctx, req, body, analyzeModel)
	if err != nil {
		return nil, err
	}
	resp.Analysis = r.cleaner(req, kept).clean(*analysis)
	if len(resp.Analysis.Ideas) < minIdeas && !isShortSource(kept) {
		slog.WarnContext(ctx, "fewer ideas than asked for", logging.AttrComponent, logComponent,
			"asset_id", req.Source.AssetID, "campaign_id", req.CampaignID, "ideas", len(resp.Analysis.Ideas))
	}
	slog.InfoContext(ctx, "analysed", logging.AttrComponent, logComponent,
		"asset_id", req.Source.AssetID, "campaign_id", req.CampaignID, "strategy", resp.Strategy,
		"source_tokens", tokens, "truncated", truncated, "ideas", len(resp.Analysis.Ideas),
		"duration_ms", time.Since(start).Milliseconds())
	return resp, nil
}

// reduce runs the analysis call over body, which is either the source or the
// map notes, and repairs an unparseable reply once.
func (r *runner) reduce(ctx context.Context, req AnalyzeRequest, body sourceData, model modelconfig.Resolved) (*Analysis, error) {
	system, err := flowkit.RenderTemplate(r.tpl.system, systemData{
		Recording: req.Source.Kind == SourceKindRecording,
		Campaign:  req.Lens.Campaign != nil,
	})
	if err != nil {
		return nil, fmt.Errorf("render system prompt: %w", err)
	}
	lens, err := flowkit.RenderTemplate(r.tpl.lens, lensData{Lens: req.Lens, Language: outputLanguage(req.Lens)})
	if err != nil {
		return nil, fmt.Errorf("render lens: %w", err)
	}
	source, err := flowkit.RenderTemplate(r.tpl.source, body)
	if err != nil {
		return nil, fmt.Errorf("render source: %w", err)
	}
	var a Analysis
	if err := r.generateJSON(ctx, modelCall{Model: model, System: system, Prompt: lens + "\n\n" + source}, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// generateJSON runs call and decodes its reply into v. A reply that is not
// valid JSON gets one repair turn; a second failure is an invalid-output
// AIError, since retrying the same prompt would most likely fail again.
func (r *runner) generateJSON(ctx context.Context, call modelCall, v any) error {
	text, err := r.gen(ctx, call)
	if err != nil {
		return err
	}
	parseErr := parseJSON(text, v)
	if parseErr == nil {
		return nil
	}
	slog.WarnContext(ctx, "reply was not valid JSON; asking for a repair", logging.AttrComponent, logComponent,
		"raw_preview", logging.Preview(text, 300), logging.AttrError, parseErr)
	call.Prompt += "\n\nYour previous reply could not be parsed as JSON (" + parseErr.Error() + "):\n" +
		text + "\n\nReturn only the corrected JSON object."
	text, err = r.gen(ctx, call)
	if err != nil {
		return err
	}
	if err := parseJSON(text, v); err != nil {
		return &AIError{Msg: "model reply was not valid JSON: " + err.Error(), Invalid: true}
	}
	return nil
}

// mapNotes is what one map call returns about its part of the source.
type mapNotes struct {
	Notes  string `json:"notes"`
	Quotes []struct {
		Label string `json:"label"`
		Text  string `json:"text"`
	} `json:"quotes"`
	Commitments []Commitment `json:"commitments"`
	Decisions   []Decision   `json:"decisions"`
	Ideas       []Idea       `json:"ideas"`
}

// mapParts takes notes on each part of a long source in parallel and returns
// them, in source order, as the segments the reduce call reads.
func (r *runner) mapParts(ctx context.Context, src Source, kept []Segment, model modelconfig.Resolved) ([]Segment, error) {
	parts := groups(kept, cmp.Or(r.cfg.MapGroupTokens, defaultMapGroupTokens))
	system, err := flowkit.RenderTemplate(r.tpl.mapSystem, systemData{Recording: src.Kind == SourceKindRecording})
	if err != nil {
		return nil, fmt.Errorf("render map system prompt: %w", err)
	}
	notes := make([]Segment, len(parts))
	errs := make([]error, len(parts))
	sem := make(chan struct{}, cmp.Or(r.cfg.MaxParallelMaps, defaultMaxParallelMaps))
	var wg sync.WaitGroup
	for i, part := range parts {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			notes[i], errs[i] = r.mapPart(ctx, system, src.Title, i+1, len(parts), part, model)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return notes, nil
}

func (r *runner) mapPart(ctx context.Context, system, title string, part, parts int, segs []Segment, model modelconfig.Resolved) (Segment, error) {
	prompt, err := flowkit.RenderTemplate(r.tpl.mapSource, mapSourceData{Title: title, Part: part, Parts: parts, Segments: segs})
	if err != nil {
		return Segment{}, fmt.Errorf("render map part %d: %w", part, err)
	}
	text, err := r.gen(ctx, modelCall{Model: model, System: system, Prompt: prompt})
	if err != nil {
		return Segment{}, err
	}
	var n mapNotes
	if err := parseJSON(text, &n); err != nil {
		// Unparseable notes are still notes; the reduce call reads them as is.
		return Segment{Text: fmt.Sprintf("### Part %d\n%s", part, strings.TrimSpace(text))}, nil
	}
	return Segment{Text: n.render(part)}, nil
}

func (n mapNotes) render(part int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### Part %d\n%s\n", part, strings.TrimSpace(n.Notes))
	for _, q := range n.Quotes {
		fmt.Fprintf(&b, "> [%s] %s\n", q.Label, q.Text)
	}
	for _, c := range n.Commitments {
		fmt.Fprintf(&b, "- Commitment: %s — %s", cmp.Or(c.Owner, unattributed), c.Commitment)
		if c.Due != "" {
			fmt.Fprintf(&b, " (due %s)", c.Due)
		}
		fmt.Fprintf(&b, " [%s]\n", c.SourceLabel)
	}
	for _, d := range n.Decisions {
		fmt.Fprintf(&b, "- Decision: %s [%s]\n", d.Text, d.SourceLabel)
	}
	for _, i := range n.Ideas {
		fmt.Fprintf(&b, "- Idea: %s — %s [%s]\n", i.Title, i.Angle, strings.Join(i.SourceLabels, ", "))
	}
	return b.String()
}

// cleaner builds the output clean-up for req over the source that was read.
func (r *runner) cleaner(req AnalyzeRequest, read []Segment) cleaner {
	existing := make(map[string]bool, len(req.Lens.ExistingIdeas))
	for _, t := range req.Lens.ExistingIdeas {
		existing[normalizeTitle(t)] = true
	}
	return cleaner{
		labels:    labelSet(read),
		recording: req.Source.Kind == SourceKindRecording,
		campaign:  req.Lens.Campaign != nil,
		existing:  existing,
	}
}

// outputLanguage is the language a lens writes in: the campaign's for a
// campaign lens, otherwise the source's own (empty).
func outputLanguage(l Lens) string {
	if l.Campaign != nil {
		return l.Campaign.Language
	}
	return ""
}

// isShortSource reports whether a source is short enough that fewer than
// minIdeas ideas are an acceptable outcome.
func isShortSource(segments []Segment) bool {
	return wordCount(segments) < shortSourceWords
}
