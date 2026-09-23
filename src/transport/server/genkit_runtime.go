package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/firebase/genkit/go/plugins/anthropic"
	"go.opentelemetry.io/otel/attribute"

	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/telemetry"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// ErrAnthropicUnavailable is returned by the runtime callbacks when no
// Anthropic key is currently configured. Handlers map this to 503 via
// IsAnthropicAvailable, so the SSE stream is never opened in the
// unavailable case.
var ErrAnthropicUnavailable = errors.New("anthropic api key not configured")

// genkitRuntime owns a *genkit.Genkit dedicated to the Anthropic flows.
// The plugin captures its API key at genkit.Init time, so changing the
// key requires re-initialising the whole instance — Rebuild does that
// under a write lock and atomically swaps the cached callbacks.
//
// The embedding genkit instance lives separately in server.New: it is
// built once at boot, never rebuilt, and shared with the runtime via
// the embedder dependency. Splitting the two instances keeps the
// Anthropic rebuild from disturbing the embedder bound to the
// embedding instance.
type genkitRuntime struct {
	mu sync.RWMutex

	g      *genkit.Genkit
	plugin *anthropic.Anthropic

	contentPlanFn       func(ctx context.Context, campaignID string, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)
	postAssistantFn     func(ctx context.Context, req post_assistant.PostAssistantRequest, onEvent post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error)
	postQualityFn       func(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error)
	enrichBriefFn       func(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error)
	campaignAssistantFn func(ctx context.Context, req campaign_assistant.CampaignAssistantRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error)
	generatePostsFn     func(ctx context.Context, req content_plan.GeneratePostsRequest, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)
	checkBriefFn        func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error)
	checkPostsFn        func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error)

	cfg                 *config.Config
	hub                 eventhub.Hub
	embedder            ai.Embedder
	contentPlanRepos    content_plan.ContentPlanRepos
	postAssistRepos     post_assistant.PostAssistantRepos
	postQualityRepos    post_quality.PostQualityRepos
	enrichBriefRepos    enrich_brief.EnrichBriefRepos
	campaignAssistRepos campaign_assistant.CampaignAssistantRepos
	draftPostRepos      draft_post.DraftPostRepos
	campaignOverviewSvc *overview.Service
	cloneSvc            *clone.Service
	restoreSvc          *restore.Service
	scheduleSvc         *schedule.Service
	noteSvc             *notes.Service
	recorder            *usage.Recorder
	checker             *usage.Checker
	notifier            *notify.Service
}

// genkitDeps groups the runtime's static inputs: things captured at
// boot and reused on every rebuild.
type genkitDeps struct {
	cfg                 *config.Config
	hub                 eventhub.Hub
	embedder            ai.Embedder
	contentPlanRepos    content_plan.ContentPlanRepos
	postAssistRepos     post_assistant.PostAssistantRepos
	postQualityRepos    post_quality.PostQualityRepos
	enrichBriefRepos    enrich_brief.EnrichBriefRepos
	campaignAssistRepos campaign_assistant.CampaignAssistantRepos
	draftPostRepos      draft_post.DraftPostRepos
	campaignOverviewSvc *overview.Service
	cloneSvc            *clone.Service
	restoreSvc          *restore.Service
	scheduleSvc         *schedule.Service
	noteSvc             *notes.Service
	recorder            *usage.Recorder
	checker             *usage.Checker
	notifier            *notify.Service
}

