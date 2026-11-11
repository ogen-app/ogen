package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

// threadMessages is how many of a thread's latest messages the history read
// returns.
const threadMessages = 50

// defaultThreads and maxThreads bound a page of the thread list.
const (
	defaultThreads = 50
	maxThreads     = 100
)

// threadPreviewLen caps a thread row's preview, in characters.
const threadPreviewLen = 140

// maxInstructionLen caps a turn's message, in characters. With the replayed
// history capped by message count, it keeps a turn's input well inside the
// model's context window.
const maxInstructionLen = 8000

// AssistantHandler serves the universal assistant: one endpoint for every
// screen, the workspace's thread list, and each thread's history.
type AssistantHandler struct {
	run       func(ctx context.Context, req assistant.Request, onEvent assistant.OnEventFunc) (*assistant.Response, error)
	available func() bool
	resolve   func(ctx context.Context, v *viewcontext.ViewContext) (*viewresolve.Resolved, error)
	messages  repository.AssistantMessageRepository
	activity  *activity.Recorder
	auth      fiber.Handler
}

// NewAssistantHandler wires the assistant endpoints. run nil, or available
// reporting false, leaves the turn endpoint at 503; activity nil is a no-op.
func NewAssistantHandler(
	run func(ctx context.Context, req assistant.Request, onEvent assistant.OnEventFunc) (*assistant.Response, error),
	available func() bool,
	resolve func(ctx context.Context, v *viewcontext.ViewContext) (*viewresolve.Resolved, error),
	messages repository.AssistantMessageRepository,
	activityRec *activity.Recorder,
	auth fiber.Handler,
) *AssistantHandler {
	return &AssistantHandler{run: run, available: available, resolve: resolve, messages: messages, activity: activityRec, auth: auth}
}

func (h *AssistantHandler) Register(app *fiber.App) {
	app.Post("/api/assistant", h.auth, h.Turn)
	app.Get("/api/assistant/threads", h.auth, h.Threads)
	app.Get("/api/assistant/threads/:scope/messages", h.auth, h.Messages)
}

type assistantTurnRequest struct {
	Instruction string `json:"instruction" validate:"required"`
	// ViewContext is the screen the message is sent from; its scope names the
	// thread. See domain/viewcontext for the schema.
	ViewContext json.RawMessage `json:"viewContext" validate:"required" swaggertype:"object"`
}

