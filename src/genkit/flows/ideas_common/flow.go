// Package ideas_common names the thread a set of ideas share: a theme of a
// few words and the angle they take, in one sentence. What they share in
// fact — assets, lenses, platforms — is worked out in code; this only words
// it.
package ideas_common

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

const logComponent = "genkit.ideas_common"

const maxOutputTokens int64 = 512

// Idea is one of the ideas compared.
type Idea struct {
	Title string
	Angle string
	Note  string
}

// Request is the ideas to name the thread of.
type Request struct {
	Ideas []Idea
}

// Result is the shared thread: Theme a few words, Angle one sentence.
// Both are empty when the ideas share none.
type Result struct {
	Theme string `json:"theme"`
	Angle string `json:"angle"`
}

// Run names the thread.
type Run func(ctx context.Context, req Request) (*Result, error)

type output struct {
	Theme string `json:"theme" jsonschema:"description=The theme the ideas share, 2 to 5 words, lower case unless a name; empty when they share none"`
	Angle string `json:"angle" jsonschema:"description=The angle they take on it, one sentence; empty when they share none"`
}

const systemPrompt = `You name what a set of social post ideas have in common, for a content team triaging them.
Give the theme they share in a few words, and the angle they take on it in one sentence. Say only what all or most of them share; never invent one.
When they share no theme, leave both empty.
Write in the language of the ideas.`

// New returns the namer, on the assistant's ideas_common slot.
func New(g *genkit.Genkit, provider *llm.Provider, recorder *usage.Recorder) Run {
	return func(ctx context.Context, req Request) (*Result, error) {
		start := time.Now()
		mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotIdeasCommon)
		record := func(ctx context.Context, resp *ai.ModelResponse) {
			recorder.RecordResp(ctx, mc.Vendor, mc.Model, "assistant_ideas_common", resp)
		}
		out, resp, err := genkit.GenerateData[output](ctx, g,
			ai.WithModelName(mc.Ref),
			ai.WithSystem("%s", systemPrompt),
			ai.WithPrompt("%s", prompt(req)),
			ai.WithMiddleware(provider.CallMiddleware(modelconfig.FlowAssistant, record)),
			provider.CallConfig(mc.Model, maxOutputTokens),
		)
		if err != nil {
			slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, "ideas", len(req.Ideas),
				"duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
			return nil, err
		}
		record(ctx, resp)
		return &Result{Theme: strings.TrimSpace(out.Theme), Angle: strings.TrimSpace(out.Angle)}, nil
	}
}

func prompt(req Request) string {
	var b strings.Builder
	for i, idea := range req.Ideas {
		fmt.Fprintf(&b, "<idea n=%d title=%q>\n", i+1, strings.TrimSpace(idea.Title))
		if s := strings.TrimSpace(idea.Angle); s != "" {
			fmt.Fprintf(&b, "%s\n", excerpt(s, 400))
		} else if s := strings.TrimSpace(idea.Note); s != "" {
			fmt.Fprintf(&b, "%s\n", excerpt(s, 400))
		}
		b.WriteString("</idea>\n")
	}
	return b.String()
}

func excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
