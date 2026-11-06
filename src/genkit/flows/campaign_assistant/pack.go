package campaign_assistant

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/flowkit"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/brandresolve"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// PackName is the campaign pack's name.
const PackName = campaignPack

const logComponent = "genkit.campaign_pack"

// emit calls onEvent when it is non-nil.
func emit(onEvent OnEventFunc, name SSEEventKind, data any) {
	if onEvent != nil {
		onEvent(name, data)
	}
}

//go:embed prompts/campaign_assistant.tmpl
var promptFS embed.FS

// prompts holds the pack's rules and the templates of its per-turn block.
type prompts struct {
	rules   string
	context *template.Template
	state   *template.Template
}

func loadPrompts() (*prompts, error) {
	raw, err := promptFS.ReadFile("prompts/campaign_assistant.tmpl")
	if err != nil {
		return nil, fmt.Errorf("load campaign_assistant.tmpl: %w", err)
	}
	tmpl, err := template.New("campaign_assistant").Funcs(template.FuncMap{"join": strings.Join}).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse campaign_assistant.tmpl: %w", err)
	}
	rulesTmpl, contextTmpl, stateTmpl := tmpl.Lookup("rules"), tmpl.Lookup("context"), tmpl.Lookup("state")
	if rulesTmpl == nil || contextTmpl == nil || stateTmpl == nil {
		return nil, fmt.Errorf(`campaign_assistant.tmpl must define the {{define "rules"}}, {{define "context"}} and {{define "state"}} blocks`)
	}
	rules, err := flowkit.RenderTemplate(rulesTmpl, nil)
	if err != nil {
		return nil, fmt.Errorf("render campaign rules: %w", err)
	}
	return &prompts{rules: rules, context: contextTmpl, state: stateTmpl}, nil
}

// NewPack is the campaign pack: the work on one campaign — its content
// plan, ideas for a platform, brief, dates, platforms, drafts from research,
// redistribution and consistency checks. It works on the thread's campaign,
// or the one in focus.
func NewPack(cfg CampaignAssistantFlowConfig, repos CampaignAssistantRepos) (assistant.Pack, error) {
	p, err := loadPrompts()
	if err != nil {
		return assistant.Pack{}, err
	}
	return assistant.Pack{
		Name:         PackName,
		Summary:      "plan a campaign's content and ideas, edit its brief, dates and platforms, draft posts from research, redistribute its posts and check its consistency",
		Scopes:       []models.AssistantScopeKind{models.AssistantScopeCampaign},
		Instructions: p.rules,
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			tools, err := defineTools(g)
			if err != nil {
				return nil, err
			}
			return tools.reg.Specs(), nil
		},
		Begin: func(ctx context.Context, t assistant.Turn) (context.Context, string, error) {
			return begin(ctx, cfg, repos, p, t)
		},
		End: end,
	}, nil
}

