package post_assistant

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

//go:embed prompts/post_assistant.tmpl
var promptFS embed.FS

// PostAssistantFlow is the singleton Genkit flow. Set by InitPostAssistant.
var PostAssistantFlow *core.Flow[PostAssistantRequest, *PostAssistantResponse, struct{}]

// postAssistantRunner is the direct closure that bypasses the Genkit flow
// wrapper, allowing per-request state injection and SSE event streaming.
var postAssistantRunner func(ctx context.Context, req PostAssistantRequest, onEvent OnEventFunc) (*PostAssistantResponse, error)

// InitPostAssistant registers the postAssistant Genkit flow and its tools.
// Must be called after the Genkit instance has been initialised with the
// Anthropic plugin.
func InitPostAssistant(g *genkit.Genkit, cfg PostAssistantFlowConfig, repos PostAssistantRepos) error {
	raw, err := promptFS.ReadFile("prompts/post_assistant.tmpl")
	if err != nil {
		return fmt.Errorf("load post_assistant.tmpl: %w", err)
	}
	tmpl, err := template.New("post_assistant").Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse post_assistant.tmpl: %w", err)
	}
	systemTmpl := tmpl.Lookup("system")
	plannerTmpl := tmpl.Lookup("planner")
	writerTmpl := tmpl.Lookup("writer")
	contextTmpl := tmpl.Lookup("context")
	if systemTmpl == nil || plannerTmpl == nil || writerTmpl == nil || contextTmpl == nil {
		return fmt.Errorf("post_assistant.tmpl must define \"system\", \"planner\", \"writer\", and \"context\" blocks")
	}

	// CON-128: in the hybrid path the orchestration loop runs on the planner
	// system prompt (routing + the editPost tool) and delegates copywriting to
	// the Sonnet writer; the legacy path keeps the single system prompt that
	// writes content inline. Resolve which system prompt the loop uses, and
	// render the (static) writer instructions once for the writer sub-call.
	activeSystemTmpl := systemTmpl
	var writerInstructions string
	if cfg.PlannerEnabled {
		activeSystemTmpl = plannerTmpl
		wi, err := renderTemplate(writerTmpl, contextTemplateData{})
		if err != nil {
			return fmt.Errorf("render writer instructions: %w", err)
		}
		writerInstructions = strings.TrimSpace(wi)
	}

	tools := defineTools(g)

	// CON-112: warm Anthropic's strict-tool grammar cache in the background so
	// the first real request doesn't pay the ~50s compile. Non-blocking.
	if cfg.PrewarmTools {
		go prewarmToolCache(g, cfg, tools)
	}

	PostAssistantFlow = genkit.DefineFlow(g, "postAssistant",
		func(ctx context.Context, req PostAssistantRequest) (*PostAssistantResponse, error) {
			return runPostAssistant(ctx, g, req, cfg, repos, activeSystemTmpl, contextTmpl, writerInstructions, tools, nil)
		},
	)

	postAssistantRunner = func(ctx context.Context, req PostAssistantRequest, onEvent OnEventFunc) (*PostAssistantResponse, error) {
		return runPostAssistant(ctx, g, req, cfg, repos, activeSystemTmpl, contextTmpl, writerInstructions, tools, onEvent)
	}

	return nil
}

// NewPostAssistantCallback returns a callback suitable for passing to the
// posts handler. onEvent is forwarded to the flow for SSE streaming; pass
// nil for a silent, non-streaming call.
func NewPostAssistantCallback() func(ctx context.Context, req PostAssistantRequest, onEvent OnEventFunc) (*PostAssistantResponse, error) {
	return func(ctx context.Context, req PostAssistantRequest, onEvent OnEventFunc) (*PostAssistantResponse, error) {
		return postAssistantRunner(ctx, req, onEvent)
	}
}

// emit calls onEvent when it is non-nil. It is a safe no-op otherwise.
func emit(onEvent OnEventFunc, name SSEEventKind, data any) {
	if onEvent != nil {
		onEvent(name, data)
	}
}

// publishAssistantFinalised announces the end of an assistant run on the
// shared event hub. Topic is "entity:post:<id>"; type is
// "assistant_completed" on success, "assistant_failed" on error.
//
// Used by the SSE event-stream consumer to drive cross-tab notifications
// (the per-request /assistant SSE already serves the requesting tab).
func publishAssistantFinalised(
	hub eventhub.Hub,
	postID, ownerID string,
	resp *PostAssistantResponse,
	err error,
) {
	if hub == nil {
		return
	}
	id, idErr := models.NewID()
	if idErr != nil {
		slog.Error("cannot mint event id", logging.AttrComponent, "genkit.post_assistant", logging.AttrError, idErr)
		return
	}
	ev := eventhub.Event{
		ID:     id,
		Topic:  "entity:post:" + postID,
		UserID: ownerID,
	}
	if err != nil {
		ev.Type = "assistant_failed"
		ev.Payload = map[string]any{
			"postId": postID,
			"error":  err.Error(),
		}
	} else {
		action := ""
		saveVersion := false
		if resp != nil {
			action = resp.Action
			saveVersion = resp.SaveVersion
		}
		ev.Type = "assistant_completed"
		ev.Payload = map[string]any{
			"postId":      postID,
			"action":      action,
			"saveVersion": saveVersion,
		}
	}
	if pubErr := hub.Publish(context.Background(), ev); pubErr != nil {
		slog.Error("hub publish failed", logging.AttrComponent, "genkit.post_assistant", "post_id", postID, logging.AttrError, pubErr)
	}
}

// notifyAssistantFinalised drops a durable "assistant finished / failed"
// notification to the post owner (CON-285): the initiator, who may have walked
// away while the run continued. The client suppresses the live echo for the tab
// that started it (lib/localRuns) — this row is for other devices and a later
// return. The dedupe_key collapses repeats for the same post while still unread,
// so an iterating edit session doesn't flood the inbox (FR7: one row per
// meaningful outcome). Uses a fresh tenant-scoped, bounded context because the
// request ctx may already be cancelled by the time this deferred call runs.
func notifyAssistantFinalised(n *notify.Service, tenantID, ownerID, postID string, resp *PostAssistantResponse, runErr error) {
	if n == nil || ownerID == "" || tenantID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(tenantctx.With(context.Background(), tenantID), 5*time.Second)
	defer cancel()
	spec := notify.Spec{
		EntityType: "post",
		EntityID:   postID,
		ActionURL:  "/posts/" + postID,
	}
	if runErr != nil {
		spec.Level = models.NotificationLevelError
		spec.Type = "assistant.failed"
		spec.Title = "Assistant run failed"
		spec.Body = "The post assistant couldn't finish your request."
		spec.DedupeKey = "assistant.failed:" + postID
	} else {
		action := ""
		if resp != nil {
			action = resp.Action
		}
		spec.Level = models.NotificationLevelSuccess
		spec.Type = "assistant.completed"
		spec.Title = "Assistant finished"
		spec.Body = "The post assistant finished your request."
		spec.Data = map[string]any{"action": action}
		spec.DedupeKey = "assistant.completed:" + postID
	}
	_ = n.Emit(ctx, ownerID, spec)
}
