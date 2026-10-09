package campaign_assistant

import (
	"context"
	"log/slog"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// prewarmToolCache fires one throwaway generation carrying the flow's full tool
// set so Anthropic compiles the strict-tool constrained-decoding grammar and
// caches it. Without it, the first *real* request per ~24h cache TTL
// pays the ~50s compile; warming it in the background at init moves that cost
// off the user path.
//
// It runs through the same tool-order stabilizer transport as real requests, so
// the warmed cache key (the sorted tool schemas) matches what real requests
// send. max_tokens=1 is enough — the grammar is compiled while the request is
// prepared, before any tokens are generated, so no real output (or tool call)
// is produced. Best-effort: any error is logged and swallowed.
func prewarmToolCache(g *genkit.Genkit, cfg CampaignAssistantFlowConfig, t *toolSet) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	start := time.Now()
	mc := modelconfig.Resolve(ctx, modelconfig.FlowCampaignAssistant, modelconfig.SlotOrchestrator)
	_, err := genkit.Generate(ctx, g,
		ai.WithModelName(mc.Ref),
		ai.WithSystem("warmup"),
		ai.WithPrompt("warmup"),
		ai.WithTools(t.all()...),
		ai.WithMaxTurns(1),
		cfg.Provider.CallConfig(mc.Model, 1), // max_tokens: 1 — grammar compiles during prep
	)
	if err != nil {
		slog.Warn("campaign_assistant tool-cache prewarm failed (non-fatal)",
			logging.AttrComponent, "genkit.campaign_assistant",
			"duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return
	}
	slog.Info("campaign_assistant tool-cache prewarmed",
		logging.AttrComponent, "genkit.campaign_assistant",
		"duration_ms", time.Since(start).Milliseconds())
}
