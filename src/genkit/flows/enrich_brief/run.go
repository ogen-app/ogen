package enrich_brief

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// defaultMaxOutputTokens caps a single brief generation. A brief is well
// under this; the cap is just a truncation guard.
const defaultMaxOutputTokens int64 = 32768

func runEnrichBrief(
	ctx context.Context,
	g *genkit.Genkit,
	req EnrichBriefRequest,
	cfg EnrichBriefFlowConfig,
	repos EnrichBriefRepos,
	systemTmpl, contextTmpl *template.Template,
	onEvent OnEventFunc,
) (*EnrichBriefResponse, error) {
	start := time.Now()
	// Log the instruction length, not its content — it is user-provided
	// free text and has no place in operational logs.
	slog.InfoContext(ctx, "starting", logging.AttrComponent, "genkit.enrich_brief", "campaign_id", req.CampaignID, "instruction_len", len(req.Instruction))

	// Enforcement gate (CON-86 FR9): in enforce mode, block before any provider
	// call when the tenant is already over a cap. Nil checker = no gate.
	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}

	// ── Validate ─────────────────────────────────────────────────────────────
	if req.CampaignID == "" {
		return nil, &ValidationError{Msg: "campaign id is required"}
	}
	campaign, err := repos.Campaigns.GetByID(ctx, req.CampaignID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "campaign not found"}
		}
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	if campaign.CampaignTypeID == "" {
		return nil, &ValidationError{Msg: "campaign type is required to enrich the brief"}
	}

	// ── Build context (cached) ───────────────────────────────────────────────
	bctx, err := assembleContextCached(ctx, campaign, req.Instruction, repos, systemTmpl, contextTmpl)
	if err != nil {
		return nil, fmt.Errorf("assemble context: %w", err)
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	maxTokens := cfg.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxOutputTokens
	}
	modelName := modelconfig.Ref(ctx, modelconfig.FlowEnrichBrief, modelconfig.SlotMain)

	// Watch the four brief fields so the client previews each one as it
	// streams. The scanner decodes JSON escapes as they arrive, so the
	// client never sees raw \n / \uXXXX.
	scanner := jsonstream.New(
		[]string{"description", "targetPersona", "keyMessages", "toneGuidelines"},
		func(key, delta string) {
			switch key {
			case "description":
				emit(onEvent, SSEEventDescriptionDelta, DeltaEventPayload{Delta: delta})
			case "targetPersona":
				emit(onEvent, SSEEventPersonaDelta, DeltaEventPayload{Delta: delta})
			case "keyMessages":
				emit(onEvent, SSEEventMessagesDelta, DeltaEventPayload{Delta: delta})
			case "toneGuidelines":
				emit(onEvent, SSEEventToneDelta, DeltaEventPayload{Delta: delta})
			}
		},
	)

	streamCb := func(_ context.Context, chunk *ai.ModelResponseChunk) error {
		// Skip aggregated frames — genkit replays the whole response in a
		// final aggregated chunk, which would double-feed the scanner.
		if chunk == nil || chunk.Aggregated {
			return nil
		}
		for _, part := range chunk.Content {
			if part.IsText() {
				scanner.Push(part.Text)
			}
		}
		return nil
	}

	// ── Generate ─────────────────────────────────────────────────────────────
	// No ai.WithOutputType: genkit's strict post-generation validator drops
	// the whole response on common Claude JSON drift (trailing commas, stray
	// prose, etc.). We parse via the tolerant scanner below instead. Format
	// discipline is enforced by the prompt.
	resp, err := genkit.Generate(ctx, g,
		ai.WithModelName(modelName),
		ai.WithSystem(bctx.SystemPrompt),
		ai.WithPrompt(bctx.ContextBlock),
		ai.WithStreaming(streamCb),
		cfg.Provider.CallConfig(maxTokens),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, "genkit.enrich_brief", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
	}

	if resp.FinishReason == ai.FinishReasonLength {
		var outputTokens int64
		if resp.Usage != nil {
			outputTokens = int64(resp.Usage.OutputTokens)
		}
		slog.WarnContext(ctx, "response truncated at max tokens", logging.AttrComponent, "genkit.enrich_brief", "campaign_id", req.CampaignID, "output_tokens", outputTokens, "cap", maxTokens)
	}
	if resp.Usage != nil {
		slog.InfoContext(ctx, "tokens", logging.AttrComponent, "genkit.enrich_brief", "campaign_id", req.CampaignID, "input", resp.Usage.InputTokens, "output", resp.Usage.OutputTokens, "total", resp.Usage.InputTokens+resp.Usage.OutputTokens)
	}
	cfg.Recorder.RecordResp(ctx, modelconfig.Vendor(ctx, modelconfig.FlowEnrichBrief, modelconfig.SlotMain), modelconfig.Model(ctx, modelconfig.FlowEnrichBrief, modelconfig.SlotMain), "enrich_brief", resp)
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "generate", Status: "done"})

	// ── Assemble response from scanner ───────────────────────────────────────
	// Values() returns the parsed top-level fields without encoding/json,
	// bypassing the whole class of Claude JSON-drift bugs. See scanner_test.go
	// TestValues_* for coverage.
	vals := scanner.Values()
	result := EnrichBriefResponse{}
	if s, ok := vals["description"].(string); ok {
		result.Description = s
	}
	if s, ok := vals["targetPersona"].(string); ok {
		result.TargetPersona = s
	}
	if s, ok := vals["keyMessages"].(string); ok {
		result.KeyMessages = s
	}
	if s, ok := vals["toneGuidelines"].(string); ok {
		result.ToneGuidelines = s
	}

	// Graceful degradation: a brief truncated at max_tokens drops its trailing
	// fields first, so a partial brief is still useful. Fail only when the
	// response is genuinely unusable — every field empty.
	if result.Description == "" && result.TargetPersona == "" &&
		result.KeyMessages == "" && result.ToneGuidelines == "" {
		raw := scanner.FullText()
		slog.ErrorContext(ctx, "scanner found no usable fields", logging.AttrComponent, "genkit.enrich_brief", "campaign_id", req.CampaignID, "len", len(raw), "raw_preview", logging.Preview(raw, 500))
		return nil, &AIError{Msg: "model response did not contain the expected fields"}
	}

	slog.InfoContext(ctx, "done", logging.AttrComponent, "genkit.enrich_brief", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds())

	// The caller emits the single canonical `complete` event from this
	// returned value (mirroring content_plan's GenerateDraft handler). The
	// per-field *_delta events above are preview-only; the client treats
	// `complete` as the source of truth.
	return &result, nil
}
