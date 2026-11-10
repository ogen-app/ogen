// Package more_like_these writes new ideas in the vein of posts that
// performed well: their angle and format, not their words, drawn from the
// ideas and assets behind them. It writes ideas, never posts, so nothing it
// makes reaches the calendar.
package more_like_these

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

const logComponent = "genkit.more_like_these"

const maxOutputTokens int64 = 4096

// Example is a post that performed well.
type Example struct {
	ID       string
	Title    string
	Platform string
	Text     string
}

// Source is something the examples came from, or the campaign may draw on,
// under the ref an idea cites it by ("idea:<id>", "asset:<id>",
// "post:<id>").
type Source struct {
	Ref   string
	Title string
	Text  string
}

// Request is the examples, their sources, and how many ideas to write.
type Request struct {
	Examples    []Example
	Sources     []Source
	Count       int
	Instruction string
}

// Idea is one new idea, with the sources it cites by ref.
type Idea struct {
	Title     string   `json:"title"`
	Angle     string   `json:"angle"`
	Rationale string   `json:"rationale"`
	Theses    []string `json:"theses"`
	Sources   []string `json:"sources"`
}

// Run writes the ideas.
type Run func(ctx context.Context, req Request) ([]Idea, error)

type output struct {
	Ideas []modelIdea `json:"ideas"`
}

type modelIdea struct {
	Title     string   `json:"title"     jsonschema:"description=A short working title for the post the idea would become"`
	Angle     string   `json:"angle"     jsonschema:"description=The angle and hook, one sentence"`
	Rationale string   `json:"rationale" jsonschema:"description=Why it should work, by what the examples did, one sentence"`
	Theses    []string `json:"theses"    jsonschema:"description=3 to 6 short key points the post would make"`
	Sources   []string `json:"sources"   jsonschema:"description=The refs of the sources it draws on, exactly as given; empty when none"`
}

const systemPrompt = `You plan new social posts for a content team, as ideas, not finished copy.
You are given posts that performed well, and the sources behind them: the ideas they were made from, the assets those came from or the posts used, and other sources the campaign may draw on.
Write new ideas in the same vein as the examples — the same kind of angle, hook and format — on new points. Never repeat or paraphrase an example.
Ground each idea in the sources where you can, and cite the refs it draws on, exactly as given. Cite an example's ref when the idea builds on the example alone.
Write in the language of the examples.`

// New returns the writer, on the assistant's more_like_these slot.
func New(g *genkit.Genkit, provider *llm.Provider, recorder *usage.Recorder) Run {
	return func(ctx context.Context, req Request) ([]Idea, error) {
		start := time.Now()
		mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotMoreLike)
		record := func(ctx context.Context, resp *ai.ModelResponse) {
			recorder.RecordResp(ctx, mc.Vendor, mc.Model, "assistant_more_like_these", resp)
		}
		out, resp, err := genkit.GenerateData[output](ctx, g,
			ai.WithModelName(mc.Ref),
			ai.WithSystem("%s", systemPrompt),
			ai.WithPrompt("%s", prompt(req)),
			ai.WithMiddleware(provider.CallMiddleware(modelconfig.FlowAssistant, record)),
			provider.CallConfig(mc.Model, maxOutputTokens),
		)
		if err != nil {
			slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, "examples", len(req.Examples),
				"duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
			return nil, err
		}
		record(ctx, resp)
		return ideas(req, out), nil
	}
}

func prompt(req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write %d new idea(s).\n", max(req.Count, 1))
	if s := strings.TrimSpace(req.Instruction); s != "" {
		fmt.Fprintf(&b, "Steer: %s\n", s)
	}
	for _, e := range req.Examples {
		fmt.Fprintf(&b, "\n<example ref=%q platform=%q title=%q>\n%s\n</example>\n", "post:"+e.ID, e.Platform, e.Title, excerpt(e.Text, 1200))
	}
	for _, s := range req.Sources {
		fmt.Fprintf(&b, "\n<source ref=%q title=%q>\n%s\n</source>\n", s.Ref, s.Title, excerpt(s.Text, 800))
	}
	return b.String()
}

// ideas keeps the model's ideas that have a title, up to the count asked,
// citing only refs it was given.
func ideas(req Request, out *output) []Idea {
	known := map[string]bool{}
	for _, e := range req.Examples {
		known["post:"+e.ID] = true
	}
	for _, s := range req.Sources {
		known[s.Ref] = true
	}
	res := []Idea{}
	for _, m := range out.Ideas {
		if len(res) == max(req.Count, 1) {
			break
		}
		title := strings.TrimSpace(m.Title)
		if title == "" {
			continue
		}
		idea := Idea{Title: title, Angle: strings.TrimSpace(m.Angle), Rationale: strings.TrimSpace(m.Rationale), Sources: []string{}}
		for _, t := range m.Theses {
			if t = strings.TrimSpace(t); t != "" {
				idea.Theses = append(idea.Theses, t)
			}
		}
		for _, r := range m.Sources {
			if r = strings.TrimSpace(r); known[r] && !slices.Contains(idea.Sources, r) {
				idea.Sources = append(idea.Sources, r)
			}
		}
		res = append(res, idea)
	}
	return res
}

func excerpt(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