// Turn godoc
// @Summary      Universal assistant turn (SSE)
// @Description  Sends a message to the assistant from any screen and streams the reply. viewContext
// @Description  (version 1) is the screen: screen, scope ({kind: workspace|campaign|post, id}), focus,
// @Description  selection ({kind, ids | allMatching}), filter, savedViewId, sort, window and visible. It holds
// @Description  references only; the server reloads them in the caller's workspace and drops ids that no
// @Description  longer exist. The scope names the thread: the conversation continues across the screens of
// @Description  one workspace, campaign or post. A malformed viewContext is a 400 that names the field; a
// @Description  campaign or post scope that doesn't exist is a 404; an instruction over 8000 characters is a 400.
// @Description  Events, in order: "explanation_delta" {"delta"} as the reply streams; "tool_call" {"name","input","ref"}
// @Description  and "tool_result" {"name","ref","ok"} for each tool, "ok" false when it refused or hit a conflict; "pack_loaded" {"pack"} when the turn restarted
// @Description  with another domain's tools (deltas before it are void); one "ui_action" per change to the screen,
// @Description  once the reply is final: {"type": navigate|set_filter|select|open_saved_view|set_window|open_connect|open_add_source, "screen", "campaignId",
// @Description  "focus" {kind,id}, "filter", "selection" {kind,ids}, "savedViewId", "window" {from,to,platforms,campaignId},
// @Description  "connect" {platform,accountId,reconnect}}, every reference already checked; open_connect opens the settings
// @Description  screen's connect flow (reconnect for an account), which the client runs; open_add_source opens the
// @Description  campaign's Assets tab ready to upload a document or import a link;
// @Description  then "complete" with {"explanation","action","uiActions","touched":[{"kind","id"}],"packs","results"}, where
// @Description  touched names what changed (no id: every entity of the kind) for the client to refresh, and results
// @Description  (omitted when empty) holds each pack's tools' last successful output, by pack then tool name. The
// @Description  workspace pack's: activityReport {from,to,tz,published,failed,created,campaigns_created,days};
// @Description  workspaceSetup {timezone,autoPublish,autoPublishNote,accounts[{id,platform,username,needsReconnect,
// @Description  health,tokenExpiresAt,autoPublish}],note}; weeklyDigest {from,to,empty,report};
// @Description  emailWeeklyDigest {done,from,to}; postHistory {postId,title,entries[{at,kind: version|event,what,version,
// @Description  by,byId,source}],more,note}; setAutoPublish {done,autoPublish,undoId}; openConnect {done,note}.
// @Description  The ideas pack's, where an idea is {id,title,verdict,origin,campaignId,fitScore,plan} and grounding
// @Description  {mode: grounded|exploratory,sources[{id,title}]}: findIdeas {ideas,total}; setIdeaVerdict {idea,undoId};
// @Description  setIdeaVerdicts, moveIdeas and deleteIdeas {done,failed,stopped,undoId (setIdeaVerdicts only)};
// @Description  compileIdeas {postId,title,status,campaignId,platformId,date,posts,ideas,grounding,undoId} (posts: each {postId,title,status,platformId,date}, one per platform; the flat fields are the first; date empty when undated); draftIdeas
// @Description  {posts[{postId,title,platformId,publishDate (none when unscheduled),ideaIds,usedAssetIds,grounding}],warnings}; getIdea {idea,note,angle,
// @Description  rationale,sources,fit{score,lens,campaignId,criteria[{key,question,weight,level,means,why,phase,campaign}],
// @Description  caps[{cap,meaning}]},sourceAsset{id,title},analysis{id,lens,gist,summary},posts,author,createdAt,explain};
// @Description  ideasInCommon {ideas[{id,title,angle}],missing,assets,campaigns,origins,lenses,platforms,postTypes,phases,
// @Description  terms}, each [{value,label,ideaIds}]; editIdea {idea,note,changed,undoId}; similarIdeas {ideaId,title,
// @Description  similar[{idea,score,why}]}; findDuplicateIdeas {groups[{keepId,similarity,ideas}],total,truncated};
// @Description  mergeIdeas {idea,deleted}.
// @Description  Plus "unsaved": true when the turn ran but could not be stored in the thread. "error"
// @Description  carries {"message":"...","code":<http_code>} and ends the stream.
// @Description  Offers: runContentPlan, redistributePosts, updateCampaignPlatforms, moveIdeas, deleteIdeas, schedulePost,
// @Description  applyCalendarMoves, setAutoPublish and emailWeeklyDigest are always held back as an offer; generatePosts, draftPost, draftIdeas, moreLikeThese,
// @Description  setIdeaVerdicts and tagAssets only above a few items. Smaller batches and other tools (distillMoreIdeas
// @Description  among them) run straight away, so a client can't rely on an offer before every write. "proposal_created" {"id","tool","summary",
// @Description  "expiresAt","options"} streams once the reply is final; "options" is an array of {"id","title"} with optional
// @Description  "description" and "recommended" (at most one option is recommended: the card leads with it). "proposal_resolved"
// @Description  {"id","status","chosen"} streams when one is answered in chat. Cards answer through POST
// @Description  /api/assistant/proposals/{id}/confirm or /dismiss. A confirmed offer re-checks first: when what it was made on
// @Description  changed, nothing runs and either it is refused or what still holds comes back as a fresh offer. A run that
// @Description  did part of the job (setIdeaVerdicts, moveIdeas, draftIdeas, editBrand, unschedulePosts, deletePosts) has, in
// @Description  its result (complete.results and a card's answer alike), "notMade" [{id,title?,reason}] and, only when some
// @Description  items are worth retrying and the offer could be stored, "retryProposalId": a fresh offer of just those.
// @Description  Packs stream their own progress between: the campaign pack forwards the campaign assistant's events
// @Description  (content_plan_idea, generate_posts_idea, draft_post_*, …), and draftIdeas sends "idea_draft_post" per post,
// @Description  shaped as one of its result's posts.
// @Tags         assistant
// @Accept       json
// @Produce      text/event-stream
// @Security     CookieAuth
// @Param        body  body  assistantTurnRequest  true  "Message and screen"
// @Success      200  "SSE stream: explanation_delta / tool_call / tool_result / complete / error"
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Failure      404  {object}  map[string]string
// @Failure      503  {object}  map[string]string
// @Router       /api/assistant [post]
func (h *AssistantHandler) Turn(c *fiber.Ctx) error {
	if h.run == nil || (h.available != nil && !h.available()) {
		return fiber.NewError(fiber.StatusServiceUnavailable, "assistant is not available")
	}
	var req assistantTurnRequest
	if err := bindAndValidate(c, &req); err != nil {
		return err
	}
	if utf8.RuneCountInString(req.Instruction) > maxInstructionLen {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("instruction must be at most %d characters", maxInstructionLen))
	}
	session, err := sessionFrom(c)
	if err != nil {
		return err
	}
	view, err := viewcontext.Parse(req.ViewContext)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	// The view resolves before the stream opens, so a bad reference is a
	// plain status code rather than an error event.
	resolved, err := h.resolve(reqCtx(c), view)
	switch {
	case errors.Is(err, viewresolve.ErrScopeNotFound):
		return fiber.NewError(fiber.StatusNotFound, string(view.Scope.Kind)+" not found")
	case err != nil:
		if ie, ok := errors.AsType[*viewcontext.InvalidError](err); ok {
			return fiber.NewError(fiber.StatusBadRequest, ie.Error())
		}
		return err
	}

	h.activity.Record(reqCtx(c), activity.CategoryAIFlow, "assistant_turn",
		activity.WithEntity(string(view.Scope.Kind), view.Scope.ID),
		activity.WithSource(activity.SourceAssistant),
	)

	// The stream writer outlives the request buffer; copy what it reads. The
	// view was decoded into fresh strings already.
	turn := assistant.Request{Instruction: strings.Clone(req.Instruction), View: resolved}
	run := h.run
	flowCtx := detachedContext(c, session.TenantID)
	streamFlow(c, func(emit sseEmit) {
		_, err := run(flowCtx, turn, func(kind assistant.SSEEventKind, data any) {
			emit(string(kind), data)
		})
		if err != nil {
			emit(string(assistant.SSEEventError), flowError[*assistant.ValidationError, *assistant.AIError](err))
		}
	})
	return nil
}

