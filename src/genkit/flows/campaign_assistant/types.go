package campaign_assistant

import (
	"context"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// CampaignAssistantRequest is the input to the campaignAssistant flow — one
// conversational turn scoped to a single campaign.
type CampaignAssistantRequest struct {
	CampaignID  string `json:"campaignId"`
	Instruction string `json:"instruction"`
}

// CampaignAssistantResponse is the structured output for one turn. Explanation
// is ordered first so it streams to the client before the metadata. ContentPlan
// and Brief are populated by the server (not the model) when the matching tool
// ran this turn.
type CampaignAssistantResponse struct {
	Explanation string `json:"explanation"          jsonschema:"description=Conversational reply to the user"`
	Action      string `json:"action"               jsonschema:"description=answered for a grounded reply; content_plan_generated when runContentPlan ran; brief_enriched when enrichBrief ran; posts_generated when generatePosts ran; post_drafted when draftPost ran; dates_updated when setCampaignDates ran; posts_redistributed when redistributePosts ran; brief_reviewed when checkBrief ran; posts_reviewed when checkPostsConsistency ran; declined when the request is out of scope,enum=answered,enum=content_plan_generated,enum=brief_enriched,enum=posts_generated,enum=post_drafted,enum=dates_updated,enum=posts_redistributed,enum=brief_reviewed,enum=posts_reviewed,enum=declined"`
	// ContentPlan is set by the server when the runContentPlan tool created
	// draft posts this turn. Action is then "content_plan_generated".
	ContentPlan *ContentPlanResult `json:"contentPlan,omitempty" jsonschema:"-"`
	// Brief is set by the server when the enrichBrief tool applied a new brief
	// to the campaign this turn. Action is then "brief_enriched".
	Brief *BriefResult `json:"brief,omitempty" jsonschema:"-"`
	// GeneratedPosts is set by the server when the generatePosts tool added
	// targeted posts this turn. Action is then "posts_generated".
	GeneratedPosts *GeneratedPostsResult `json:"generatedPosts,omitempty" jsonschema:"-"`
	// DraftedPosts is set by the server when the draftPost tool created
	// content-first drafts from chat research this turn. Action is then
	// "post_drafted".
	DraftedPosts *DraftPostResult `json:"draftedPosts,omitempty" jsonschema:"-"`
	// Dates is set by the server when setCampaignDates changed the campaign's
	// dates this turn. Action is then "dates_updated".
	Dates *DatesResult `json:"dates,omitempty" jsonschema:"-"`
	// Redistribute is set by the server when redistributePosts moved posts this
	// turn. Action is then "posts_redistributed".
	Redistribute *RedistributeResult `json:"redistribute,omitempty" jsonschema:"-"`
	// BriefReview is set by the server when the checkBrief tool ran this turn
	// (read-only). Action is then "brief_reviewed".
	BriefReview *consistency.BriefReview `json:"briefReview,omitempty" jsonschema:"-"`
	// PostsReview is set by the server when the checkPostsConsistency tool ran
	// this turn (read-only). Action is then "posts_reviewed".
	PostsReview *consistency.PostsReview `json:"postsReview,omitempty" jsonschema:"-"`
}

// DatesResult summarises a setCampaignDates tool invocation.
type DatesResult struct {
	StartDate         string `json:"startDate"`
	EndDate           string `json:"endDate"`
	PostsOutsideRange int    `json:"postsOutsideRange"` // eligible posts now dated outside the new range
}

// RedistributeResult summarises a redistributePosts tool invocation.
type RedistributeResult struct {
	PostsUpdated int `json:"postsUpdated"`
	PhaseCount   int `json:"phaseCount"`
}

// GeneratedPostsResult summarises a generatePosts tool invocation.
type GeneratedPostsResult struct {
	PostCount   int      `json:"postCount"`
	PlatformIDs []string `json:"platformIds"`
	PhaseID     string   `json:"phaseId"`
	Warnings    []string `json:"warnings,omitempty"`
	// UsedAssets lists the campaign assets that informed the posts;
	// empty when none were used.
	UsedAssets []AssetRef `json:"usedAssets,omitempty"`
}

// DraftPostResult summarises a draftPost tool invocation.
type DraftPostResult struct {
	PostCount   int      `json:"postCount"`
	PlatformIDs []string `json:"platformIds"`
	PhaseID     string   `json:"phaseId"`
	Dates       []string `json:"dates,omitempty"` // actual publish dates of the created posts
	Warnings    []string `json:"warnings,omitempty"`
	// UsedAssets lists the campaign assets that informed the drafts;
	// empty in v1 (provenance is carried by each post's Source research note).
	UsedAssets []AssetRef `json:"usedAssets,omitempty"`
}

// ContentPlanResult summarises a runContentPlan tool invocation.
type ContentPlanResult struct {
	PostCount int      `json:"postCount"`
	Warnings  []string `json:"warnings,omitempty"`
	// UsedAssets lists the campaign assets that informed the plan.
	UsedAssets []AssetRef `json:"usedAssets,omitempty"`
}

// AssetRef is the id+title of a campaign asset that informed generation.
type AssetRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// BriefResult reports whether the enrichBrief tool applied a new brief.
type BriefResult struct {
	Applied bool `json:"applied"`
}

// CampaignAssistantRepos bundles the repository dependencies for the flow.
type CampaignAssistantRepos struct {
	Messages  repository.CampaignAssistantMessageRepository
	Campaigns repository.CampaignRepository
	// Posts backs the listCampaignPosts read tool used for grounded Q&A.
	Posts repository.PostRepository
	// Assets + Chunks back the askCampaignAssets read tool: resolve the
	// campaign's ready attached assets and search their embedded chunks.
	Assets repository.AssetRepository
	Chunks repository.AssetChunksRepository
	// Brands resolves the campaign's brand voice/audience/guardrails into the
	// context block. nil falls back to the legacy tone prose.
	Brands repository.BrandRepository
}

// CampaignAssistantFlowConfig holds static settings for the flow.
type CampaignAssistantFlowConfig struct {
	// Provider resolves the model reference + call config by role. The
	// orchestration/routing loop runs on RolePlanning (cheap/fast); the
	// content_plan / enrich_brief sub-flows keep their own generation model.
	Provider *llm.Provider
	// Recorder captures the planner's usage event; nil disables recording.
	Recorder *usage.Recorder
	// Checker gates the flow against the tenant's spend caps; nil = no gate.
	Checker *usage.Checker
	// Embedder embeds the askCampaignAssets query for chunk search.
	// A nil / unavailable embedder disables asset Q&A gracefully.
	Embedder ai.Embedder
	ModelID  string
	// MaxOutputTokens caps a single planner call. 0 falls back to 8192 — the
	// planner only emits a short JSON envelope, never long prose.
	MaxOutputTokens int64
	// MaxTurns caps tool-use round-trips. 0 falls back to 4 (routing rarely
	// needs more than one tool call plus the final answer).
	MaxTurns int
	// Hub publishes "operation finalised" events on success/failure. nil = silent.
	Hub eventhub.Hub
	// Notifier drops a persistent "content plan ready" notification to the
	// campaign owner when a run generates a content plan — only that action, not
	// every assistant turn. nil = silent.
	Notifier *notify.Service

	// PrewarmTools, when true, fires one throwaway generation carrying the full
	// tool set at init so Anthropic compiles + caches the strict-tool grammar
	// off the user path. Only worth it alongside a stable tool order,
	// so the server sets this from cfg.AnthropicStableToolOrder.
	PrewarmTools bool

	// ContentPlan and EnrichBrief are the existing flow callbacks, invoked as
	// tools. Injected so the assistant reuses them verbatim — no duplicate LLM
	// generation. nil disables the corresponding tool.
	ContentPlan func(ctx context.Context, campaignID string, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)
	EnrichBrief func(ctx context.Context, req enrich_brief.EnrichBriefRequest, onEvent enrich_brief.OnEventFunc) (*enrich_brief.EnrichBriefResponse, error)
	// Overview backs the getCampaignOverview read tool. nil disables
	// the tool.
	Overview *overview.Service
	// GeneratePosts backs the generatePosts targeted-generation tool.
	// nil disables the tool.
	GeneratePosts func(ctx context.Context, req content_plan.GeneratePostsRequest, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)
	// MaxGeneratePosts caps how many posts one generatePosts call may create.
	// 0 falls back to 10.
	MaxGeneratePosts int
	// DraftPost backs the draftPost tool: rewrite chat research into
	// extended content-first drafts. nil disables the tool.
	DraftPost func(ctx context.Context, req draft_post.DraftPostRequest, onEvent draft_post.OnEventFunc) (*draft_post.DraftPostResponse, error)
	// MaxDraftPosts caps how many posts one draftPost call may create.
	// 0 falls back to 5.
	MaxDraftPosts int
	// CheckBrief / CheckPosts back the read-only consistency review tools.
	// nil disables the corresponding tool.
	CheckBrief func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error)
	CheckPosts func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error)
}

