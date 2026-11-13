package campaign_assistant

import (
	"context"

	"github.com/firebase/genkit/go/ai"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/consistency"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// CampaignAssistantResponse is what the campaign pack's tools did in a turn,
// under "campaign" in the assistant's results: the fallback reply, the action
// that labels the turn, and each committed tool result.
type CampaignAssistantResponse struct {
	Explanation string `json:"explanation"          jsonschema:"description=Conversational reply to the user"`
	Action      string `json:"action"               jsonschema:"description=answered for a grounded reply; content_plan_generated when runContentPlan ran; brief_enriched when enrichBrief ran; posts_generated when generatePosts ran; post_drafted when draftPost ran; dates_updated when setCampaignDates ran; platforms_updated when updateCampaignPlatforms ran; goal_updated when setCampaignGoal ran; posts_redistributed when redistributePosts ran; brief_reviewed when checkBrief ran; posts_reviewed when checkPostsConsistency ran; proposed when a requested change is waiting for the user's confirmation; declined when the request is out of scope,enum=answered,enum=proposed,enum=content_plan_generated,enum=brief_enriched,enum=posts_generated,enum=post_drafted,enum=dates_updated,enum=platforms_updated,enum=goal_updated,enum=posts_redistributed,enum=brief_reviewed,enum=posts_reviewed,enum=declined"`
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
	// Platforms is set by the server when updateCampaignPlatforms added
	// platforms or post types this turn. Action is then "platforms_updated".
	Platforms *PlatformsResult `json:"platforms,omitempty" jsonschema:"-"`
	// Goal is set by the server when setCampaignGoal changed the campaign's
	// post goal this turn. Action is then "goal_updated", unless drafting
	// after it labels the turn "post_drafted".
	Goal *GoalResult `json:"goal,omitempty" jsonschema:"-"`
	// Ask is set by the server when askCampaignAssets answered this turn:
	// the last answer's sources, for the client's card. It doesn't change
	// Action.
	Ask *AskResult `json:"ask,omitempty" jsonschema:"-"`
	// State is the campaign as the turn read it before acting, with a goal
	// the turn raised already counted: the figures the client cites under
	// the reply. Set on every campaign turn.
	State *state.Snapshot `json:"state,omitempty" jsonschema:"-"`
	// Starters lead with the state's numbers, for the client's chips.
	Starters []Starter `json:"starters,omitempty" jsonschema:"-"`
}

// What an askCampaignAssets answer searched: the asset open on screen, the
// assets asked for, or the campaign's.
const (
	askScopeOpenAsset = "open_asset"
	askScopeAsked     = "asked"
	askScopeCampaign  = "campaign"
)

// AskResult is an askCampaignAssets answer's sources: what was searched
// and the passages' assets and pages, without their text — the assets
// pack's results.assets.ask shape.
type AskResult struct {
	Scope    string    `json:"scope"`
	Excerpts []AskCite `json:"excerpts"`
}

// AskCite is one passage an answer drew on.
type AskCite struct {
	AssetID string `json:"assetId"`
	Title   string `json:"title"`
	Pages   string `json:"pages,omitempty"`
}

// DatesResult summarises a setCampaignDates tool invocation.
type DatesResult struct {
	StartDate         string `json:"startDate"`
	EndDate           string `json:"endDate"`
	PostsOutsideRange int    `json:"postsOutsideRange"` // eligible posts now dated outside the new range
	// UndoID reverses the change via POST /api/assistant/actions/:id/undo;
	// empty when the change could not be logged.
	UndoID string `json:"undoId,omitempty"`
}

// RedistributeResult summarises a redistributePosts tool invocation.
type RedistributeResult struct {
	PostsUpdated int `json:"postsUpdated"`
	PhaseCount   int `json:"phaseCount"`
}

// GeneratedPostsResult summarises a generatePosts tool invocation.
type GeneratedPostsResult struct {
	// IdeaCount is how many ideas were planned; the plan writes ideas, not
	// posts.
	IdeaCount   int      `json:"ideaCount"`
	PlatformIDs []string `json:"platformIds"`
	PhaseID     string   `json:"phaseId"`
	Warnings    []string `json:"warnings,omitempty"`
	// UsedAssets lists the campaign assets that informed the posts;
	// empty when none were used.
	UsedAssets []AssetRef `json:"usedAssets,omitempty"`
	// Grounding says whether the posts came from the campaign's sources.
	Grounding assistantkit.Grounding `json:"grounding"`
	// WindowStart and WindowEnd are the days the ideas target, YYYY-MM-DD.
	WindowStart string `json:"windowStart,omitempty"`
	WindowEnd   string `json:"windowEnd,omitempty"`
	// UndoID deletes the ideas while they are as made; empty when they
	// could not be logged.
	UndoID string `json:"undoId,omitempty"`
}

// DraftPostResult summarises a draftPost tool invocation.
type DraftPostResult struct {
	PostCount   int      `json:"postCount"`
	PlatformIDs []string `json:"platformIds"`
	PhaseID     string   `json:"phaseId"`
	Dates       []string `json:"dates,omitempty"` // actual publish dates of the created posts
	Warnings    []string `json:"warnings,omitempty"`
	// UsedAssets lists the assets the research the drafts came from quoted.
	UsedAssets []AssetRef `json:"usedAssets,omitempty"`
	// Grounding is those assets; unset when the research's sources aren't
	// known.
	Grounding *assistantkit.Grounding `json:"grounding,omitempty"`
	// Posts are the drafts, one row each, in the order they were written.
	Posts []DraftedRow `json:"posts,omitempty"`
}

// DraftedRow is one drafted post as its card lists it. Date is its publish
// day (YYYY-MM-DD), empty for a draft left off the calendar.
type DraftedRow struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	PlatformID string `json:"platformId"`
	Platform   string `json:"platform"`
	PostType   string `json:"postType"`
	Date       string `json:"date,omitempty"`
}