// Messages godoc
// @Summary      Universal assistant thread
// @Description  Returns a thread's latest messages, oldest first. scope is "workspace", "campaign:<id>" or
// @Description  "post:<id>". A user message carries the view_context it was sent from; a model message's
// @Description  content is the JSON of that turn's "complete" event.
// @Tags         assistant
// @Produce      json
// @Security     CookieAuth
// @Param        scope  path  string  true  "workspace | campaign:<id> | post:<id>"
// @Success      200  {array}   models.AssistantMessage
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Router       /api/assistant/threads/{scope}/messages [get]
func (h *AssistantHandler) Messages(c *fiber.Ctx) error {
	kind, id, err := parseThreadScope(c.Params("scope"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	msgs, err := h.messages.ListRecent(reqCtx(c), kind, id, threadMessages)
	if err != nil {
		return err
	}
	if msgs == nil {
		msgs = []models.AssistantMessage{}
	}
	return c.JSON(msgs)
}

// assistantThreadRow is one thread in the list.
type assistantThreadRow struct {
	// Scope is the thread's address for the history read.
	Scope     string                    `json:"scope"`
	ScopeKind models.AssistantScopeKind `json:"scope_kind" swaggertype:"string" enums:"workspace,campaign,post"`
	ScopeID   string                    `json:"scope_id"`
	// Title is the campaign's name or the post's title; empty for the
	// workspace.
	Title string `json:"title"`
	// CampaignID and CampaignTitle name the thread's campaign: its own for a
	// campaign thread, the post's for a post thread. Empty for the workspace.
	CampaignID    string `json:"campaign_id"`
	CampaignTitle string `json:"campaign_title"`
	// Archived reports that the thread's campaign is archived.
	Archived      bool      `json:"archived"`
	LastMessageAt time.Time `json:"last_message_at"`
	LastRole      string    `json:"last_role" enums:"user,model"`
	// Preview is the start of the latest message as plain text: the user's
	// words, or the model's explanation.
	Preview      string `json:"preview"`
	MessageCount int    `json:"message_count"`
}

// Threads godoc
// @Summary      Universal assistant threads
// @Description  Lists the workspace's threads, the latest message first. Threads are the workspace's, not one
// @Description  user's. A campaign thread is left out once its campaign is deleted, and a post thread once its
// @Description  post or its campaign is; an archived campaign's threads are listed with archived set. For the
// @Description  next page, pass the last row's last_message_at as before and its scope as before_scope; with
// @Description  before alone, the page starts at the first thread whose latest message is earlier.
// @Tags         assistant
// @Produce      json
// @Security     CookieAuth
// @Param        limit         query  int     false  "Page size, 1-100 (default 50)"
// @Param        before        query  string  false  "The last row's last_message_at (RFC 3339)"
// @Param        before_scope  query  string  false  "The last row's scope; needs before"
// @Success      200  {array}   assistantThreadRow
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Router       /api/assistant/threads [get]
func (h *AssistantHandler) Threads(c *fiber.Ctx) error {
	limit := defaultThreads
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxThreads {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxThreads))
		}
		limit = n
	}
	var cursor *repository.ThreadCursor
	if v := c.Query("before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "before must be RFC3339")
		}
		cursor = &repository.ThreadCursor{LastMessageAt: t}
	}
	if v := c.Query("before_scope"); v != "" {
		if cursor == nil {
			return fiber.NewError(fiber.StatusBadRequest, "before_scope needs before")
		}
		kind, id, err := parseThreadScope(v)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "before_scope: "+err.Error())
		}
		cursor.ScopeKind, cursor.ScopeID = kind, id
	}
	threads, err := h.messages.ListThreads(reqCtx(c), cursor, limit)
	if err != nil {
		return err
	}
	rows := make([]assistantThreadRow, 0, len(threads))
	for _, t := range threads {
		scope := string(t.ScopeKind)
		if t.ScopeKind != models.AssistantScopeWorkspace {
			scope += ":" + t.ScopeID
		}
		rows = append(rows, assistantThreadRow{
			Scope:         scope,
			ScopeKind:     t.ScopeKind,
			ScopeID:       t.ScopeID,
			Title:         t.Title,
			CampaignID:    t.CampaignID,
			CampaignTitle: t.CampaignTitle,
			Archived:      t.Archived,
			LastMessageAt: t.LastMessageAt,
			LastRole:      t.LastRole,
			Preview:       threadPreview(t.LastRole, t.LastContent),
			MessageCount:  t.MessageCount,
		})
	}
	return c.JSON(rows)
}

