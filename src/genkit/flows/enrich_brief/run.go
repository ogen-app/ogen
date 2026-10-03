package enrich_brief

import (
	"cmp"
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
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/genkit/jsonstream"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

const logComponent = "genkit.enrich_brief"

// defaultMaxOutputTokens caps a single brief generation. A brief is well
// under this; the cap is just a truncation guard.
const defaultMaxOutputTokens int64 = 32768

// deltaEvents maps each streamed brief field to its preview event.
var deltaEvents = map[string]SSEEventKind{
	"description":    SSEEventDescriptionDelta,
	"targetPersona":  SSEEventPersonaDelta,
	"keyMessages":    SSEEventMessagesDelta,
	"toneGuidelines": SSEEventToneDelta,
}

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
	// The instruction is user free text, so only its length is logged.
	slog.InfoContext(ctx, "starting", logging.AttrComponent, logComponent, "campaign_id", req.CampaignID, "instruction_len", len(req.Instruction))

	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	campaign, err := loadCampaign(ctx, repos, req.CampaignID)
	if err != nil {
		return nil, err
	}
	bctx, err := assembleContext(ctx, campaign, req.Instruction, repos, systemTmpl, contextTmpl)
	if err != nil {
		return nil, fmt.Errorf("assemble context: %w", err)
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "buildContext", Status: "done"})

	scanner, err := generateBrief(ctx, g, cfg, req.CampaignID, bctx, onEvent, start)
	if err != nil {
		return nil, err
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "generate", Status: "done"})

	result, err := decodeBrief(ctx, scanner, req.CampaignID)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "done", logging.AttrComponent, logComponent, "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds())

	// The caller emits the canonical `complete` event from the returned value;
	// the *_delta events are preview-only.
	return result, nil
}

func loadCampaign(ctx context.Context, repos EnrichBriefRepos, campaignID string) (*models.Campaign, error) {
	if campaignID == "" {
		return nil, &ValidationError{Msg: "campaign id is required"}
	}
	campaign, err := repos.Campaigns.GetByID(ctx, campaignID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &ValidationError{Msg: "campaign not found"}
		}
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	if campaign.CampaignTypeID == "" {
		return nil, &ValidationError{Msg: "campaign type is required to enrich the brief"}
	}
	return campaign, nil
}

// generateBrief streams the brief, previewing each field as it arrives. It
// avoids ai.WithOutputType because genkit's strict validator drops the whole
// response on common JSON drift; the returned scanner parses it tolerantly.
func generateBrief(
	ctx context.Context,
	g *genkit.Genkit,
	cfg EnrichBriefFlowConfig,
	campaignID string,
	bctx *briefContext,
	onEvent OnEventFunc,
	start time.Time,
) (*jsonstream.Scanner, error) {
	maxTokens := cmp.Or(cfg.MaxOutputTokens, defaultMaxOutputTokens)
	mc := modelconfig.Resolve(ctx, modelconfig.FlowEnrichBrief, modelconfig.SlotMain)
	scanner := jsonstream.New(
		[]string{"description", "targetPersona", "keyMessages", "toneGuidelines"},
		func(key, delta string) {
			if kind, ok := deltaEvents[key]; ok {
				emit(onEvent, kind, DeltaEventPayload{Delta: delta})
			}
		},
	)

	resp, err := genkit.Generate(ctx, g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem(bctx.SystemPrompt),
		ai.WithPrompt(bctx.ContextBlock),
		ai.WithStreaming(flowkit.StreamCallback(flowkit.StreamHandlers{OnText: scanner.Push})),
		cfg.Provider.CallConfig(maxTokens),
	)
	if err != nil {
		slog.ErrorContext(ctx, "model call failed", logging.AttrComponent, logComponent, "campaign_id", campaignID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, &AIError{Msg: fmt.Sprintf("model call failed: %v", err)}
	}
	flowkit.Usage{
		Recorder:  cfg.Recorder,
		Model:     mc,
		Feature:   "enrich_brief",
		Component: logComponent,
		Attrs:     []any{"campaign_id", campaignID},
	}.Finish(ctx, resp, maxTokens)
	return scanner, nil
}

// decodeBrief reads the brief fields from the scanner. A brief truncated at
// max_tokens drops its trailing fields first and is still useful, so only a
// response with every field empty fails.
func decodeBrief(ctx context.Context, scanner *jsonstream.Scanner, campaignID string) (*EnrichBriefResponse, error) {
	vals := scanner.Values()
	var r EnrichBriefResponse
	r.Description, _ = vals["description"].(string)
	r.TargetPersona, _ = vals["targetPersona"].(string)
	r.KeyMessages, _ = vals["keyMessages"].(string)
	r.ToneGuidelines, _ = vals["toneGuidelines"].(string)

	if r.Description == "" && r.TargetPersona == "" && r.KeyMessages == "" && r.ToneGuidelines == "" {
		raw := scanner.FullText()
		slog.ErrorContext(ctx, "scanner found no usable fields", logging.AttrComponent, logComponent, "campaign_id", campaignID, "len", len(raw), "raw_preview", logging.Preview(raw, 500))
		return nil, &AIError{Msg: "model response did not contain the expected fields"}
	}
	return &r, nil
}
