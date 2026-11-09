// Package brand_check checks posts against the brand they were written for:
// the voice, the guardrails and the facts they may claim. Banned words are
// found in code; the model reads for the rest.
package brand_check

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
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

const logComponent = "genkit.brand_check"

const maxOutputTokens int64 = 4096

// The rules a finding breaks.
const (
	RuleVoice         = "voice"
	RuleBannedWord    = "banned_word"
	RuleNeverClaim    = "never_claim"
	RuleUnbackedClaim = "unbacked_claim"
	RuleDisclaimer    = "disclaimer"
)

// Post is one post to check.
type Post struct {
	ID       string
	Platform string
	Text     string
}

// Group is posts written for the same brand: Brand is the brand block their
// writer was given, and BannedWords the words no post may use.
type Group struct {
	Brand       string
	BannedWords []string
	Posts       []Post
}

// Request is the posts to check, by brand.
type Request struct {
	Groups []Group
}

// Finding is one way a post breaks its brand.
type Finding struct {
	PostID     string `json:"postId"`
	Rule       string `json:"rule"`
	Severity   string `json:"severity"`
	Quote      string `json:"quote,omitempty"`
	Issue      string `json:"issue"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Result is what the check found, and how many posts it read.
type Result struct {
	Checked  int       `json:"checked"`
	Findings []Finding `json:"findings"`
}

// Run checks the posts.
type Run func(ctx context.Context, req Request) (*Result, error)

type output struct {
	Findings []modelFinding `json:"findings"`
}

type modelFinding struct {
	PostID     string `json:"postId"`
	Rule       string `json:"rule"     jsonschema:"description=voice, never_claim, unbacked_claim or disclaimer"`
	Severity   string `json:"severity" jsonschema:"description=high, medium or low"`
	Quote      string `json:"quote"    jsonschema:"description=The words in the post, verbatim"`
	Issue      string `json:"issue"    jsonschema:"description=What is wrong, one sentence"`
	Suggestion string `json:"suggestion" jsonschema:"description=How to fix it, one sentence"`
}

const systemPrompt = `You check social posts against the brand they were written for, for a content team.
For each post, report only real breaks of the brand given above it:
- voice: the post clearly breaks the voice — its rules (emoji, hashtags, formality, person, length, opening, closing), its channel note, or a register far from its samples. Not taste.
- never_claim: the post makes a claim the guardrails say never to make, in any wording.
- unbacked_claim: the post states a number, award, customer count or other checkable fact about us that no fact in the ledger or may-claim line backs.
- disclaimer: the guardrails give a disclaimer and the post leaves it out.
Quote the post's words exactly. One finding per break; none for a post that is fine.
high: would embarrass or mislead if published; medium: clearly off-brand; low: a small slip.
Write the issue and suggestion in the language of the post.`

// New returns the check, on the assistant's brand_check slot.
func New(g *genkit.Genkit, provider *llm.Provider, recorder *usage.Recorder) Run {
	return func(ctx context.Context, req Request) (*Result, error) {
		res := &Result{Findings: []Finding{}}
		for _, grp := range req.Groups {
			if len(grp.Posts) == 0 {
				continue
			}
			res.Checked += len(grp.Posts)
			res.Findings = append(res.Findings, bannedWords(grp)...)
			found, err := check(ctx, g, provider, recorder, grp)
			if err != nil {
				return nil, err
			}
			res.Findings = append(res.Findings, found...)
		}
		return res, nil
	}
}

func check(ctx context.Context, g *genkit.Genkit, provider *llm.Provider, recorder *usage.Recorder, grp Group) ([]Finding, error) {
	start := time.Now()
	mc := modelconfig.Resolve(ctx, modelconfig.FlowAssistant, modelconfig.SlotBrandCheck)
	record := func(ctx context.Context, resp *ai.ModelResponse) {
		recorder.RecordResp(ctx, mc.Vendor, mc.Model, "assistant_brand_check", resp)
	}
	out, resp, err := genkit.GenerateData[output](ctx, g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem("%s", systemPrompt),
		ai.WithPrompt("%s", prompt(grp)),
		ai.WithMiddleware(provider.CallMiddleware(modelconfig.FlowAssistant, record)),
		provider.CallConfig(mc.Model, maxOutputTokens),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, "posts", len(grp.Posts),
			"duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, err
	}
	record(ctx, resp)
	return findings(grp, out), nil
}

func prompt(grp Group) string {
	var b strings.Builder
	brand := strings.TrimSpace(grp.Brand)
	if brand == "" {
		brand = "(no brand material set)"
	}
	fmt.Fprintf(&b, "<brand>\n%s\n</brand>\n", brand)
	for _, p := range grp.Posts {
		fmt.Fprintf(&b, "\n<post id=%q platform=%q>\n%s\n</post>\n", p.ID, p.Platform, strings.TrimSpace(p.Text))
	}
	return b.String()
}

// findings keeps the model's findings on the group's posts, with a rule it
// may report and the post's own words.
func findings(grp Group, out *output) []Finding {
	text := map[string]string{}
	for _, p := range grp.Posts {
		text[p.ID] = p.Text
	}
	var res []Finding
	for _, f := range out.Findings {
		body, ok := text[f.PostID]
		quote := strings.TrimSpace(f.Quote)
		switch {
		case !ok, !slices.Contains([]string{RuleVoice, RuleNeverClaim, RuleUnbackedClaim, RuleDisclaimer}, f.Rule),
			strings.TrimSpace(f.Issue) == "":
			continue
		case quote != "" && !strings.Contains(body, quote):
			// A quote not in the post is the model's own words: keep the
			// finding, drop the quote.
			quote = ""
		}
		sev := f.Severity
		if !slices.Contains([]string{"high", "medium", "low"}, sev) {
			sev = "medium"
		}
		res = append(res, Finding{PostID: f.PostID, Rule: f.Rule, Severity: sev, Quote: quote,
			Issue: strings.TrimSpace(f.Issue), Suggestion: strings.TrimSpace(f.Suggestion)})
	}
	return res
}

// bannedWords finds each banned word or phrase a post uses, as a whole word,
// whatever its case.
func bannedWords(grp Group) []Finding {
	var res []Finding
	for _, w := range grp.BannedWords {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		re := regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])(` + regexp.QuoteMeta(w) + `)($|[^\p{L}\p{N}])`)
		for _, p := range grp.Posts {
			if m := re.FindStringSubmatch(p.Text); m != nil {
				res = append(res, Finding{PostID: p.ID, Rule: RuleBannedWord, Severity: "high", Quote: m[2],
					Issue: fmt.Sprintf("Uses the banned word %q.", w), Suggestion: "Reword without it."})
			}
		}
	}
	return res
}