// ValidationError is returned when preconditions are not met (HTTP 400).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// AIError is returned when the model call fails or returns nothing usable
// (HTTP 502).
type AIError struct{ Msg string }

func (e *AIError) Error() string { return e.Msg }

// ── SSE types ─────────────────────────────────────────────────────────────────

// SSEEventKind identifies the SSE event types emitted by the assistant flow.
// The content_plan_* and enrich_brief_* events are the nested sub-flow events
// re-emitted namespaced so the client sees live progress while a tool runs.
type SSEEventKind string

const (
	SSEEventExplanationDelta SSEEventKind = "explanation_delta"
	SSEEventToolCall         SSEEventKind = "tool_call"
	SSEEventToolResult       SSEEventKind = "tool_result"

	SSEEventContentPlanStarted  SSEEventKind = "content_plan_started"
	SSEEventContentPlanStep     SSEEventKind = "content_plan_step"
	SSEEventContentPlanPost     SSEEventKind = "content_plan_post"
	SSEEventContentPlanWarning  SSEEventKind = "content_plan_warning"
	SSEEventContentPlanComplete SSEEventKind = "content_plan_complete"

	SSEEventEnrichBriefStarted          SSEEventKind = "enrich_brief_started"
	SSEEventEnrichBriefDescriptionDelta SSEEventKind = "enrich_brief_description_delta"
	SSEEventEnrichBriefPersonaDelta     SSEEventKind = "enrich_brief_persona_delta"
	SSEEventEnrichBriefMessagesDelta    SSEEventKind = "enrich_brief_messages_delta"
	SSEEventEnrichBriefToneDelta        SSEEventKind = "enrich_brief_tone_delta"
	SSEEventEnrichBriefComplete         SSEEventKind = "enrich_brief_complete"

	SSEEventGeneratePostsStarted  SSEEventKind = "generate_posts_started"
	SSEEventGeneratePostsStep     SSEEventKind = "generate_posts_step"
	SSEEventGeneratePostsPost     SSEEventKind = "generate_posts_post"
	SSEEventGeneratePostsWarning  SSEEventKind = "generate_posts_warning"
	SSEEventGeneratePostsComplete SSEEventKind = "generate_posts_complete"

	SSEEventDraftPostStarted  SSEEventKind = "draft_post_started"
	SSEEventDraftPostStep     SSEEventKind = "draft_post_step"
	SSEEventDraftPostPost     SSEEventKind = "draft_post_post"
	SSEEventDraftPostWarning  SSEEventKind = "draft_post_warning"
	SSEEventDraftPostComplete SSEEventKind = "draft_post_complete"

	// SSEEventAssetsUsed reports which attached assets informed the generated
	// posts; emitted by runContentPlan/generatePosts when non-empty.
	SSEEventAssetsUsed SSEEventKind = "assets_used"

	SSEEventDatesUpdated       SSEEventKind = "dates_updated"
	SSEEventPostsRedistributed SSEEventKind = "posts_redistributed"

	SSEEventCheckBriefStarted  SSEEventKind = "check_brief_started"
	SSEEventCheckBriefComplete SSEEventKind = "check_brief_complete"
	SSEEventCheckPostsStarted  SSEEventKind = "check_posts_started"
	SSEEventCheckPostsComplete SSEEventKind = "check_posts_complete"

	SSEEventComplete SSEEventKind = "complete"
	SSEEventError    SSEEventKind = "error"
)