// ContentPlanResult summarises a runContentPlan tool invocation.
type ContentPlanResult struct {
	// IdeaCount is how many ideas the plan proposed.
	IdeaCount int      `json:"ideaCount"`
	Warnings  []string `json:"warnings,omitempty"`
	// UsedAssets lists the campaign assets that informed the plan.
	UsedAssets []AssetRef `json:"usedAssets,omitempty"`
	// Grounding says whether the posts came from the campaign's sources.
	Grounding assistantkit.Grounding `json:"grounding"`
}

// AssetRef is the id+title of a campaign asset that informed generation.
type AssetRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// BriefResult reports whether the enrichBrief tool applied a new brief.
type BriefResult struct {
	Applied bool `json:"applied"`
	// UndoID reverses the change via POST /api/assistant/actions/:id/undo;
	// empty when the change could not be logged.
	UndoID string `json:"undoId,omitempty"`
	// Grounding is the campaign's sources the brief drew on.
	Grounding *assistantkit.Grounding `json:"grounding,omitempty"`
	// Fields are the brief's fields the enrichment rewrote, each with its
	// new value; none when it changed nothing.
	Fields []BriefField `json:"fields,omitempty"`
}

// BriefField is one rewritten brief field: description, targetPersona,
// keyMessages or toneGuidelines, and its new value.
type BriefField struct {
	Field string `json:"field"`
	Value string `json:"value"`
}

// CampaignAssistantRepos bundles the repository dependencies for the flow.
type CampaignAssistantRepos struct {
	// Messages is the assistant's history; draftPost reads the research
	// discussed in the thread from it.
	Messages  repository.AssistantMessageRepository
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
	// Proposals stores proposal-tier calls held back for the user to confirm.
	// nil fails every proposal-tier call closed instead of running it.
	Proposals repository.AssistantProposalRepository
	// Actions logs reversible changes so they can be undone. nil makes them
	// not undoable; the change itself still applies.
	Actions repository.AssistantActionRepository
	// Platforms is the platform catalog updateCampaignPlatforms checks
	// against. nil makes that tool unavailable.
	Platforms repository.PlatformRepository
	// Analyses counts the sources analysed for the campaign's state; nil
	// leaves the count out.
	Analyses repository.AssetAnalysisRepository
	// Evaluations reads the drafts' quality scores, so the budget guard can
	// offer to replace the weakest; nil leaves that option out.
	Evaluations repository.PostEvaluationRepository
	// Links maps the campaign's posts to their ideas, for the calendar
	// check's "same idea close together"; nil skips that check.
	Links repository.PostIdeaRepository
}

