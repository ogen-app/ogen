package post_quality

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// retryBackoff is the pause before the single retry on a failed or
// invalid evaluation ("1-retry / 2s-backoff convention").
const retryBackoff = 2 * time.Second

// defaultMaxOutputTokens caps each per-dimension scoring call when the
// config leaves MaxOutputTokens at 0. A compact critique is a few hundred
// tokens; the cap leaves room for the low-effort thinking newer models do,
// which counts toward it, and stays far below the Anthropic non-streaming
// limit for the blocking GenerateData call.
const defaultMaxOutputTokens = 2048

// dimensionTargets enumerates the four dimensions and the label shown to the
// model. The order matches the assembly in evaluate.
var dimensionTargets = []struct {
	key   models.EvaluationDimensionKey
	label string
}{
	{models.DimensionCorrectness, "Correctness"},
	{models.DimensionClarity, "Clarity"},
	{models.DimensionEngagement, "Engagement"},
	{models.DimensionDelivery, "Delivery"},
}

// evaluate scores the post ONE dimension per model call and assembles the
// four results. A single nested call (all four dimensions in one schema) has
// the model fully fill only the first sub-object and stub the rest: genkit's
// Anthropic constrained output is prompt-injected, not hard-constrained, so
// the model can satisfy the schema by leaving later objects empty. A focused
// per-dimension call returns one complete object every time. The four calls
// run concurrently and share the cached system prefix.
//
// Each call still uses genkit.GenerateData (native structured output) rather
// than the skill's JSONStringScanner: the scanner drops nested arrays, and a
// dimension carries a suggestion array, so the scanner cannot parse it
// without abandoning the structured-suggestions contract. Field order in
// dimensionOutput keeps rationale before score (guarded by
// TestRationalePrecedesScore).
func evaluate(
	ctx context.Context,
	g *genkit.Genkit,
	cfg PostQualityFlowConfig,
	mc modelconfig.Resolved,
	prompts *renderedPrompts,
) (*assessmentOutput, error) {
	results := make([]dimensionOutput, len(dimensionTargets))
	errs := make([]error, len(dimensionTargets))

	var wg sync.WaitGroup
	for i, t := range dimensionTargets {
		wg.Go(func() {
			results[i], errs[i] = evaluateDimension(ctx, g, cfg, mc, prompts, t.label)
		})
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err // already *AIError
		}
	}

	out := &assessmentOutput{
		Correctness: results[0],
		Clarity:     results[1],
		Engagement:  results[2],
		Delivery:    results[3],
	}
	// Sanitize suggestions across the assembled output (drop span-less / thin
	// suggestions, trim to cap). Score-range is the only hard error, and the
	// focused calls return valid scores, so this does not fail here.
	if err := validateOutput(ctx, out, cfg.SuggestionCap); err != nil {
		return nil, &AIError{Msg: fmt.Sprintf("evaluation failed validation: %v", err)}
	}
	return out, nil
}

// evaluateDimension runs the focused model call for one dimension with the
// 1-retry/2s-backoff convention. It retries on a model error or an empty
// rationale — the signal that the dimension was not actually evaluated; a
// focused call reliably returns one, so this rarely trips.
func evaluateDimension(
	ctx context.Context,
	g *genkit.Genkit,
	cfg PostQualityFlowConfig,
	mc modelconfig.Resolved,
	prompts *renderedPrompts,
	label string,
) (dimensionOutput, error) {
	maxTokens := cfg.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxOutputTokens
	}
	modelName := mc.Ref
	userPrompt := prompts.user + dimensionInstruction(label)
	u := flowkit.Usage{
		Recorder:  cfg.Recorder,
		Model:     mc,
		Feature:   "post_quality",
		Component: "genkit.post_quality",
		Attrs:     []any{"dimension", label},
	}

	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return dimensionOutput{}, ctx.Err()
			case <-time.After(retryBackoff):
			}
		}

		// WithSystem/WithPrompt run their first arg through fmt.Sprintf, so
		// pass the text as a "%s" argument — never as the format string.
		// Otherwise a post containing "6% of", "5% CPU" etc. gets mangled
		// into "6%!o(MISSING)f" (the post text interpreted as fmt verbs).
		out, resp, err := genkit.GenerateData[dimensionOutput](ctx, g,
			ai.WithModelName(modelName),
			ai.WithSystem("%s", prompts.system),
			ai.WithPrompt("%s", userPrompt),
			ai.WithMiddleware(cfg.Provider.CallMiddleware(modelconfig.FlowPostQuality, u.Record, llm.Effort(llm.EffortLow))),
			cfg.Provider.CallConfig(mc.Model, maxTokens),
		)
		u.WarnIfTruncated(ctx, resp, maxTokens)
		if err != nil {
			lastErr = fmt.Errorf("model call: %w", err)
			slog.ErrorContext(ctx, "attempt failed", logging.AttrComponent, "genkit.post_quality", "dimension", label, "attempt", attempt+1, logging.AttrError, lastErr)
			if errors.Is(err, llm.ErrRefused) {
				break // the same request would be refused again
			}
			continue
		}
		// Record every completed call (one per dimension, plus any empty-rationale
		// retry that still consumed tokens). Nil recorder = no-op.
		u.Record(ctx, resp)
		if isBlankProse(out.Verdict) {
			lastErr = fmt.Errorf("empty verdict")
			slog.WarnContext(ctx, "attempt returned empty verdict", logging.AttrComponent, "genkit.post_quality", "dimension", label, "attempt", attempt+1)
			continue
		}
		return *out, nil
	}
	return dimensionOutput{}, &AIError{Msg: fmt.Sprintf("%s evaluation failed after retry: %v", label, lastErr)}
}

// dimensionInstruction is the per-call directive appended to the post-context
// user prompt, naming the single dimension to score.
func dimensionInstruction(label string) string {
	return fmt.Sprintf(
		"\n\nScore ONLY the %s dimension. Write the verdict first (one or two sentences, at most 240 characters), then the 0-10 score, then one or two span-anchored suggestions of one sentence each. Wrap the one phrase the author must not miss in ** and **, once in the verdict and once in each advice.",
		label,
	)
}

// toEvaluationResult maps the model's structured output into the persisted
// result type. Weight and Contribution are left zero here — ComposeScore
// fills them.
func toEvaluationResult(out *assessmentOutput) models.EvaluationResult {
	return models.EvaluationResult{
		Correctness: toDimension(out.Correctness, models.DimensionCorrectness),
		Clarity:     toDimension(out.Clarity, models.DimensionClarity),
		Engagement:  toDimension(out.Engagement, models.DimensionEngagement),
		Delivery:    toDimension(out.Delivery, models.DimensionDelivery),
	}
}

func toDimension(d dimensionOutput, key models.EvaluationDimensionKey) models.EvaluationDimension {
	suggestions := make([]models.EvaluationSuggestion, 0, len(d.Suggestions))
	for _, s := range d.Suggestions {
		suggestions = append(suggestions, models.EvaluationSuggestion{
			Dimension: key, // implied by the parent block; stamped here
			Severity:  models.SuggestionSeverity(s.Severity),
			Span:      s.Span,
			Advice:    s.Advice,
		})
	}
	return models.EvaluationDimension{
		Score:       d.Score,
		Verdict:     d.Verdict,
		Suggestions: suggestions,
	}
}
