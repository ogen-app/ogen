package assistant

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

//go:embed prompts/assistant.tmpl
var promptFS embed.FS

// Init registers the core tools and the packs', and returns the turn runner.
// It must be called after g has the Anthropic plugin.
func Init(g *genkit.Genkit, cfg FlowConfig, repos Repos, packs ...Pack) (Runner, error) {
	tools, err := defineTools(g, repos, packs)
	if err != nil {
		return nil, fmt.Errorf("register assistant tools: %w", err)
	}
	p, err := loadPrompts(tools)
	if err != nil {
		return nil, err
	}
	if cfg.PrewarmTools {
		go prewarm(g, cfg, tools)
	}
	return func(ctx context.Context, req Request, onEvent OnEventFunc) (*Response, error) {
		return run(ctx, g, req, cfg, repos, p, tools, onEvent)
	}, nil
}

// prompts holds the system prompt of each pack set, rendered once so it
// stays a stable cache prefix, and the per-turn <view> template.
type prompts struct {
	tools        *toolSet
	systemTmpl   *template.Template
	view         *template.Template
	mu           sync.Mutex
	systemByPack map[string]string
}

// system returns the system prompt for the core tools plus loaded, which
// must be normalized.
func (p *prompts) system(loaded []string) (string, error) {
	key := strings.Join(loaded, ",")
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.systemByPack[key]; ok {
		return s, nil
	}
	s, err := flowkit.RenderTemplate(p.systemTmpl, struct{ ToolGuide string }{p.tools.reg.Guide(append([]string{corePack}, loaded...)...)})
	if err != nil {
		return "", fmt.Errorf("render assistant system prompt: %w", err)
	}
	p.systemByPack[key] = s
	return s, nil
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
	p := &prompts{tools: tools, systemTmpl: systemTmpl, view: viewTmpl, systemByPack: map[string]string{}}
	// Render the default sets now, so a broken template fails at init.
	for _, set := range tools.packs.combinations() {
		if _, err := p.system(set); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// prewarm compiles Anthropic's strict-tool grammar for each default pack
// set, one throwaway call each with max_tokens 1, so the first real turn of
// a set doesn't pay the compile. Best-effort: failures are logged.
func prewarm(g *genkit.Genkit, cfg FlowConfig, tools *toolSet) {
	for _, set := range tools.packs.combinations() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		start := time.Now()
		mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotOrchestrator)
		_, err := genkit.Generate(ctx, g,
			ai.WithModelName(mc.Ref),
			ai.WithSystem("warmup"),
			ai.WithPrompt("warmup"),
			ai.WithTools(tools.tools(set)...),
			ai.WithMaxTurns(1),
			cfg.Provider.CallConfig(mc.Model, 1),
		)
		cancel()
		attrs := []any{logging.AttrComponent, logComponent, "packs", strings.Join(set, ","), "duration_ms", time.Since(start).Milliseconds()}
		if err != nil {
			slog.Warn("tool-cache prewarm failed (non-fatal)", append(attrs, logging.AttrError, err)...)
			continue
		}
		slog.Info("tool-cache prewarmed", attrs...)
	}
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
