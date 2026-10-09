package asset_analysis

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

// usageFeature attributes the flow's spend in the usage events.
const usageFeature = "asset_analysis"

// AnalyzeAssetFlow is the singleton Genkit flow, registered for Dev-UI
// discovery. Set by InitAssetAnalysis.
var AnalyzeAssetFlow *core.Flow[AnalyzeRequest, *AnalyzeResponse, struct{}]

// analyzeRunner is the closure NewAnalyzeAssetCallback hands out. Set by
// InitAssetAnalysis.
var analyzeRunner func(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error)

// InitAssetAnalysis parses the prompt templates and registers the
// analyzeAsset flow. Must be called after the Genkit instance has been
// initialised with the Anthropic plugin.
func InitAssetAnalysis(g *genkit.Genkit, cfg FlowConfig) error {
	tpl, err := loadPrompts()
	if err != nil {
		return err
	}
	r := &runner{cfg: cfg, tpl: tpl, gen: genkitGenerator(g, cfg), resolve: modelconfig.Resolve}
	AnalyzeAssetFlow = genkit.DefineFlow(g, "analyzeAsset", r.analyze)
	analyzeRunner = r.analyze
	return nil
}

// NewAnalyzeAssetCallback returns the callback the analysis job invokes.
func NewAnalyzeAssetCallback() func(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error) {
	return func(ctx context.Context, req AnalyzeRequest) (*AnalyzeResponse, error) {
		return analyzeRunner(ctx, req)
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
	p := prompts{
		system:    main.Lookup("system"),
		lens:      main.Lookup("lens"),
		source:    main.Lookup("source"),
		mapSystem: mapTpl.Lookup("system"),
		mapSource: mapTpl.Lookup("source"),
	}
	if p.system == nil || p.lens == nil || p.source == nil {
		return prompts{}, fmt.Errorf("asset_analysis.tmpl must define system, lens and source blocks")
	}
	if p.mapSystem == nil || p.mapSource == nil {
		return prompts{}, fmt.Errorf("asset_analysis_map.tmpl must define system and source blocks")
	}
	return p, nil
}

func parsePrompt(name string) (*template.Template, error) {
	raw, err := promptFS.ReadFile("prompts/" + name + ".tmpl")
	if err != nil {
		return nil, fmt.Errorf("load %s.tmpl: %w", name, err)
	}
	tpl, err := template.New(name).Funcs(templateFuncs).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s.tmpl: %w", name, err)
	}
	return tpl, nil
}

// genkitGenerator runs one blocking generation and meters it. The output is
// read whole rather than streamed: nobody watches an analysis as it is
// written, and the nested arrays need a full JSON decode anyway.
func genkitGenerator(g *genkit.Genkit, cfg FlowConfig) generator {
	maxTokens := cmp.Or(cfg.MaxOutputTokens, defaultMaxOutputTokens)
	return func(ctx context.Context, call modelCall) (string, error) {
		resp, err := genkit.Generate(ctx, g,
			ai.WithModelName(call.Model.Ref),
			ai.WithSystem("%s", call.System),
			ai.WithPrompt("%s", call.Prompt),
			cfg.Provider.CallConfig(maxTokens),
		)
		if err != nil {
			return "", &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
		}
		flowkit.Usage{
			Recorder:  cfg.Recorder,
			Model:     call.Model,
			Feature:   usageFeature,
			Component: logComponent,
		}.Finish(ctx, resp, maxTokens)
		return resp.Text(), nil
	}
}
