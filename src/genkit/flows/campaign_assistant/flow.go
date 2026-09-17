package campaign_assistant

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
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

//go:embed prompts/campaign_assistant.tmpl
var promptFS embed.FS

// CampaignAssistantFlow is the singleton Genkit flow. Set by
// InitCampaignAssistant. Registered for Dev-UI discovery; the SSE path uses the
// runner closure below so it can stream events.
var CampaignAssistantFlow *core.Flow[CampaignAssistantRequest, *CampaignAssistantResponse, struct{}]

// campaignAssistantRunner is the direct closure that threads an OnEventFunc for
// SSE streaming. Set by InitCampaignAssistant.
var campaignAssistantRunner func(ctx context.Context, req CampaignAssistantRequest, onEvent OnEventFunc) (*CampaignAssistantResponse, error)

// InitCampaignAssistant parses the prompt template, registers the tools, and
// registers the campaignAssistant Genkit flow. Must be called after the Genkit
// instance has been initialised with the Anthropic plugin.
func InitCampaignAssistant(g *genkit.Genkit, cfg CampaignAssistantFlowConfig, repos CampaignAssistantRepos) error {
	raw, err := promptFS.ReadFile("prompts/campaign_assistant.tmpl")
	if err != nil {
		return fmt.Errorf("load campaign_assistant.tmpl: %w", err)
	}
	tmpl, err := template.New("campaign_assistant").Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse campaign_assistant.tmpl: %w", err)
	}
	systemTmpl := tmpl.Lookup("system")
	contextTmpl := tmpl.Lookup("context")
	if systemTmpl == nil || contextTmpl == nil {
		return fmt.Errorf("campaign_assistant.tmpl must define both {{define \"system\"}} and {{define \"context\"}} blocks")
	}

	tools := defineTools(g)

	// CON-112: warm Anthropic's strict-tool grammar cache in the background so
	// the first real request doesn't pay the ~50s compile. Non-blocking.
	if cfg.PrewarmTools {
		go prewarmToolCache(g, cfg, tools)
	}

	CampaignAssistantFlow = genkit.DefineFlow(g, "campaignAssistant",
		func(ctx context.Context, req CampaignAssistantRequest) (*CampaignAssistantResponse, error) {
			return runCampaignAssistant(ctx, g, req, cfg, repos, systemTmpl, contextTmpl, tools, nil)
		},
	)

	campaignAssistantRunner = func(ctx context.Context, req CampaignAssistantRequest, onEvent OnEventFunc) (*CampaignAssistantResponse, error) {
		return runCampaignAssistant(ctx, g, req, cfg, repos, systemTmpl, contextTmpl, tools, onEvent)
	}

	return nil
}

// NewCampaignAssistantCallback returns a callback suitable for passing to the
// campaigns handler. onEvent is forwarded to the flow for SSE streaming; pass
// nil for a silent, non-streaming call.
func NewCampaignAssistantCallback() func(ctx context.Context, req CampaignAssistantRequest, onEvent OnEventFunc) (*CampaignAssistantResponse, error) {
	return func(ctx context.Context, req CampaignAssistantRequest, onEvent OnEventFunc) (*CampaignAssistantResponse, error) {
		return campaignAssistantRunner(ctx, req, onEvent)
	}
}

// emit calls onEvent when it is non-nil. It is a safe no-op otherwise.
func emit(onEvent OnEventFunc, name SSEEventKind, data any) {
	if onEvent != nil {
		onEvent(name, data)
	}
}

// publishAssistantFinalised announces the end of an assistant run on the shared
// event hub. Topic is "entity:campaign:<id>"; type is "assistant.completed" on
// success, "assistant.failed" on error (dotted convention, CON-285) — driving
// cross-tab notifications.
func publishAssistantFinalised(
	hub eventhub.Hub,
	campaignID, ownerID string,
	resp *CampaignAssistantResponse,
	err error,
) {
	if hub == nil {
		return
	}
	id, idErr := models.NewID()
	if idErr != nil {
		slog.Error("cannot mint event id", logging.AttrComponent, "genkit.campaign_assistant", logging.AttrError, idErr)
		return
	}
	ev := eventhub.Event{
		ID:     id,
		Topic:  "entity:campaign:" + campaignID,
		UserID: ownerID,
	}
	if err != nil {
		ev.Type = "assistant.failed"
		ev.Payload = map[string]any{
			"campaignId": campaignID,
			"error":      err.Error(),
		}
	} else {
		action := ""
		if resp != nil {
			action = resp.Action
		}
		ev.Type = "assistant.completed"
		ev.Payload = map[string]any{
			"campaignId": campaignID,
			"action":     action,
		}
	}
	if pubErr := hub.Publish(context.Background(), ev); pubErr != nil {
		slog.Error("hub publish failed", logging.AttrComponent, "genkit.campaign_assistant", "campaign_id", campaignID, logging.AttrError, pubErr)
	}
}