// newGenkitRuntime builds the runtime and runs the initial rebuild.
// The boot path is allowed to start without an Anthropic key — the
// runtime's callbacks then return ErrAnthropicUnavailable until a key
// is added via the secrets API. Any error from the rebuild path
// (e.g. flow registration on a valid key) is returned so the caller
// can decide whether to fail boot.
func newGenkitRuntime(ctx context.Context, deps genkitDeps, store secrets.Store) (*genkitRuntime, error) {
	r := &genkitRuntime{
		cfg:                 deps.cfg,
		hub:                 deps.hub,
		embedder:            deps.embedder,
		contentPlanRepos:    deps.contentPlanRepos,
		postAssistRepos:     deps.postAssistRepos,
		postQualityRepos:    deps.postQualityRepos,
		enrichBriefRepos:    deps.enrichBriefRepos,
		campaignAssistRepos: deps.campaignAssistRepos,
		draftPostRepos:      deps.draftPostRepos,
		campaignOverviewSvc: deps.campaignOverviewSvc,
		cloneSvc:            deps.cloneSvc,
		restoreSvc:          deps.restoreSvc,
		scheduleSvc:         deps.scheduleSvc,
		noteSvc:             deps.noteSvc,
		recorder:            deps.recorder,
		checker:             deps.checker,
		notifier:            deps.notifier,
	}

	// CON-112 fix: stabilize the outgoing tool order so Anthropic's strict-tool
	// grammar-compilation cache stays warm across requests. Must be installed
	// before the plugin builds its client (from http.DefaultTransport), and
	// before the debug-logging wrap below so logging measures the stabilized
	// request. On by default; opt out via ANTHROPIC_STABLE_TOOL_ORDER=false.
	//
	// The planning model (campaign_assistant's tool-using routing loop) also
	// gets an ephemeral cache_control breakpoint on its system prefix so a
	// multi-turn conversation reuses the tools+system tokens across turns; other
	// flows are left uncached to avoid paying the cache-write premium for
	// nothing. See InstallAnthropicToolOrderStabilizer.
	if r.cfg == nil || r.cfg.AnthropicStableToolOrder {
		planningModel := ""
		if r.cfg != nil {
			planningModel = r.cfg.PlanningModelID
		}
		InstallAnthropicToolOrderStabilizer(planningModel)
	}

	// Opt-in Anthropic HTTP tracing (CON-112 perf diagnostics) — must be
	// installed before the plugin builds its client so the wrapped transport is
	// in place.
	if r.cfg != nil && r.cfg.AnthropicDebugHTTP {
		InstallAnthropicHTTPLogging()
	}

	if err := r.rebuild(ctx, store); err != nil {
		return nil, err
	}

	store.Subscribe(secrets.NameAnthropicAPIKey, func() {
		if err := r.rebuild(context.Background(), store); err != nil {
			slog.Error("rebuild after anthropic_api_key change failed",
				logging.AttrComponent, "genkit",
				logging.AttrError, err)
			// CON-303: a failed rebuild silently disables all AI flows until the
			// next key change — surface it to Sentry, not just the logs.
			telemetry.CaptureError(context.Background(), err,
				attribute.String("component", "genkit"),
				attribute.String("cause", "anthropic_key_rebuild"))
		}
	})
	return r, nil
}

// IsAnthropicAvailable reports whether the runtime currently has live
// Anthropic-backed flows. Handlers consult this before opening SSE
// streams so a missing key produces a clean 503.
func (r *genkitRuntime) IsAnthropicAvailable() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.contentPlanFn != nil && r.postAssistantFn != nil && r.postQualityFn != nil && r.enrichBriefFn != nil && r.campaignAssistantFn != nil && r.checkBriefFn != nil && r.checkPostsFn != nil
}

func (r *genkitRuntime) GenerateDraft(ctx context.Context, campaignID string, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
	r.mu.RLock()
	fn := r.contentPlanFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, campaignID, onEvent)
}

func (r *genkitRuntime) RunPostAssistant(ctx context.Context, req post_assistant.PostAssistantRequest, onEvent post_assistant.OnEventFunc) (*post_assistant.PostAssistantResponse, error) {
	r.mu.RLock()
	fn := r.postAssistantFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req, onEvent)
}

func (r *genkitRuntime) AssessPostQuality(ctx context.Context, postID string, onEvent post_quality.OnEventFunc) (*post_quality.PostQualityResponse, error) {
	r.mu.RLock()
	fn := r.postQualityFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, postID, onEvent)
}

func (r *genkitRuntime) EnrichBrief(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error) {
	r.mu.RLock()
	fn := r.enrichBriefFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req, onEvent)
}

func (r *genkitRuntime) RunCampaignAssistant(ctx context.Context, req campaign_assistant.CampaignAssistantRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
	r.mu.RLock()
	fn := r.campaignAssistantFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req, onEvent)
}

func (r *genkitRuntime) GeneratePosts(ctx context.Context, req content_plan.GeneratePostsRequest, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error) {
	r.mu.RLock()
	fn := r.generatePostsFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req, onEvent)
}

func (r *genkitRuntime) CheckBrief(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error) {
	r.mu.RLock()
	fn := r.checkBriefFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, campaignID, onEvent)
}

func (r *genkitRuntime) CheckPosts(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error) {
	r.mu.RLock()
	fn := r.checkPostsFn
	r.mu.RUnlock()
	if fn == nil {
		return nil, ErrAnthropicUnavailable
	}
	return fn(ctx, req, onEvent)
}

