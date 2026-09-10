package post_quality

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// retryBackoff is the pause before the single retry on a failed or
// invalid evaluation (CON-85: "1-retry / 2s-backoff convention").
const retryBackoff = 2 * time.Second

// defaultMaxOutputTokens caps the scoring response when the config leaves
// MaxOutputTokens at 0. It is deliberately small: a four-dimension critique
// with capped suggestions runs only a few thousand tokens, and evaluate
// issues a NON-streaming GenerateData call. The Anthropic SDK rejects a
// non-streaming request whose max_tokens implies a >10-minute run — its
// estimate is 1h × max_tokens/128000, so anything above ~21k tokens is
// refused (and Opus-tier models cap non-streaming at 8192). 8192 stays
// under every limit while leaving ample headroom for the response.
const defaultMaxOutputTokens = 8192

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
	prompts *renderedPrompts,
) (*assessmentOutput, error) {
	results := make([]dimensionOutput, len(dimensionTargets))
	errs := make([]error, len(dimensionTargets))

	var wg sync.WaitGroup
	for i, t := range dimensionTargets {
		wg.Go(func() {
			results[i], errs[i] = evaluateDimension(ctx, g, cfg, prompts, t.label)
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
	if err := validateOutput(out, cfg.SuggestionCap); err != nil {
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
	prompts *renderedPrompts,
	label string,
) (dimensionOutput, error) {
	maxTokens := cfg.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxOutputTokens
	}
	modelName := cfg.Provider.Ref(llm.RoleQuality)
	userPrompt := prompts.user + dimensionInstruction(label, cfg.SuggestionCap)

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
			cfg.Provider.CallConfig(maxTokens),
		)
		if resp != nil && resp.FinishReason == ai.FinishReasonLength {
			var outputTokens int64
			if resp.Usage != nil {
				outputTokens = int64(resp.Usage.OutputTokens)
			}
			slog.WarnContext(ctx, "response truncated at max tokens", logging.AttrComponent, "genkit.post_quality", "dimension", label, "output_tokens", outputTokens, "cap", maxTokens)
		}
		if err != nil {
			lastErr = fmt.Errorf("model call: %w", err)
			slog.ErrorContext(ctx, "attempt failed", logging.AttrComponent, "genkit.post_quality", "dimension", label, "attempt", attempt+1, logging.AttrError, lastErr)
			continue
		}
		// Record every completed call (one per dimension, plus any empty-rationale
		// retry that still consumed tokens) (CON-86 FR1). Nil recorder = no-op.
		cfg.Recorder.RecordResp(ctx, cfg.Provider.Vendor(), cfg.Provider.Model(llm.RoleQuality), "post_quality", resp)
		if strings.TrimSpace(out.Rationale) == "" {
			lastErr = fmt.Errorf("empty rationale")
			slog.WarnContext(ctx, "attempt returned empty rationale", logging.AttrComponent, "genkit.post_quality", "dimension", label, "attempt", attempt+1)
			continue
		}
		return *out, nil
	}
	return dimensionOutput{}, &AIError{Msg: fmt.Sprintf("%s evaluation failed after retry: %v", label, lastErr)}
}

// dimensionInstruction is the per-call directive appended to the post-context
// user prompt, naming the single dimension to score.
func dimensionInstruction(label string, suggestionCap int) string {
	return fmt.Sprintf(
		"\n\nScore ONLY the %s dimension against its anchored bands. Write the rationale as articulate feedback addressed to the author (2-4 flowing sentences) before the score, name one concrete weakness, choose a 0-10 score, and give at most %d span-anchored, guidance-only suggestions — all for %s only.",
		label, suggestionCap, label,
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
			Issue:     s.Issue,
			Fix:       s.Fix,
			Span:      s.Span,
		})
	}
	return models.EvaluationDimension{
		Score:       d.Score,
		Rationale:   d.Rationale,
		Weakness:    d.Weakness,
		Suggestions: suggestions,
	}
}
