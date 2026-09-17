package content_plan

import (
	"context"
	"embed"
	"fmt"
	"log/slog"
	"text/template"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/core"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

//go:embed prompts/content_plan.tmpl
var promptFS embed.FS

// ContentPlanFlow is the singleton flow for generating a content plan.
// Set by InitContentPlan; nil until then.
var ContentPlanFlow *core.Flow[ContentPlanRequest, *ContentPlanResponse, struct{}]

// contentPlanRunner is a direct closure over (g, cfg, repos) that bypasses
// the Genkit flow wrapper, allowing an OnEventFunc to be threaded through.
// Set by InitContentPlan alongside ContentPlanFlow.
var contentPlanRunner func(ctx context.Context, req ContentPlanRequest, onEvent OnEventFunc) (*ContentPlanResponse, error)

// generatePostsRunner backs the CON-114 targeted generation callback. Set by
// InitContentPlan.
var generatePostsRunner func(ctx context.Context, req GeneratePostsRequest, onEvent OnEventFunc) (*ContentPlanResponse, error)

// ContentPlanFlowConfig holds the settings for the content plan flow.
type ContentPlanFlowConfig struct {
	// Provider resolves the model reference + call config by role, so the
	// flow doesn't hardcode the "anthropic/" prefix or the Anthropic SDK
	// config type (CON-86 FR12).
	Provider *llm.Provider
	// Recorder captures one usage event per model call; nil disables recording
	// (CON-86 FR5/FR10).
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps before the model
	// call; nil = no enforcement (CON-86 FR9).
	Checker          *usage.Checker
	ModelID          string
	MaxContextAssets int   // max assets when no embedder (creation-order fallback)
	MaxContextChars  int   // character budget for asset context in the prompt
	MaxOutputTokens  int64 // max_tokens sent to the model; 0 falls back to 8192
	// MaxPostsPerBatch caps the number of posts the model is asked to
	// produce in a single batched call. Sized so 800 tokens/post stays
	// comfortably under MaxOutputTokens with headroom for slower
	// per-post tail latencies. 0 falls back to 30.
	MaxPostsPerBatch int
	// MaxParallelBatches caps how many batches run concurrently — a
	// safety knob against Anthropic per-account rate limits when very
	// large campaigns produce many batches. 0 falls back to 5.
	MaxParallelBatches int
	Embedder           ai.Embedder // nil = skip semantic ranking, fall back to creation order
	// Hub is the event broker used to publish "operation finalised"
	// events on success/failure. nil = silent (no events emitted).
	Hub eventhub.Hub
	// Notifier drops a durable content_plan.failed notification to the campaign
	// owner (CON-285) — the failure twin of campaign.content_plan_ready (which the
	// campaign assistant emits on success). nil is a no-op.
	Notifier   *notify.Service
	systemTmpl *template.Template
	userTmpl   *template.Template
}

// ContentPlanRepos bundles all repository dependencies for the flow.
type ContentPlanRepos struct {
	Campaigns repository.CampaignRepository
	Assets    repository.AssetRepository
	Chunks    repository.AssetChunksRepository
	Platforms repository.PlatformRepository
	Posts     repository.PostRepository
	// Notes captures each generated post's bullet-point thesis as a
	// draft_thesis note instead of the post body (CON-188). nil skips note
	// creation (the post is still created with an empty body).
	Notes repository.PostNoteRepository
	// Brands resolves the campaign's brand voice/audience/guardrails into the
	// prompt (CON-245). nil falls back to the legacy tone_guidelines prose.
	Brands repository.BrandRepository
}

// InitContentPlan registers the generateContentPlan Genkit flow. It must be
// called after the Genkit instance has been initialised with the Anthropic
// plugin.
func InitContentPlan(g *genkit.Genkit, cfg ContentPlanFlowConfig, repos ContentPlanRepos) error {
	raw, err := promptFS.ReadFile("prompts/content_plan.tmpl")
	if err != nil {
		return fmt.Errorf("load content_plan.tmpl: %w", err)
	}
	tmpl, err := template.New("content_plan").Parse(string(raw))
	if err != nil {
		return fmt.Errorf("parse content_plan.tmpl: %w", err)
	}
	cfg.systemTmpl = tmpl.Lookup("system")
	cfg.userTmpl = tmpl.Lookup("user")
	if cfg.systemTmpl == nil || cfg.userTmpl == nil {
		return fmt.Errorf("content_plan.tmpl must define both {{define \"system\"}} and {{define \"user\"}} blocks")
	}

	ContentPlanFlow = genkit.DefineFlow(g, "generateContentPlan",
		func(ctx context.Context, req ContentPlanRequest) (*ContentPlanResponse, error) {
			return runContentPlan(ctx, g, req, cfg, repos, nil)
		},
	)

	contentPlanRunner = func(ctx context.Context, req ContentPlanRequest, onEvent OnEventFunc) (*ContentPlanResponse, error) {
		return runContentPlan(ctx, g, req, cfg, repos, onEvent)
	}

	generatePostsRunner = func(ctx context.Context, req GeneratePostsRequest, onEvent OnEventFunc) (*ContentPlanResponse, error) {
		return runGeneratePosts(ctx, g, req, cfg, repos, onEvent)
	}

	return nil
}

// NewContentPlanCallback returns a callback suitable for passing to the
// campaigns handler. onEvent is forwarded to the flow for SSE streaming;
// pass nil for a silent, non-streaming call.
func NewContentPlanCallback() func(ctx context.Context, campaignID string, onEvent OnEventFunc) (*ContentPlanResponse, error) {
	return func(ctx context.Context, campaignID string, onEvent OnEventFunc) (*ContentPlanResponse, error) {
		return contentPlanRunner(ctx, ContentPlanRequest{CampaignID: campaignID}, onEvent)
	}
}

// NewGeneratePostsCallback returns the CON-114 targeted generation callback for
// the campaign assistant tool and the REST endpoint. onEvent is forwarded for
// SSE streaming; nil runs silently.
func NewGeneratePostsCallback() func(ctx context.Context, req GeneratePostsRequest, onEvent OnEventFunc) (*ContentPlanResponse, error) {
	return func(ctx context.Context, req GeneratePostsRequest, onEvent OnEventFunc) (*ContentPlanResponse, error) {
		return generatePostsRunner(ctx, req, onEvent)
	}
}

// emit calls onEvent when it is non-nil. It is a safe no-op otherwise.
func emit(onEvent OnEventFunc, name SSEEventKind, data any) {
	if onEvent != nil {
		onEvent(name, data)
	}
}

// publishContentPlanFinalised announces the end of a content-plan run on
// the shared event hub. Topic is "entity:campaign:<id>"; type is
// "content_plan_completed" on success, "content_plan_failed" on error.
func publishContentPlanFinalised(
	hub eventhub.Hub,
	campaignID, ownerID string,
	resp *ContentPlanResponse,
	err error,
) {
	if hub == nil {
		return
	}
	id, idErr := models.NewID()
	if idErr != nil {
		slog.Error("cannot mint event id", logging.AttrComponent, "genkit.content_plan", logging.AttrError, idErr)
		return
	}
	ev := eventhub.Event{
		ID:     id,
		Topic:  "entity:campaign:" + campaignID,
		UserID: ownerID,
	}
	if err != nil {
		ev.Type = "content_plan_failed"
		ev.Payload = map[string]any{
			"campaignId": campaignID,
			"error":      err.Error(),
		}
	} else {
		postCount := 0
		warningCount := 0
		if resp != nil {
			postCount = len(resp.Posts)
			warningCount = len(resp.Warnings)
		}
		ev.Type = "content_plan_completed"
		ev.Payload = map[string]any{
			"campaignId":   campaignID,
			"postCount":    postCount,
			"warningCount": warningCount,
		}
	}
	if pubErr := hub.Publish(context.Background(), ev); pubErr != nil {
		slog.Error("hub publish failed", logging.AttrComponent, "genkit.content_plan", "campaign_id", campaignID, logging.AttrError, pubErr)
	}
}

// notifyContentPlanFailed drops a durable content_plan.failed notification to the
// campaign owner (CON-285) when a plan generation fails — the failure twin of
// campaign.content_plan_ready, which the campaign assistant emits on success (so
// this fires only on error, never on the happy path). The dedupe_key collapses
// repeats for the same campaign while still unread. Uses a fresh tenant-scoped,
// bounded context because the request ctx may already be cancelled by the time
// this deferred call runs.
func notifyContentPlanFailed(n *notify.Service, tenantID, ownerID, campaignID string, runErr error) {
	if n == nil || runErr == nil || ownerID == "" || tenantID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(tenantctx.With(context.Background(), tenantID), 5*time.Second)
	defer cancel()
	_ = n.Emit(ctx, ownerID, notify.Spec{
		Level:      models.NotificationLevelError,
		Type:       "content_plan.failed",
		Title:      "Content plan failed",
		Body:       "We couldn't generate your content plan.",
		EntityType: "campaign",
		EntityID:   campaignID,
		ActionURL:  "/campaigns/" + campaignID,
		DedupeKey:  "content_plan.failed:" + campaignID,
	})
}

// runContentPlan executes the six steps of the flow.
func runContentPlan(
	ctx context.Context,
	g *genkit.Genkit,
	req ContentPlanRequest,
	cfg ContentPlanFlowConfig,
	repos ContentPlanRepos,
	onEvent OnEventFunc,
) (out *ContentPlanResponse, retErr error) {
	start := time.Now()
	slog.InfoContext(ctx, "starting", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID)

	// Enforcement gate (CON-86 FR9): block before any provider call when the
	// tenant is already over a cap in enforce mode. Nil checker = no gate.
	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}

	// finaliseOwnerID / finaliseTenantID are captured once the campaign is loaded
	// so the deferred finalisation event + durable notification can be scoped to
	// the campaign owner and tenant. Empty before validateInput → finalisation
	// for very-early failures is skipped.
	var finaliseOwnerID, finaliseTenantID string

	defer func() {
		if finaliseOwnerID == "" {
			return
		}
		publishContentPlanFinalised(cfg.Hub, req.CampaignID, finaliseOwnerID, out, retErr)
		// CON-285: the durable content_plan.failed twin (success is announced by
		// the campaign assistant's campaign.content_plan_ready).
		notifyContentPlanFailed(cfg.Notifier, finaliseTenantID, finaliseOwnerID, req.CampaignID, retErr)
	}()

	// ── Step 1: validateInput ─────────────────────────────────────────────────
	slog.InfoContext(ctx, "step 1/6 validateInput", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID)
	campaign, err := validateInput(ctx, req.CampaignID, repos.Campaigns)
	if err != nil {
		slog.ErrorContext(ctx, "validateInput failed", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, err
	}
	finaliseOwnerID = campaign.CreatedBy
	finaliseTenantID = campaign.TenantID
	slog.InfoContext(ctx, "validateInput done", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "campaign", campaign.Name, "platforms", len(campaign.TargetPlatforms))
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "validateInput", Status: "done"})

	// ── Step 2: resolveAssets ─────────────────────────────────────────────────
	slog.InfoContext(ctx, "step 2/6 resolveAssets", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "use_assets", campaign.UseAssets)
	assets, assetWarnings, err := resolveAssets(ctx, campaign, cfg, repos)
	if err != nil {
		slog.ErrorContext(ctx, "resolveAssets failed", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, err
	}
	slog.InfoContext(ctx, "resolveAssets done", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "assets", len(assets), "warnings", len(assetWarnings))
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "resolveAssets", Status: "done"})
	warnings := assetWarnings

	// ── Step 3: resolvePlatforms ──────────────────────────────────────────────
	slog.InfoContext(ctx, "step 3/6 resolvePlatforms", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID)
	platforms, err := resolvePlatforms(ctx, campaign.TargetPlatforms, repos.Platforms)
	if err != nil {
		slog.ErrorContext(ctx, "resolvePlatforms failed", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), logging.AttrError, err)
		return nil, err
	}
	if len(platforms) == 0 {
		return nil, &ValidationError{Msg: "none of the campaign's target platforms could be resolved — check that platform IDs are valid"}
	}
	if len(platforms) < len(campaign.TargetPlatforms) {
		slog.WarnContext(ctx, "resolvePlatforms resolved fewer platforms than targeted, some IDs may be stale", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "resolved", len(platforms), "targeted", len(campaign.TargetPlatforms))
	}
	slog.InfoContext(ctx, "resolvePlatforms done", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "platforms", len(platforms))
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "resolvePlatforms", Status: "done"})

	// ── Step 4: generatePosts (now: parse → validate → persist → emit) ───────
	// Per CON-66 each post is inserted into the database the moment it's
	// parsed and passes validation, so the response set is also the
	// authoritative persisted set — no separate persist step. The legacy
	// "validateOutput" / "persistDraftPosts" SSE step events still fire as
	// no-ops below for client compatibility; the substantive work all
	// happens inside generatePosts.
	slog.InfoContext(ctx, "step 4/6 generatePosts", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "model", cfg.ModelID, "estimated_count", campaign.EstimatedPostCount)
	posts, genWarnings, err := generatePosts(ctx, g, campaign, platforms, assets, cfg, repos, onEvent, nil)
	if err != nil {
		slog.ErrorContext(ctx, "generatePosts failed", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), "persisted", len(posts), logging.AttrError, err)
		return nil, err
	}
	slog.InfoContext(ctx, "generatePosts done", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "posts", len(posts), "warnings", len(genWarnings))
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "generatePosts", Status: "done"})
	warnings = append(warnings, genWarnings...)

	// ── Step 5: validateOutput (no-op — validation is inline) ─────────────────
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "validateOutput", Status: "done"})

	// ── Step 6: persistDraftPosts (no-op — persistence is inline) ─────────────
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "persistDraftPosts", Status: "done"})

	slog.InfoContext(ctx, "done", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), "posts", len(posts), "warnings", len(warnings))
	return &ContentPlanResponse{
		CampaignID:  campaign.ID,
		GeneratedAt: time.Now().UTC(),
		Posts:       posts,
		Warnings:    warnings,
		UsedAssets:  assetRefsOf(assets),
	}, nil
}

