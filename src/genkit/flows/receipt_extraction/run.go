package receipt_extraction

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const logComponent = "genkit.receipt_extraction"

const (
	maxTokens = int64(8192)
	// maxBodyRunes bounds the body sent: a receipt is a page, and anything
	// past this is footer and quoted history.
	maxBodyRunes = 120_000
	// maxPDFBytes bounds the PDF sent, well under the model's document limit.
	maxPDFBytes = 20 << 20
)

// modelCall is one generation the runner asks for.
type modelCall struct {
	Model     modelconfig.Resolved
	System    string
	Prompt    string
	PDF       []byte
	MaxTokens int64
}

// modelReply is a generation's text and what it cost.
type modelReply struct {
	Text                      string
	InputTokens, OutputTokens int64
	CostMicros                int64
}

// generator runs one model call. Production wraps genkit.Generate; tests
// stub it.
type generator func(ctx context.Context, call modelCall) (modelReply, error)

type prompts struct {
	system, request *template.Template
}

type runner struct {
	tpl     prompts
	gen     generator
	resolve func(ctx context.Context, flowKey, slotKey string) modelconfig.Resolved
}

type requestData struct {
	From, ForwardedFrom, Subject, Date string
	Source, Body, PDFName              string
}

// extract reads the email once, with one repair turn when the answer is not
// JSON. When a model call fails or the answer stays unreadable, the response
// still comes back with the error, carrying the usage of the calls made.
func (r *runner) extract(ctx context.Context, req Request) (*Response, error) {
	start := time.Now()
	data := requestData{From: req.From, ForwardedFrom: req.ForwardedFrom, Subject: req.Subject, Date: req.Date}
	var pdf []byte
	switch {
	case strings.TrimSpace(req.HTMLBody) != "":
		data.Source, data.Body = SourceHTML, clamp(compactHTML(req.HTMLBody))
	case strings.TrimSpace(req.TextBody) != "":
		data.Source, data.Body = SourceText, clamp(strings.TrimSpace(req.TextBody))
	case len(req.PDF) > 0 && len(req.PDF) <= maxPDFBytes:
		data.Source, data.PDFName, pdf = SourcePDF, req.PDFName, req.PDF
	case len(req.PDF) > 0:
		return nil, &ValidationError{Msg: fmt.Sprintf("the email has no body and its PDF is over %d MiB", maxPDFBytes>>20)}
	default:
		return nil, &ValidationError{Msg: "the email has no body and no PDF attachment to read"}
	}
	model := r.resolve(ctx, modelconfig.FlowReceiptExtraction, modelconfig.SlotMain)
	system, err := flowkit.RenderTemplate(r.tpl.system, nil)
	if err != nil {
		return nil, fmt.Errorf("render system prompt: %w", err)
	}
	prompt, err := flowkit.RenderTemplate(r.tpl.request, data)
	if err != nil {
		return nil, fmt.Errorf("render request: %w", err)
	}
	call := modelCall{Model: model, System: system, Prompt: prompt, PDF: pdf, MaxTokens: maxTokens}
	resp := &Response{Source: data.Source, Usage: Usage{ModelID: model.Model, PromptVersion: PromptVersion}}

	reply, err := r.generate(ctx, call, &resp.Usage)
	if err != nil {
		return resp, err
	}
	a, parseErr := parseAnswer(reply.Text)
	if parseErr != nil {
		slog.WarnContext(ctx, "reply was not valid JSON; asking for a repair", logging.AttrComponent, logComponent, logging.AttrError, parseErr)
		call.Prompt += "\n\nYour previous reply could not be parsed as JSON (" + parseErr.Error() + "):\n" +
			reply.Text + "\n\nReturn only the corrected JSON object."
		if reply, err = r.generate(ctx, call, &resp.Usage); err != nil {
			return resp, err
		}
		if a, parseErr = parseAnswer(reply.Text); parseErr != nil {
			return resp, &AIError{Msg: "model reply was not valid JSON: " + parseErr.Error(), Invalid: true}
		}
	}
	resp.Extraction = a.extraction()
	slog.InfoContext(ctx, "receipt extracted", logging.AttrComponent, logComponent,
		"source", data.Source, "kind", resp.Extraction.Kind, "lines", len(resp.Extraction.LineItems),
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens,
		"cost_micros", resp.Usage.CostMicros, "duration_ms", time.Since(start).Milliseconds())
	return resp, nil
}

// generate runs a call and adds its cost to u.
func (r *runner) generate(ctx context.Context, call modelCall, u *Usage) (modelReply, error) {
	reply, err := r.gen(ctx, call)
	u.InputTokens += reply.InputTokens
	u.OutputTokens += reply.OutputTokens
	u.CostMicros += reply.CostMicros
	return reply, err
}

func clamp(s string) string {
	if r := []rune(s); len(r) > maxBodyRunes {
		return string(r[:maxBodyRunes])
	}
	return s
}

var (
	htmlDrop    = regexp.MustCompile(`(?is)<(script|style|head|title)\b.*?</(script|style|head|title)\s*>|<!--.*?-->`)
	htmlAttrs   = regexp.MustCompile(`(?s)<([a-zA-Z][a-zA-Z0-9]*)\b[^>]*?(/?)>`)
	htmlNoise   = regexp.MustCompile(`(?i)</?(span|font|center|o:p|meta|link|img)\b[^>]*>`)
	htmlSpace   = regexp.MustCompile(`[ \t\r\f\v]+`)
	htmlNewline = regexp.MustCompile(`\n\s*\n+`)
)

// compactHTML keeps an HTML body's structure (tables, rows, paragraphs) and
// drops what costs tokens without carrying figures: head, styles, scripts,
// comments, attributes and inline wrappers. A receipt's table layout is what
// ties each amount to its label.
func compactHTML(s string) string {
	s = htmlDrop.ReplaceAllString(s, "")
	s = htmlNoise.ReplaceAllString(s, "")
	s = htmlAttrs.ReplaceAllString(s, "<$1$2>")
	s = htmlSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(htmlNewline.ReplaceAllString(s, "\n"))
}
