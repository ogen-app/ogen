package receipt_extraction

import (
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"text/template"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
)

//go:embed prompts/receipt_extraction.tmpl
var promptFS embed.FS

// Flow is the singleton Genkit flow, registered for Dev-UI discovery. Set by
// Init.
var Flow *core.Flow[Request, *Response, struct{}]

// Init parses the prompt, registers the receiptExtraction flow and returns
// its callback. Must be called after the Genkit instance has been
// initialised with the Anthropic plugin.
func Init(g *genkit.Genkit, cfg FlowConfig) (func(ctx context.Context, req Request) (*Response, error), error) {
	tpl, err := loadPrompts()
	if err != nil {
		return nil, err
	}
	r := &runner{tpl: tpl, gen: genkitGenerator(g, cfg), resolve: modelconfig.Resolve}
	Flow = genkit.DefineFlow(g, "receiptExtraction", r.extract)
	return r.extract, nil
}

func loadPrompts() (prompts, error) {
	raw, err := promptFS.ReadFile("prompts/receipt_extraction.tmpl")
	if err != nil {
		return prompts{}, fmt.Errorf("load receipt_extraction.tmpl: %w", err)
	}
	tpl, err := template.New("receipt_extraction").Parse(string(raw))
	if err != nil {
		return prompts{}, fmt.Errorf("parse receipt_extraction.tmpl: %w", err)
	}
	p := prompts{system: tpl.Lookup("system"), request: tpl.Lookup("request")}
	if p.system == nil || p.request == nil {
		return prompts{}, errors.New("receipt_extraction.tmpl must define system and request")
	}
	return p, nil
}

// genkitGenerator runs one generation, with the PDF (when there is one) as a
// document beside the prompt, and prices it from the vendor's rate table.
func genkitGenerator(g *genkit.Genkit, cfg FlowConfig) generator {
	return func(ctx context.Context, call modelCall) (modelReply, error) {
		parts := []*ai.Part{ai.NewTextPart(call.Prompt)}
		if len(call.PDF) > 0 {
			parts = append(parts, ai.NewMediaPart("application/pdf", "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(call.PDF)))
		}
		usage := flowkit.Usage{Model: call.Model, Feature: modelconfig.FlowReceiptExtraction, Component: logComponent}
		var refused modelReply
		resp, err := genkit.Generate(ctx, g,
			ai.WithModelName(call.Model.Ref),
			ai.WithSystem("%s", call.System),
			ai.WithMessages(ai.NewUserMessage(parts...)),
			ai.WithMiddleware(cfg.Provider.CallMiddleware(modelconfig.FlowReceiptExtraction, func(_ context.Context, r *ai.ModelResponse) {
				refused = price(call.Model, r)
			})),
			cfg.Provider.CallConfig(call.Model.Model, call.MaxTokens),
		)
		if errors.Is(err, llm.ErrRefused) {
			return refused, &AIError{Msg: err.Error(), Refused: true}
		}
		if err != nil {
			return modelReply{}, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
		}
		usage.WarnIfTruncated(ctx, resp, call.MaxTokens)
		reply := price(call.Model, resp)
		reply.Text = resp.Text()
		return reply, nil
	}
}

// price reads a response's tokens and costs them with the vendor's meter and
// rate table, the same pricing the usage pipeline applies.
func price(model modelconfig.Resolved, resp *ai.ModelResponse) modelReply {
	if resp == nil {
		return modelReply{}
	}
	d, ok := vendors.Get(model.Vendor)
	if !ok || d.Meter == nil {
		return modelReply{}
	}
	_, u, ok := d.Meter.Extract(resp)
	if !ok {
		return modelReply{}
	}
	cost, _, _ := vendors.CostOf(model.Vendor, model.Model, u)
	return modelReply{
		InputTokens:  u[vendors.KindInput] + u[vendors.KindCacheRead] + u[vendors.KindCacheCreation],
		OutputTokens: u[vendors.KindOutput] + u[vendors.KindReasoning],
		CostMicros:   cost,
	}
}