// rebuild fetches the current Anthropic key (if any), constructs a
// fresh Genkit instance with a fresh Anthropic plugin, re-registers
// the two flows, and atomically swaps the cached callbacks. Holding
// the write lock for the duration ensures handlers don't observe a
// half-rebuilt state.
//
// When no key is set, the runtime drops to the unavailable state:
// callbacks become nil, IsAnthropicAvailable returns false, and the
// previously-built genkit instance is left for the GC. Existing
// in-flight requests still hold a reference to the previous closures
// and continue running on the previous instance until they return.
func (r *genkitRuntime) rebuild(ctx context.Context, store secrets.Store) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	key, err := store.Get(ctx, secrets.NameAnthropicAPIKey)
	if errors.Is(err, secrets.ErrNotFound) {
		slog.WarnContext(ctx, "anthropic_api_key not configured; flows disabled",
			logging.AttrComponent, "genkit")
		r.contentPlanFn = nil
		r.postAssistantFn = nil
		r.postQualityFn = nil
		r.enrichBriefFn = nil
		r.campaignAssistantFn = nil
		r.generatePostsFn = nil
		r.checkBriefFn = nil
		r.checkPostsFn = nil
		r.g = nil
		r.plugin = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("genkit: read anthropic_api_key: %w", err)
	}

	plugin := &anthropic.Anthropic{APIKey: key}
	g := genkit.Init(ctx, genkit.WithPlugins(plugin))

	// The Provider now only builds the Anthropic call config (max tokens) the
	// flows pass; model SELECTION moved to the DB-backed modelconfig resolver
	// (CON-308), which each flow consults per (flow, slot). The model-id args are
	// vestigial and ignored by CallConfig — kept until provider.go is trimmed.
	provider := llm.NewProvider(r.cfg.ModelID, r.cfg.QualityModelID, r.cfg.PlanningModelID)

	contentPlanFn, err := initContentPlan(g, r.cfg, provider, r.recorder, r.checker, r.embedder, r.hub, r.notifier, r.contentPlanRepos)
	if err != nil {
		return fmt.Errorf("init content plan: %w", err)
	}
	postAssistantFn, err := initPostAssistant(g, r.cfg, provider, r.recorder, r.checker, r.embedder, r.hub, r.notifier, r.postAssistRepos, r.cloneSvc, r.restoreSvc, r.scheduleSvc, r.noteSvc)
	if err != nil {
		return fmt.Errorf("init post assistant: %w", err)
	}
	postQualityFn, err := initPostQuality(g, r.cfg, provider, r.recorder, r.checker, r.hub, r.notifier, r.postQualityRepos)
	if err != nil {
		return fmt.Errorf("init post quality: %w", err)
	}
	enrichBriefFn, err := initEnrichBrief(g, r.cfg, provider, r.recorder, r.checker, r.enrichBriefRepos)
	if err != nil {
		return fmt.Errorf("init enrich brief: %w", err)
	}
	// CON-114: the targeted generation callback shares the content_plan flow
	// (already registered by initContentPlan above).
	generatePostsFn := content_plan.NewGeneratePostsCallback()
	// CON-207: the draftPost generation flow (asset research → content-first drafts).
	draftPostFn, err := initDraftPost(g, r.cfg, provider, r.recorder, r.checker, r.draftPostRepos)
	if err != nil {
		return fmt.Errorf("init draft post: %w", err)
	}
	// CON-116: read-only brief + posts consistency review.
	checkBriefFn, checkPostsFn, err := initConsistency(g, r.cfg, provider, r.recorder, r.checker, r.hub, r.campaignAssistRepos.Campaigns, r.campaignAssistRepos.Posts)
	if err != nil {
		return fmt.Errorf("init consistency: %w", err)
	}
	// CON-112: the campaign assistant reuses the content_plan + enrich_brief
	// callbacks (plus the CON-114 generatePosts, CON-207 draftPost, and CON-116
	// consistency callbacks) as tools, so it is initialised after them.
	campaignAssistantFn, err := initCampaignAssistant(g, r.cfg, provider, r.recorder, r.checker, r.embedder, r.hub, r.notifier, r.campaignAssistRepos, contentPlanFn, enrichBriefFn, r.campaignOverviewSvc, generatePostsFn, draftPostFn, checkBriefFn, checkPostsFn)
	if err != nil {
		return fmt.Errorf("init campaign assistant: %w", err)
	}

	r.g = g
	r.plugin = plugin
	r.contentPlanFn = contentPlanFn
	r.postAssistantFn = postAssistantFn
	r.postQualityFn = postQualityFn
	r.enrichBriefFn = enrichBriefFn
	r.campaignAssistantFn = campaignAssistantFn
	r.generatePostsFn = generatePostsFn
	r.checkBriefFn = checkBriefFn
	r.checkPostsFn = checkPostsFn
	slog.InfoContext(ctx, "anthropic flows rebuilt",
		logging.AttrComponent, "genkit")
	return nil
}