// CampaignAssistantFlowConfig holds the campaign pack's services and limits.
type CampaignAssistantFlowConfig struct {
	// Embedder embeds the askCampaignAssets query for chunk search.
	// A nil / unavailable embedder disables asset Q&A gracefully.
	Embedder ai.Embedder
	// Hub publishes "operation finalised" events on success/failure. nil = silent.
	Hub eventhub.Hub
	// Notifier drops a persistent "content plan ready" notification to the
	// campaign owner when a run generates a content plan — only that action, not
	// every assistant turn. nil = silent.
	Notifier *notify.Service

	// ContentPlan and EnrichBrief are the existing flow callbacks, invoked as
	// tools. Injected so the assistant reuses them verbatim — no duplicate LLM
	// generation. nil disables the corresponding tool.
	ContentPlan func(ctx context.Context, req content_plan.ContentPlanRequest, onEvent content_plan.OnEventFunc) (*content_plan.ContentPlanResponse, error)
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
	// ProposalThreshold is the largest batch generatePosts / draftPost create
	// without asking; a bigger request becomes a proposal. 0 falls back to 3.
	ProposalThreshold int
	// CheckBrief / CheckPosts back the read-only consistency review tools.
	// nil disables the corresponding tool.
	CheckBrief func(ctx context.Context, campaignID string, onEvent consistency.OnEventFunc) (*consistency.BriefReview, error)
	CheckPosts func(ctx context.Context, req consistency.PostsCheckRequest, onEvent consistency.OnEventFunc) (*consistency.PostsReview, error)
	// DeletePost deletes a post with its files, for the budget guard's
	// "replace the weakest drafts"; nil leaves that option out.
	DeletePost func(ctx context.Context, post *models.Post, actor string) (bool, error)
	// CancelPost withdraws a scheduled post from publishing, for a platform
	// removal that waits on it; nil offers no unschedule.
	CancelPost func(ctx context.Context, postID, actor string) error
}

// ── SSE types ─────────────────────────────────────────────────────────────────

// SSEEventKind identifies the SSE event types emitted by the assistant flow.
// The content_plan_* and enrich_brief_* events are the nested sub-flow events
// re-emitted namespaced so the client sees live progress while a tool runs.
type SSEEventKind string

const (
	SSEEventContentPlanStarted  SSEEventKind = "content_plan_started"
	SSEEventContentPlanStep     SSEEventKind = "content_plan_step"
	SSEEventContentPlanIdea     SSEEventKind = "content_plan_idea"
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
	SSEEventGeneratePostsIdea     SSEEventKind = "generate_posts_idea"
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
	SSEEventPlatformsUpdated   SSEEventKind = "platforms_updated"
	SSEEventGoalUpdated        SSEEventKind = "goal_updated"
	SSEEventPostsRedistributed SSEEventKind = "posts_redistributed"

	SSEEventCheckBriefStarted  SSEEventKind = "check_brief_started"
	SSEEventCheckBriefComplete SSEEventKind = "check_brief_complete"
	SSEEventCheckPostsStarted  SSEEventKind = "check_posts_started"
	SSEEventCheckPostsComplete SSEEventKind = "check_posts_complete"
)

// ContentPlanStartedEventPayload is emitted when the runContentPlan tool begins.
type ContentPlanStartedEventPayload struct{}

// ContentPlanCompleteEventPayload is emitted once the plan's ideas are saved.
type ContentPlanCompleteEventPayload struct {
	IdeaCount int                    `json:"ideaCount"`
	Warnings  []string               `json:"warnings,omitempty"`
	Grounding assistantkit.Grounding `json:"grounding"`
}

// EnrichBriefStartedEventPayload is emitted when the enrichBrief tool begins.
type EnrichBriefStartedEventPayload struct {
	Instruction string `json:"instruction,omitempty"`
}

// EnrichBriefCompleteEventPayload is emitted once the brief is applied.
type EnrichBriefCompleteEventPayload struct {
	Applied bool   `json:"applied"`
	UndoID  string `json:"undoId,omitempty"`
}

// GeneratePostsStartedEventPayload is emitted when the generatePosts tool begins.
type GeneratePostsStartedEventPayload struct {
	PlatformIDs []string `json:"platformIds"`
	PhaseID     string   `json:"phaseId"`
	Count       int      `json:"count"`
}

// GeneratePostsCompleteEventPayload is emitted once the targeted ideas are saved.
type GeneratePostsCompleteEventPayload struct {
	IdeaCount int                    `json:"ideaCount"`
	Warnings  []string               `json:"warnings,omitempty"`
	Grounding assistantkit.Grounding `json:"grounding"`
	UndoID    string                 `json:"undoId,omitempty"`
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
	UndoID            string `json:"undoId,omitempty"`
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

// OnEventFunc is an optional callback invoked as SSE events are produced.
// A nil OnEventFunc is valid — the flow runs silently.
type OnEventFunc func(name SSEEventKind, data any)