// begin loads the turn's campaign and readies its tools; the block is the
// campaign's brief and state.
func begin(ctx context.Context, cfg CampaignAssistantFlowConfig, repos CampaignAssistantRepos, p *prompts, t assistant.Turn) (context.Context, string, error) {
	id := t.Subject(models.AssistantScopeCampaign)
	if id == "" {
		return nil, "", assistantkit.Refused("this message isn't about one campaign; open the campaign, or ask the user which one")
	}
	campaign, err := repos.Campaigns.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", assistantkit.Refused("that campaign no longer exists")
	}
	if err != nil {
		return nil, "", fmt.Errorf("load campaign: %w", err)
	}
	// One Brand library load serves this turn and every sub-flow its tools run.
	ctx = brandresolve.WithMemo(ctx)
	brand, err := brandresolve.Resolve(ctx, repos.Brands, campaign, nil)
	if err != nil {
		slog.WarnContext(ctx, "brand resolve failed; using legacy tone", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
	brief, err := renderContext(campaign, brand.PromptBlock(""), p)
	if err != nil {
		return nil, "", fmt.Errorf("render campaign context: %w", err)
	}
	snap := snapshot(ctx, repos, campaign)
	stateBlock, err := renderState(p, snap)
	if err != nil {
		return nil, "", fmt.Errorf("render campaign state: %w", err)
	}
	// The campaign's tools stream their progress (the plan's ideas, the
	// brief as it is rewritten) to the client through the turn.
	st := newRequestState(cfg, repos, campaign, t.Instruction, func(kind SSEEventKind, data any) {
		assistant.Emit(ctx, string(kind), data)
	})
	st.snapshot = snap
	st.hub, st.notifier = cfg.Hub, cfg.Notifier
	block := "<campaign>\n" + strings.TrimSpace(brief) + "\n</campaign>\n\n" + stateBlock
	return withRequestState(ctx, st), block, nil
}

// end streams the completion of each committed tool result, announces the
// work to the campaign owner, and returns the turn's campaign result.
func end(ctx context.Context) (*assistant.PackResult, error) {
	st := getRequestState(ctx)
	outs := st.outcomes()
	if len(outs) == 0 {
		return nil, nil
	}
	resp := &CampaignAssistantResponse{Action: turnAction(outs, st.writes)}
	for _, o := range outs {
		if o.attach != nil {
			o.attach(resp)
		}
		if resp.Explanation == "" {
			resp.Explanation = o.explanation
		}
		assistant.Emit(ctx, string(o.event), o.payload)
	}
	touchOutcomes(ctx, st, resp)
	c := st.campaign
	publishAssistantFinalised(st.hub, c.ID, c.CreatedBy, resp)
	notifyContentPlanReady(st.notifier, c.TenantID, c.CreatedBy, c.ID, resp)
	return &assistant.PackResult{Value: resp, Fallback: resp.Explanation}, nil
}

// touchOutcomes records what the turn changed, for the client to refresh.
func touchOutcomes(ctx context.Context, st *requestState, resp *CampaignAssistantResponse) {
	if resp.ContentPlan != nil || resp.GeneratedPosts != nil {
		assistantkit.Touch(ctx, "idea", "")
	}
	if resp.DraftedPosts != nil || resp.Redistribute != nil {
		assistantkit.Touch(ctx, "post", "")
	}
	if resp.Brief != nil || resp.Dates != nil || resp.Platforms != nil {
		assistantkit.Touch(ctx, "campaign", st.campaignID)
	}
}

// publishAssistantFinalised announces committed campaign work on the shared
// event hub, so the campaign's other open tabs refresh. Topic is
// "entity:campaign:<id>", type "assistant.completed".
func publishAssistantFinalised(hub eventhub.Hub, campaignID, ownerID string, resp *CampaignAssistantResponse) {
	if hub == nil {
		return
	}
	id, err := models.NewID()
	if err != nil {
		slog.Error("cannot mint event id", logging.AttrComponent, logComponent, logging.AttrError, err)
		return
	}
	ev := eventhub.Event{
		ID:      id,
		Topic:   "entity:campaign:" + campaignID,
		UserID:  ownerID,
		Type:    "assistant.completed",
		Payload: map[string]any{"campaignId": campaignID, "action": resp.Action},
	}
	if err := hub.Publish(context.Background(), ev); err != nil {
		slog.Error("hub publish failed", logging.AttrComponent, logComponent, "campaign_id", campaignID, logging.AttrError, err)
	}
}

// notifyContentPlanReady drops a persistent "content plan ready"
// notification to the campaign owner when the turn planned the campaign, so
// an ordinary chat turn never reaches the inbox. It uses a fresh
// tenant-scoped context, bounded: the request's may be done.
func notifyContentPlanReady(n *notify.Service, tenantID, ownerID, campaignID string, resp *CampaignAssistantResponse) {
	if n == nil || ownerID == "" || tenantID == "" || resp.ContentPlan == nil {
		return
	}
	ctx, cancel := context.WithTimeout(tenantctx.With(context.Background(), tenantID), 5*time.Second)
	defer cancel()
	ideaCount := resp.ContentPlan.IdeaCount
	_ = n.Emit(ctx, ownerID, notify.Spec{
		Level:      models.NotificationLevelSuccess,
		Type:       "campaign.content_plan_ready",
		Title:      "Content plan ready",
		Body:       fmt.Sprintf("Your content plan is ready — %d idea(s) to pick from.", ideaCount),
		EntityType: "campaign",
		EntityID:   campaignID,
		ActionURL:  "/campaigns/" + campaignID + "/ideas",
		Data:       map[string]any{"idea_count": ideaCount},
	})
}
