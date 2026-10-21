package asset_analysis

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

// AnalyzeAssetFlow and DistillIdeasFlow are the singleton Genkit flows,
// registered for Dev-UI discovery. Set by InitAssetAnalysis.
var (
	AnalyzeAssetFlow *core.Flow[AnalyzeRequest, *AnalyzeResponse, struct{}]
	DistillIdeasFlow *core.Flow[DistillRequest, *DistillResponse, struct{}]
)

// analyzeRunner and distillRunner are the closures the callbacks hand out.
// Set by InitAssetAnalysis.
var (
	analyzeRunner func(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error)
	distillRunner func(ctx context.Context, req DistillRequest) (*DistillResponse, error)
)

// InitAssetAnalysis parses the prompt templates and registers the
// analyzeAsset and distillAssetIdeas flows. Must be called after the Genkit
// instance has been initialised with the Anthropic plugin.
func InitAssetAnalysis(g *genkit.Genkit, cfg FlowConfig) error {
	tpl, err := loadPrompts()
	if err != nil {
		return err
	}
	r := &runner{cfg: cfg, tpl: tpl, gen: genkitGenerator(g, cfg), resolve: modelconfig.Resolve}
	AnalyzeAssetFlow = genkit.DefineFlow(g, "analyzeAsset", r.analyze)
	DistillIdeasFlow = genkit.DefineFlow(g, "distillAssetIdeas", r.distill)
	analyzeRunner, distillRunner = r.analyze, r.distill
	return nil
}

// NewAnalyzeAssetCallback returns the callback the analysis job invokes.
func NewAnalyzeAssetCallback() func(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error) {
	return func(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error) {
		return analyzeRunner(ctx, req)
	}
}

// NewDistillIdeasCallback returns the callback the distill job invokes.
func NewDistillIdeasCallback() func(ctx context.Context, req DistillRequest) (*DistillResponse, error) {
	return func(ctx context.Context, req DistillRequest) (*DistillResponse, error) {
		return distillRunner(ctx, req)
	}
}

var templateFuncs = template.FuncMap{"join": strings.Join}

func loadPrompts() (prompts, error) {
	main, err := parsePrompt("asset_analysis")
	if err != nil {
		return prompts{}, err
	}
	mapTpl, err := parsePrompt("asset_analysis_map")
	if err != nil {
		return prompts{}, err
	}
	// The distill blocks are parsed into a copy of the analysis set so they
	// can use its rubric, lens and source blocks.
	distill, err := main.Clone()
	if err != nil {
		return prompts{}, fmt.Errorf("clone asset_analysis.tmpl: %w", err)
	}
	if distill, err = parseInto(distill, "idea_distill"); err != nil {
		return prompts{}, err
	}
	p := prompts{
		system:         main.Lookup("system"),
		lens:           main.Lookup("lens"),
		source:         main.Lookup("source"),
		mapSystem:      mapTpl.Lookup("system"),
		mapSource:      mapTpl.Lookup("source"),
		distillSystem:  distill.Lookup("distillSystem"),
		distillRequest: distill.Lookup("distillRequest"),
	}
	if p.system == nil || p.lens == nil || p.source == nil {
		return prompts{}, fmt.Errorf("asset_analysis.tmpl must define system, lens and source blocks")
	}
	if p.mapSystem == nil || p.mapSource == nil {
		return prompts{}, fmt.Errorf("asset_analysis_map.tmpl must define system and source blocks")
	}
	if p.distillSystem == nil || p.distillRequest == nil {
		return prompts{}, fmt.Errorf("idea_distill.tmpl must define distillSystem and distillRequest blocks")
	}
	return p, nil
}

func parsePrompt(name string) (*template.Template, error) {
	return parseInto(template.New(name).Funcs(templateFuncs), name)
}

// parseInto parses the named prompt file into tpl's set.
func parseInto(tpl *template.Template, name string) (*template.Template, error) {
	raw, err := promptFS.ReadFile("prompts/" + name + ".tmpl")
	if err != nil {
		return nil, fmt.Errorf("load %s.tmpl: %w", name, err)
	}
	out, err := tpl.Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s.tmpl: %w", name, err)
	}
	return out, nil
}

// genkitGenerator runs one generation and meters it. The call streams, but
// the chunks are dropped and the output is read whole: nobody watches an
// analysis as it is written, and the nested arrays need a full JSON decode
// anyway. Streaming is still required, because the Anthropic SDK refuses a
// blocking call whose max_tokens could take over 10 minutes (above ~21K).
func genkitGenerator(g *genkit.Genkit, cfg FlowConfig) generator {
	defaultMax := cmp.Or(cfg.MaxOutputTokens, defaultMaxOutputTokens)
	return func(ctx context.Context, call modelCall) (string, error) {
		maxTokens := cmp.Or(call.MaxTokens, defaultMax)
		flow := cmp.Or(call.Flow, modelconfig.FlowAssetAnalysis)
		usage := flowkit.Usage{
			Recorder:  cfg.Recorder,
			Model:     call.Model,
			Feature:   flow,
			Component: logComponent,
		}
		resp, err := genkit.Generate(ctx, g,
			ai.WithModelName(call.Model.Ref),
			ai.WithSystem("%s", call.System),
			ai.WithPrompt("%s", call.Prompt),
			ai.WithStreaming(discardChunks),
			ai.WithMiddleware(cfg.Provider.CallMiddleware(flow, usage.Record)),
			cfg.Provider.CallConfig(call.Model.Model, maxTokens),
		)
		if errors.Is(err, llm.ErrRefused) {
			return "", &AIError{Msg: err.Error(), Refused: true}
		}
		if err != nil {
			return "", &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
		}
		usage.Finish(ctx, resp, maxTokens)
		return resp.Text(), nil
	}
}

// discardChunks makes a call stream without using the chunks.
func discardChunks(context.Context, *ai.ModelResponseChunk) error { return nil }
