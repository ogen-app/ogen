package assistant

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

//go:embed prompts/assistant.tmpl
var promptFS embed.FS

// Init registers the assistant's tools and returns the turn runner. It must be
// called after g has the Anthropic plugin.
func Init(g *genkit.Genkit, cfg FlowConfig, repos Repos) (Runner, error) {
	tools, err := defineTools(g)
	if err != nil {
		return nil, fmt.Errorf("register assistant tools: %w", err)
	}
	p, err := loadPrompts(tools)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, req Request, onEvent OnEventFunc) (*Response, error) {
		return run(ctx, g, req, cfg, repos, p, tools, onEvent)
	}, nil
}

// prompts holds the system prompt, rendered once so it stays a stable cache
// prefix, and the per-turn <view> template.
type prompts struct {
	system string
	view   *template.Template
}

func loadPrompts(tools *toolSet) (*prompts, error) {
	raw, err := promptFS.ReadFile("prompts/assistant.tmpl")
	if err != nil {
		return nil, fmt.Errorf("load assistant.tmpl: %w", err)
	}
	tmpl, err := template.New("assistant").Funcs(template.FuncMap{"inc": func(i int) int { return i + 1 }}).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse assistant.tmpl: %w", err)
	}
	systemTmpl, viewTmpl := tmpl.Lookup("system"), tmpl.Lookup("view")
	if systemTmpl == nil || viewTmpl == nil {
		return nil, fmt.Errorf(`assistant.tmpl must define the {{define "system"}} and {{define "view"}} blocks`)
	}
	system, err := flowkit.RenderTemplate(systemTmpl, struct{ ToolGuide string }{tools.reg.Guide()})
	if err != nil {
		return nil, fmt.Errorf("render assistant system prompt: %w", err)
	}
	return &prompts{system: system, view: viewTmpl}, nil
}

// viewListed caps the selected items the <view> block lists; getSelection
// pages through the rest.
const viewListed = 20

type viewData struct {
	Summary      string
	Today        string
	Focus        string
	Selected     []string
	SelectedMore bool
	Filter       string
	Window       string
	Visible      []string
}

// renderView renders the per-turn <view> block that opens the user's message,
// after the cache breakpoints.
func renderView(p *prompts, v *viewresolve.Resolved, now time.Time) (string, error) {
	d := viewData{Summary: v.Summary, Today: now.Format(time.DateOnly), Filter: v.View.Filter}
	if v.Focus != nil {
		d.Focus = itemLabel(*v.Focus)
	}
	for _, it := range v.Selection[:min(len(v.Selection), viewListed)] {
		d.Selected = append(d.Selected, itemLabel(it))
	}
	d.SelectedMore = v.SelectionTotal > len(d.Selected)
	for _, it := range v.Visible {
		d.Visible = append(d.Visible, itemLabel(it))
	}
	if w := v.View.Window; w != nil {
		d.Window = cmp.Or(w.From, "…") + " to " + cmp.Or(w.To, "…")
		if len(w.Platforms) > 0 {
			d.Window += ", platforms " + strings.Join(w.Platforms, ", ")
		}
	}
	return flowkit.RenderTemplate(p.view, d)
}

func itemLabel(it viewresolve.Item) string {
	if it.Title == "" {
		return fmt.Sprintf("%s (id %s)", it.Kind, it.ID)
	}
	return fmt.Sprintf("%s %q (id %s)", it.Kind, it.Title, it.ID)
}
