package post_assistant

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
// caches it (CON-112). Without it, the first *real* request per ~24h cache TTL
// pays the ~50s compile; warming it in the background at init moves that cost
// off the user path.
//
// It runs through the same tool-order stabilizer transport as real requests, so
// the warmed cache key (the sorted tool schemas, under the generation model)
// matches what real requests send. max_tokens=1 is enough — the grammar is
// compiled while the request is prepared, before any tokens are generated, so
// no real output (or tool call) is produced. Best-effort: any error is logged
// and swallowed.
func prewarmToolCache(g *genkit.Genkit, cfg PostAssistantFlowConfig, t *toolSet) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// CON-128: warm the grammar the real loop uses — in the hybrid path that's
	// the planner tool set (incl. editPost) on the planning model; in the legacy
	// path it's the base tool set on the generation model. The writer sub-call
	// carries no tools, so there is no separate grammar to warm for it.
	slot := modelconfig.SlotWriter
	tools := []ai.ToolRef{
		t.listAssets, t.getAssetChunks, t.searchAssetChunks, t.getCurrentContent,
		t.clonePost, t.restoreVersion, t.schedulePost, t.createNote,
	}
	if cfg.PlannerEnabled {
		slot = modelconfig.SlotPlanner
		tools = append(tools, t.editPost)
	}

	start := time.Now()
	_, err := genkit.Generate(ctx, g,
		ai.WithModelName(modelconfig.Ref(ctx, modelconfig.FlowPostAssistant, slot)),
		ai.WithSystem("warmup"),
		ai.WithPrompt("warmup"),
		ai.WithTools(tools...),
		ai.WithMaxTurns(1),
		cfg.Provider.CallConfig(1), // max_tokens: 1 — grammar compiles during prep
	)
	if err != nil {
		slog.Warn("post_assistant tool-cache prewarm failed (non-fatal)",
			logging.AttrComponent, "genkit.post_assistant",
			"duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return
	}
	slog.Info("post_assistant tool-cache prewarmed",
		logging.AttrComponent, "genkit.post_assistant",
		"duration_ms", time.Since(start).Milliseconds())
}