// threadPreview is a message as one line of plain text, cut to
// threadPreviewLen characters. A model message is stored as its "complete"
// event's JSON, so its preview is the explanation; one that doesn't decode
// has none rather than showing the JSON.
func threadPreview(role, content string) string {
	text := content
	if role == "model" {
		var env struct {
			Explanation string `json:"explanation"`
		}
		if err := json.Unmarshal([]byte(content), &env); err != nil {
			return ""
		}
		text = env.Explanation
	}
	text = strings.Join(strings.Fields(strings.ReplaceAll(text, "**", "")), " ")
	if utf8.RuneCountInString(text) <= threadPreviewLen {
		return text
	}
	runes := []rune(text)
	return strings.TrimRight(string(runes[:threadPreviewLen-1]), " ") + "…"
}

// parseThreadScope reads "workspace", "campaign:<id>" or "post:<id>".
func parseThreadScope(s string) (models.AssistantScopeKind, string, error) {
	kind, id, _ := strings.Cut(s, ":")
	switch models.AssistantScopeKind(kind) {
	case models.AssistantScopeWorkspace:
		if id == "" {
			return models.AssistantScopeWorkspace, "", nil
		}
	case models.AssistantScopeCampaign, models.AssistantScopePost:
		if id != "" && len(id) <= viewcontext.MaxIDLen {
			return models.AssistantScopeKind(kind), id, nil
		}
	}
	return "", "", errors.New(`scope must be "workspace", "campaign:<id>" or "post:<id>"`)
}
