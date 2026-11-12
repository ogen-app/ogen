package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
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
	proposals repository.AssistantProposalRepository
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

// WithProposals lets the thread's history say how each turn's offers stand
// now.
func (h *AssistantHandler) WithProposals(proposals repository.AssistantProposalRepository) *AssistantHandler {
	h.proposals = proposals
	return h
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
// @Description  and "tool_result" {"name","ref","ok","code"} for each tool, "ok" false when it refused or hit a conflict, "code" naming a refusal
// @Description  the client draws its own card for (editIdea's "idea_read_only": an analysed idea's title and note can't change); "pack_loaded" {"pack"} when the turn restarted
// @Description  with another domain's tools (deltas before it are void); one "ui_action" per change to the screen,
// @Description  once the reply is final: {"type": navigate|set_filter|select|open_saved_view|set_window|open_connect|open_add_source, "screen", "campaignId",
// @Description  "focus" {kind,id}, "filter", "selection" {kind,ids}, "savedViewId", "window" {from,to,platforms,campaignId},
// @Description  "connect" {platform,accountId,reconnect}}, every reference already checked; open_connect opens the settings
// @Description  screen's connect flow (reconnect for an account), which the client runs; open_add_source opens the
// @Description  campaign's Assets tab ready to upload a document or import a link, by "via" when set: upload opens the
// @Description  upload, page the import of a page;
// @Description  then "complete" with {"explanation","action","uiActions","touched":[{"kind","id"}],"packs","results"}, where
// @Description  touched names what changed (no id: every entity of the kind) for the client to refresh, and results
// @Description  (omitted when empty) holds each pack's tools' last successful output, by pack then tool name. The
// @Description  workspace pack's: activityReport {from,to,tz,published,failed,created,campaigns_created,days};
// @Description  workspaceSetup {timezone,autoPublish,autoPublishNote,accounts[{id,platform,username,needsReconnect,
// @Description  health,tokenExpiresAt,autoPublish}],note}; weeklyDigest {from,to,empty,report};
// @Description  emailWeeklyDigest {done,from,to}; postHistory {postId,title,entries[{at,kind: version|event,what,version,
// @Description  by,byId,source}],more,note}; setAutoPublish {done,autoPublish,undoId}; openConnect {done,note}.
// @Description  The ideas pack's, where an idea is {id,title,verdict,remindAt,origin,campaignId,fitScore,plan,posts} (remindAt: when a
// @Description  later idea comes back; once past, it is woken and counts as undecided though its verdict stays later) and grounding
// @Description  {mode: grounded|exploratory,sources[{id,title}]}: findIdeas {ideas,total,pileTotal (the filter's pile with no other filter; absent without a pile)}; setIdeaVerdict {idea,undoId};
// @Description  setIdeaVerdicts, moveIdeas and deleteIdeas {done,failed,stopped,ideas,verdict,remindAt,undoId}: ideas are the ideas a
// @Description  triage or move changed, as they are now (none for a delete); verdict (yes|later|no|none), remindAt and undoId are a triage's;
// @Description  compileIdeas {postId,title,status,campaignId,platformId,postType,date,posts,ideas,builtFrom,theses,grounding,undoId} (posts: each
// @Description  {postId,title,status,platformId,postType,date}, one per platform; the flat fields are the first; date empty when undated;
// @Description  ideas the count, builtFrom [{id,title}] the ideas in their theses' order, theses [{title,points}] every post's, one per idea); draftIdeas
// @Description  {posts[{postId,title,platformId,publishDate (none when unscheduled),excerpt,campaignId,ideaIds,usedAssetIds,grounding}],warnings}; getIdea {idea,note,angle,
// @Description  rationale,sources,fit{score,lens,campaignId,criteria[{key,question,weight,level,means,why,phase,campaign}],
// @Description  caps[{cap,meaning}]},sourceAsset{id,title},analysis{id,lens,gist,summary},posts,author,createdAt,explain};
// @Description  ideasInCommon {ideas[{id,title,angle}],missing,theme,angle,assets,campaigns,origins,lenses,platforms,postTypes,phases,
// @Description  terms,splits}, each list [{value,label,ideaIds}]; theme (a few words) and angle (a sentence) word the thread they share,
// @Description  absent when none could be worded; lenses are the analysis lens each idea was read and scored in ("workspace" or a campaign);
// @Description  splits [{field: campaign|lens|platform|postType|phase, values[{value,label,ideaIds}]}] are where they differ; editIdea {idea,note,before{title,note,plan},changed,undoId}; similarIdeas {ideaId,title,
// @Description  similar[{idea,score,why}]}; findDuplicateIdeas {groups[{keepId,similarity,ideas}],total,truncated};
// @Description  mergeIdeas {idea,deleted,groups[{idea,deleted}],stopped} (one per group merged; idea and deleted are the first's;
// @Description  stopped says why later groups weren't merged).
// @Description  Plus "unsaved": true when the turn ran but could not be stored in the thread. "error"
// @Description  carries {"message":"...","code":<http_code>} and ends the stream.
// @Description  Offers: runContentPlan, redistributePosts, updateCampaignPlatforms, moveIdeas, deleteIdeas, schedulePost,
// @Description  applyCalendarMoves, setAutoPublish and emailWeeklyDigest are always held back as an offer; generatePosts, draftPost, draftIdeas, moreLikeThese,
// @Description  setIdeaVerdicts and tagAssets only above a few items. Smaller batches and other tools (distillMoreIdeas
// @Description  among them) run straight away, so a client can't rely on an offer before every write. "proposal_created" {"id","tool","summary",
// @Description  "expiresAt","options","card","replaces","retryOf"} streams once the reply is final; "options" is an array of
// @Description  {"id","title"} with optional "description", "recommended" and "why" (at most one option is recommended:
// @Description  the card leads with it, and its "why" says in a line why it's the one to pick) and "input" {"kind": url,"label"}
// @Description  for an option that asks for a value, sent with its confirm. "proposal_resolved" {"id","status","chosen","reasons","replacedBy"} streams when one is
// @Description  answered in chat; "complete" lists them again as "resolved". Cards answer through POST
// @Description  /api/assistant/proposals/{id}/confirm or /dismiss. A confirmed offer re-checks first: when what it was made on
// @Description  changed, nothing runs, the offer stays open and resolves "stale" with "reasons", and what still holds
// @Description  may come back as a fresh offer ("replacedBy"; its card's "replaces"), the only one then confirmable. A
// @Description  yes after an offer's expiry resolves it "expired". A run that can
// @Description  do part of the job (setIdeaVerdicts, moveIdeas, draftIdeas, editBrand, unschedulePosts, deletePosts) has, in
// @Description  its result (complete.results and a card's answer alike), "made" of "total" items and, when some weren't,
// @Description  "notMade" [{id,title?,platform?,reason}] (platform: the platform id of an idea's post that wasn't written)
// @Description  and, only when some items are worth retrying and the offer could be stored, "retryProposalId": a fresh
// @Description  offer of just those, whose card has "retryOf". The thread records such an answer as "Confirmed: <summary>
// @Description  · <made> of <total> made", a stale one as "Stale: <summary>", an expired one as "Expired: <summary>".
// @Description  Packs stream their own progress between: the campaign pack forwards the campaign assistant's events
// @Description  (content_plan_idea, generate_posts_idea, draft_post_*, …), and draftIdeas sends "idea_draft_post" per post,
// @Description  shaped as one of its result's posts plus done (posts written so far) and total (posts planned).
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
// @Description  content is the JSON of that turn's "complete" event, and its "offers" are the offers the turn made
// @Description  or answered as they stand now: {id, status: pending|confirmed|dismissed|expired (a pending one past
// @Description  its expiry reads expired), chosen, expires_at, resolved_at, replaces, retry_of, replaced_by (the
// @Description  fresh offer made when a yes found things changed; the stale offer stays pending)}.
// @Tags         assistant
// @Produce      json
// @Security     CookieAuth
// @Param        scope  path  string  true  "workspace | campaign:<id> | post:<id>"
// @Success      200  {array}   models.AssistantMessage
// @Failure      400  {object}  map[string]string
// @Failure      401  {object}  map[string]string
// @Router       /api/assistant/threads/{scope}/messages [get]
func (h *AssistantHandler) Messages(c *fiber.Ctx) error {
	// Fiber hands the segment over still escaped, and clients encode the
	// scope's colon as %3A.
	scope, err := url.PathUnescape(c.Params("scope"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "scope is not a valid path segment")
	}
	kind, id, err := parseThreadScope(scope)
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
	if err := h.withOffers(reqCtx(c), msgs); err != nil {
		return err
	}
	return c.JSON(msgs)
}

// withOffers gives each model message the offers its turn made or answered,
// as they stand now.
func (h *AssistantHandler) withOffers(ctx context.Context, msgs []models.AssistantMessage) error {
	if h.proposals == nil {
		return nil
	}
	byMsg := make([][]string, len(msgs))
	var ids []string
	for i, m := range msgs {
		if m.Role == "model" {
			byMsg[i] = assistant.OfferIDs(m.Content)
			ids = append(ids, byMsg[i]...)
		}
	}
	rows, err := h.proposals.ListByIDs(ctx, ids)
	if err != nil {
		return err
	}
	// The offers made afresh are read on their own: the turn that made one
	// may not be in the thread, e.g. when its answer failed to save.
	fresh, err := h.proposals.ListReplacing(ctx, ids)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	states := make(map[string]models.AssistantOfferState, len(rows))
	for _, p := range rows {
		states[p.ID] = p.State(now)
	}
	// The latest offer made afresh for one is its replacement.
	slices.SortFunc(fresh, func(a, b models.AssistantProposal) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, p := range fresh {
		if s, ok := states[p.Replaces]; ok {
			s.ReplacedBy = p.ID
			states[p.Replaces] = s
		}
	}
	for i, offerIDs := range byMsg {
		for _, id := range offerIDs {
			if s, ok := states[id]; ok {
				msgs[i].Offers = append(msgs[i].Offers, s)
			}
		}
	}
	return nil
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