// runGeneratePosts is the CON-114 targeted generation orchestrator: generate
// exactly req.Count draft posts for the requested platform subset, in a single
// phase, with publish dates within [WindowStart, WindowEnd]. It reuses the
// content-plan validation, asset grounding, batching, validation, inline
// persistence, and streaming with a targeting override.
func runGeneratePosts(
	ctx context.Context,
	g *genkit.Genkit,
	req GeneratePostsRequest,
	cfg ContentPlanFlowConfig,
	repos ContentPlanRepos,
	onEvent OnEventFunc,
) (out *ContentPlanResponse, retErr error) {
	start := time.Now()
	slog.InfoContext(ctx, "starting targeted generation", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "platforms", len(req.PlatformIDs), "phase_id", req.PhaseID, "count", req.Count)

	if err := cfg.Checker.Enforce(ctx); err != nil {
		return nil, err
	}
	if req.Count <= 0 {
		return nil, &ValidationError{Msg: "count must be at least 1"}
	}

	// A fully-configured campaign is still required (brief, phases, dates,
	// platforms); the targeted window is separate.
	campaign, err := validateInput(ctx, req.CampaignID, repos.Campaigns)
	if err != nil {
		return nil, err
	}

	finaliseOwnerID := campaign.CreatedBy
	defer func() {
		if cfg.Hub == nil || finaliseOwnerID == "" {
			return
		}
		publishContentPlanFinalised(cfg.Hub, req.CampaignID, finaliseOwnerID, out, retErr)
	}()

	phase, ok := resolvePhaseByID(campaign, req.PhaseID)
	if !ok {
		return nil, &ValidationError{Msg: fmt.Sprintf("phase %q is not a phase of this campaign", req.PhaseID)}
	}

	winStart, err := time.Parse("2006-01-02", req.WindowStart)
	if err != nil {
		return nil, &ValidationError{Msg: "windowStart must be an ISO date (YYYY-MM-DD)"}
	}
	winEnd, err := time.Parse("2006-01-02", req.WindowEnd)
	if err != nil {
		return nil, &ValidationError{Msg: "windowEnd must be an ISO date (YYYY-MM-DD)"}
	}
	if winEnd.Before(winStart) {
		return nil, &ValidationError{Msg: "windowEnd must be on or after windowStart"}
	}

	assets, assetWarnings, err := resolveAssets(ctx, campaign, cfg, repos)
	if err != nil {
		return nil, err
	}

	allPlatforms, err := resolvePlatforms(ctx, campaign.TargetPlatforms, repos.Platforms)
	if err != nil {
		return nil, err
	}
	platforms, err := selectPlatforms(allPlatforms, req.PlatformIDs, req.PostType)
	if err != nil {
		return nil, err
	}
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "resolveTargets", Status: "done"})

	tgt := &targeting{
		count:       req.Count,
		phases:      []resolvedPhase{phase},
		windowStart: winStart,
		windowEnd:   winEnd,
	}
	posts, genWarnings, err := generatePosts(ctx, g, campaign, platforms, assets, cfg, repos, onEvent, tgt)
	if err != nil {
		return nil, err
	}
	warnings := append(assetWarnings, genWarnings...)
	emit(onEvent, SSEEventStep, StepEventPayload{Step: "generatePosts", Status: "done"})

	slog.InfoContext(ctx, "targeted generation done", logging.AttrComponent, "genkit.content_plan", "campaign_id", req.CampaignID, "duration_ms", time.Since(start).Milliseconds(), "posts", len(posts), "warnings", len(warnings))
	return &ContentPlanResponse{
		CampaignID:  campaign.ID,
		GeneratedAt: time.Now().UTC(),
		Posts:       posts,
		Warnings:    warnings,
		UsedAssets:  assetRefsOf(assets),
	}, nil
}