// DeltaEventPayload carries a fragment of a streamed string value.
type DeltaEventPayload struct {
	Delta string `json:"delta"`
}

// ToolCallEventPayload is emitted once per fully-formed tool request.
type ToolCallEventPayload struct {
	Name  string `json:"name"`
	Input any    `json:"input,omitempty"`
	Ref   string `json:"ref,omitempty"`
}

// ToolResultEventPayload is emitted once a tool invocation returns.
type ToolResultEventPayload struct {
	Name string `json:"name"`
	Ref  string `json:"ref,omitempty"`
	OK   bool   `json:"ok"`
}

// ContentPlanStartedEventPayload is emitted when the runContentPlan tool begins.
type ContentPlanStartedEventPayload struct{}

// ContentPlanCompleteEventPayload is emitted once the content plan is persisted.
type ContentPlanCompleteEventPayload struct {
	PostCount int      `json:"postCount"`
	Warnings  []string `json:"warnings,omitempty"`
}

// EnrichBriefStartedEventPayload is emitted when the enrichBrief tool begins.
type EnrichBriefStartedEventPayload struct {
	Instruction string `json:"instruction,omitempty"`
}

// EnrichBriefCompleteEventPayload is emitted once the brief is applied.
type EnrichBriefCompleteEventPayload struct {
	Applied bool `json:"applied"`
}

