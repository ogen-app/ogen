package brand_distill

import (
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

// ExtractFlow and ComposeFlow are the singleton Genkit flows, registered for
// Dev-UI discovery. Set by Init.
var (
	ExtractFlow *core.Flow[ExtractRequest, *ExtractResponse, struct{}]
	ComposeFlow *core.Flow[ComposeRequest, *ComposeResponse, struct{}]
)

// Callbacks are the two stages, bound to the current Genkit instance.
type Callbacks struct {
	Extract func(ctx context.Context, req ExtractRequest) (*ExtractResponse, error)
	Compose func(ctx context.Context, req ComposeRequest) (*ComposeResponse, error)
}

// Init parses the prompts, registers the brandDistillExtract and
// brandDistillCompose flows and returns their callbacks. Must be called after
// the Genkit instance has been initialised with the Anthropic plugin.
func Init(g *genkit.Genkit, cfg FlowConfig) (Callbacks, error) {
	tpl, err := loadPrompts()
	if err != nil {
		return Callbacks{}, err
	}
	r := &runner{cfg: cfg, tpl: tpl, gen: genkitGenerator(g, cfg), resolve: modelconfig.Resolve}
	ExtractFlow = genkit.DefineFlow(g, "brandDistillExtract", r.extract)
	ComposeFlow = genkit.DefineFlow(g, "brandDistillCompose", r.compose)
	return Callbacks{Extract: r.extract, Compose: r.compose}, nil
}

var templateFuncs = template.FuncMap{"join": strings.Join}

func loadPrompts() (prompts, error) {
	tpl := template.New("brand_distill").Funcs(templateFuncs)
	for _, name := range []string{"extract", "compose"} {
		raw, err := promptFS.ReadFile("prompts/" + name + ".tmpl")
		if err != nil {
			return prompts{}, fmt.Errorf("load %s.tmpl: %w", name, err)
		}
		if tpl, err = tpl.Parse(string(raw)); err != nil {
			return prompts{}, fmt.Errorf("parse %s.tmpl: %w", name, err)
		}
	}
	p := prompts{
		extractSystem:  tpl.Lookup("extractSystem"),
		extractPart:    tpl.Lookup("extractPart"),
		composeSystem:  tpl.Lookup("composeSystem"),
		composeRequest: tpl.Lookup("composeRequest"),
	}
	if p.extractSystem == nil || p.extractPart == nil || p.composeSystem == nil || p.composeRequest == nil {
		return prompts{}, fmt.Errorf("brand_distill prompts must define extractSystem, extractPart, composeSystem and composeRequest")
	}
	return p, nil
}

// genkitGenerator runs one generation and meters it. The call streams with
// the chunks dropped: the Anthropic SDK refuses a blocking call whose
// max_tokens could take over 10 minutes, and the JSON is read whole.
func genkitGenerator(g *genkit.Genkit, cfg FlowConfig) generator {
	return func(ctx context.Context, call modelCall) (string, error) {
		usage := flowkit.Usage{
			Recorder:  cfg.Recorder,
			Model:     call.Model,
			Feature:   modelconfig.FlowBrandDistill,
			Component: logComponent,
		}
		resp, err := genkit.Generate(ctx, g,
			ai.WithModelName(call.Model.Ref),
			ai.WithSystem("%s", call.System),
			ai.WithPrompt("%s", call.Prompt),
			ai.WithStreaming(discardChunks),
			ai.WithMiddleware(cfg.Provider.CallMiddleware(modelconfig.FlowBrandDistill, usage.Record)),
			cfg.Provider.CallConfig(call.Model.Model, call.MaxTokens),
		)
		if errors.Is(err, llm.ErrRefused) {
			return "", &AIError{Msg: err.Error(), Refused: true}
		}
		if err != nil {
			return "", &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
		}
		usage.Finish(ctx, resp, call.MaxTokens)
		return resp.Text(), nil
	}
}

func discardChunks(context.Context, *ai.ModelResponseChunk) error { return nil }