// notifyContentPlanReady drops a persistent "content plan ready" notification to
// the campaign owner (CON-242) — but ONLY when this run generated a content plan
// (resp.Action == "content_plan_generated"), so an ordinary chat turn never
// spams the inbox. Best-effort: a nil notifier, a run error, or a non-plan turn
// is a silent no-op. Uses a fresh tenant-scoped background context because the
// request ctx may already be cancelled by the time this deferred call runs.
func notifyContentPlanReady(n *notify.Service, tenantID, ownerID, campaignID string, resp *CampaignAssistantResponse, runErr error) {
	if n == nil || runErr != nil || resp == nil || ownerID == "" || tenantID == "" {
		return
	}
	if resp.Action != "content_plan_generated" || resp.ContentPlan == nil {
		return
	}
	// Bound this best-effort persistence: it runs in a deferred call on the
	// request path, so an unbounded background insert could stall the response
	// if the DB is slow. Keep the tenant scope; just add a deadline + cancel.
	ctx, cancel := context.WithTimeout(tenantctx.With(context.Background(), tenantID), 5*time.Second)
	defer cancel()
	postCount := resp.ContentPlan.PostCount
	_ = n.Emit(ctx, ownerID, notify.Spec{
		Level:      models.NotificationLevelSuccess,
		Type:       "campaign.content_plan_ready",
		Title:      "Content plan ready",
		Body:       fmt.Sprintf("Your content plan is ready — %d post(s) drafted.", postCount),
		EntityType: "campaign",
		EntityID:   campaignID,
		ActionURL:  "/campaigns/" + campaignID,
		Data:       map[string]any{"post_count": postCount},
	})
}

// notifyAssistantFinalised drops a durable "assistant finished / failed"
// notification to the campaign owner (CON-285): the initiator, who may have
// walked away while the run continued. The client suppresses the live echo for
// the tab that started it (lib/localRuns) — this row is for other devices and a
// later return. The dedupe_key collapses repeats for the same campaign while
// still unread, so an iterating chat doesn't flood the inbox (FR7: one row per
// meaningful outcome). The success case for a generated content plan is skipped:
// campaign.content_plan_ready already announces that specific outcome.
func notifyAssistantFinalised(n *notify.Service, tenantID, ownerID, campaignID string, resp *CampaignAssistantResponse, runErr error) {
	if n == nil || ownerID == "" || tenantID == "" {
		return
	}
	action := ""
	if resp != nil {
		action = resp.Action
	}
	if runErr == nil && action == "content_plan_generated" {
		return // covered by notifyContentPlanReady
	}
	ctx, cancel := context.WithTimeout(tenantctx.With(context.Background(), tenantID), 5*time.Second)
	defer cancel()
	spec := notify.Spec{
		EntityType: "campaign",
		EntityID:   campaignID,
		ActionURL:  "/campaigns/" + campaignID,
	}
	if runErr != nil {
		spec.Level = models.NotificationLevelError
		spec.Type = "assistant.failed"
		spec.Title = "Assistant run failed"
		spec.Body = "The campaign assistant couldn't finish your request."
		spec.DedupeKey = "assistant.failed:" + campaignID
	} else {
		spec.Level = models.NotificationLevelSuccess
		spec.Type = "assistant.completed"
		spec.Title = "Assistant finished"
		spec.Body = "The campaign assistant finished your request."
		spec.Data = map[string]any{"action": action}
		spec.DedupeKey = "assistant.completed:" + campaignID
	}
	_ = n.Emit(ctx, ownerID, spec)
}