// GeneratePostsStartedEventPayload is emitted when the generatePosts tool begins.
type GeneratePostsStartedEventPayload struct {
	PlatformIDs []string `json:"platformIds"`
	PhaseID     string   `json:"phaseId"`
	Count       int      `json:"count"`
}

// GeneratePostsCompleteEventPayload is emitted once targeted posts are persisted.
type GeneratePostsCompleteEventPayload struct {
	PostCount int      `json:"postCount"`
	Warnings  []string `json:"warnings,omitempty"`
}

// DraftPostStartedEventPayload is emitted when the draftPost tool begins.
type DraftPostStartedEventPayload struct {
	PlatformIDs []string `json:"platformIds"`
	Count       int      `json:"count"`
}

// DraftPostCompleteEventPayload is emitted once content-first drafts are persisted.
type DraftPostCompleteEventPayload struct {
	PostCount int      `json:"postCount"`
	Warnings  []string `json:"warnings,omitempty"`
}

// AssetsUsedEventPayload lists the attached assets that informed a generation.
type AssetsUsedEventPayload struct {
	Assets []AssetRef `json:"assets"`
}

// DatesUpdatedEventPayload is emitted once the campaign's dates are saved.
type DatesUpdatedEventPayload struct {
	StartDate         string `json:"startDate"`
	EndDate           string `json:"endDate"`
	PostsOutsideRange int    `json:"postsOutsideRange"`
}

// PostsRedistributedEventPayload is emitted once posts are re-dated.
type PostsRedistributedEventPayload struct {
	PostsUpdated int `json:"postsUpdated"`
}

// CheckBriefCompleteEventPayload is emitted once a brief review completes.
type CheckBriefCompleteEventPayload struct {
	Consistent   bool `json:"consistent"`
	FindingCount int  `json:"findingCount"`
}

// CheckPostsCompleteEventPayload is emitted once a posts review completes.
type CheckPostsCompleteEventPayload struct {
	Checked    int  `json:"checked"`
	Total      int  `json:"total"`
	Capped     bool `json:"capped"`
	DriftCount int  `json:"driftCount"`
}

// ErrorEventPayload is emitted when the flow fails mid-stream.
type ErrorEventPayload struct {
	Message string `json:"message"`
	Code    int    `json:"code"` // HTTP semantic: 400, 402, 502, 500
}

// OnEventFunc is an optional callback invoked as SSE events are produced.
// A nil OnEventFunc is valid — the flow runs silently.
type OnEventFunc func(name SSEEventKind, data any)
