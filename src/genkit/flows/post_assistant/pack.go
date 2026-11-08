package post_assistant

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
)

// PackName is the post pack's name.
const PackName = "post"

const logComponent = "genkit.post_pack"

//go:embed prompts/post_assistant.tmpl
var promptFS embed.FS

// emit calls onEvent when it is non-nil.
func emit(onEvent OnEventFunc, name SSEEventKind, data any) {
	if onEvent != nil {
		onEvent(name, data)
	}
}

// prompts holds the pack's rules, the writer's instructions and the
// template of the per-turn post block.
type prompts struct {
	rules   string
	writer  string
	context *template.Template
}

func loadPrompts() (*prompts, error) {
	raw, err := promptFS.ReadFile("prompts/post_assistant.tmpl")
	if err != nil {
		return nil, fmt.Errorf("load post_assistant.tmpl: %w", err)
	}
	tmpl, err := template.New("post_assistant").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse post_assistant.tmpl: %w", err)
	}
	rulesTmpl, writerTmpl, contextTmpl := tmpl.Lookup("rules"), tmpl.Lookup("writer"), tmpl.Lookup("context")
	if rulesTmpl == nil || writerTmpl == nil || contextTmpl == nil {
		return nil, fmt.Errorf(`post_assistant.tmpl must define the "rules", "writer" and "context" blocks`)
	}
	rules, err := flowkit.RenderTemplate(rulesTmpl, nil)
	if err != nil {
		return nil, fmt.Errorf("render post rules: %w", err)
	}
	writer, err := flowkit.RenderTemplate(writerTmpl, nil)
	if err != nil {
		return nil, fmt.Errorf("render writer instructions: %w", err)
	}
	return &prompts{rules: rules, writer: strings.TrimSpace(writer), context: contextTmpl}, nil
}

// NewPack is the post pack: editing one post — its copy, through a capable
// writer behind editPost, its notes, versions, clones and schedule — and
// answering from its assets. It works on the thread's post, or the one in
// focus.
func NewPack(g *genkit.Genkit, cfg PostAssistantFlowConfig, repos PostAssistantRepos) (assistant.Pack, error) {
	p, err := loadPrompts()
	if err != nil {
		return assistant.Pack{}, err
	}
	return assistant.Pack{
		Name:         PackName,
		Summary:      "edit one post's copy, add notes to it, restore its versions, clone it to another platform, schedule it, and answer from its assets",
		Scopes:       []models.AssistantScopeKind{models.AssistantScopePost},
		Instructions: p.rules,
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			return defineTools(g), nil
		},
		Begin: func(ctx context.Context, t assistant.Turn) (context.Context, string, error) {
			return begin(ctx, g, cfg, repos, p, t)
		},
		End: end,
	}, nil
}

// begin loads the turn's post and readies its tools; the block is the post,
// its campaign, notes, versions and assets, and when it can be scheduled.
func begin(ctx context.Context, g *genkit.Genkit, cfg PostAssistantFlowConfig, repos PostAssistantRepos, p *prompts, t assistant.Turn) (context.Context, string, error) {
	id := t.Subject(models.AssistantScopePost)
	if id == "" {
		return nil, "", assistantkit.Refused("this message isn't about one post; open the post, or ask the user which one")
	}
	post, err := repos.Posts.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", assistantkit.Refused("that post no longer exists")
	}
	if err != nil {
		return nil, "", fmt.Errorf("load post: %w", err)
	}
	if err := ensureInitialVersion(ctx, repos, post); err != nil {
		return nil, "", err
	}
	actx, err := assembleContextCached(ctx, post, repos, p.context)
	if err != nil {
		return nil, "", fmt.Errorf("assemble post context: %w", err)
	}
	st := &requestState{
		postID:          post.ID,
		postStatus:      post.Status,
		assetIDs:        post.UsedAssetIDs,
		repos:           repos,
		embedder:        cfg.Embedder,
		cloneSvc:        cfg.CloneService,
		restoreSvc:      cfg.RestoreService,
		scheduleSvc:     cfg.ScheduleService,
		noteSvc:         cfg.NoteService,
		qualityAuto:     cfg.QualityAuto,
		hub:             cfg.Hub,
		owner:           post.CreatedBy,
		actor:           post.CreatedBy,
		platforms:       loadPlatforms(ctx, repos),
		g:               g,
		provider:        cfg.Provider,
		recorder:        cfg.Recorder,
		writerMaxTokens: cfg.MaxOutputTokens,
		// The writer sees the post block too, so it knows the campaign's
		// voice and the current content without a second read.
		writerSystem: p.writer + "\n\n" + actx.ContextBlock,
	}
	// The writer's copy and the tools' progress stream to the client
	// through the turn.
	st.onEvent = func(kind SSEEventKind, data any) { assistant.Emit(ctx, string(kind), data) }
	block := "<post id=\"" + post.ID + "\">\n" + strings.TrimSpace(actx.ContextBlock)
	if cfg.ScheduleService != nil {
		if sb := buildSchedulingContext(ctx, post, repos); sb != "" {
			block += "\n\n" + strings.TrimSpace(sb)
		}
	}
	return withRequestState(ctx, st), block + "\n</post>\n\n", nil
}

// loadPlatforms backs the clonePost tool's platform-name resolution. It is
// best-effort: a failure only disables cross-platform clones.
func loadPlatforms(ctx context.Context, repos PostAssistantRepos) []models.Platform {
	if repos.Platforms == nil {
		return nil
	}
	ps, err := repos.Platforms.List(ctx)
	if err != nil {
		slog.WarnContext(ctx, "load platforms failed, clone name-resolution degraded", logging.AttrComponent, logComponent, logging.AttrError, err)
		return nil
	}
	return ps
}

// end streams the completion of each committed tool result, announces the
// work on the post, and returns the turn's post result.
func end(ctx context.Context) (*assistant.PackResult, error) {
	st := getRequestState(ctx)
	r := st.result()
	if r == nil {
		return nil, nil
	}
	if cr := r.CloneResult; cr != nil {
		assistant.Emit(ctx, string(SSEEventCloneComplete), CloneCompleteEventPayload(*cr))
		assistantkit.Touch(ctx, "post", cr.NewPostID)
	}
	if rr := r.RestoreResult; rr != nil {
		assistant.Emit(ctx, string(SSEEventRestoreComplete), RestoreCompleteEventPayload(*rr))
		assistantkit.Touch(ctx, "post", st.postID)
	}
	if sr := r.ScheduleResult; sr != nil {
		assistant.Emit(ctx, string(SSEEventScheduleComplete), ScheduleCompleteEventPayload(*sr))
		assistantkit.Touch(ctx, "post", st.postID)
	}
	if len(r.NotesCreated) > 0 {
		assistantkit.Touch(ctx, "note", "")
	}
	publishFinalised(st.hub, st.postID, st.owner, r.Action)
	return &assistant.PackResult{Value: r, Fallback: r.Explanation}, nil
}

// result is what the turn's tools committed, labelled by the most telling:
// a clone, restore or schedule over an edit, and an edit over notes. nil
// when nothing was committed.
func (st *requestState) result() *PostAssistantResponse {
	r := &PostAssistantResponse{}
	if e := st.editResult; e != nil {
		r.Action, r.UpdatedContent, r.SaveVersion, r.VersionNote = actionEdited, e.Content, e.SaveVersion, e.Note
		r.Explanation = "Updated the post."
	}
	if n := len(st.noteResults); n > 0 {
		for _, note := range st.noteResults {
			r.NotesCreated = append(r.NotesCreated, NotePayload{ID: note.ID, Type: string(note.Type), Title: note.Title, Body: note.Body})
		}
		if r.Action == "" {
			r.Action, r.Explanation = actionNoted, "Saved a note."
			if n > 1 {
				r.Explanation = fmt.Sprintf("Saved %d notes.", n)
			}
		}
	}
	if sr := st.scheduleResult; sr != nil {
		mode := "auto-publish"
		if !sr.AutoPublish {
			mode = "manual publishing"
		}
		r.Action, r.Explanation = actionScheduled, fmt.Sprintf("Scheduled this post for %s (%s).", sr.ScheduledAt.Format("Jan 2, 2006 15:04 MST"), mode)
		r.ScheduleResult = &ScheduleResultPayload{ScheduledAt: sr.ScheduledAt.Format(time.RFC3339), Status: string(sr.Status), AutoPublish: sr.AutoPublish, Promoted: sr.Promoted}
	}
	if rr := st.restoreResult; rr != nil {
		r.Action, r.UpdatedContent = actionRestored, rr.Post.Content
		r.Explanation = fmt.Sprintf("Restored the post to version %d (saved as version %d). Your previous content is kept in the history.", rr.RestoredFromVersion, rr.NewVersionNumber)
		if rr.NoOp {
			r.Explanation = fmt.Sprintf("The post already matches version %d — nothing to restore.", rr.RestoredFromVersion)
		}
		r.RestoreResult = &RestoreResultPayload{RestoredFromVersion: rr.RestoredFromVersion, NewVersionNumber: rr.NewVersionNumber, NoOp: rr.NoOp}
	}
	if cr := st.cloneResult; cr != nil {
		r.Action, r.Explanation = actionCloned, fmt.Sprintf("Cloned this post into a new draft (#%s).", cr.Post.ID)
		r.CloneResult = &CloneResultPayload{NewPostID: cr.Post.ID, PlatformID: cr.Post.PlatformID, PostType: cr.ResolvedPostType, Adapted: cr.Adapted}
	}
	if r.Action == "" {
		return nil
	}
	return r
}

// Action values of PostAssistantResponse.Action.
const (
	actionEdited    = "edited"
	actionCloned    = "cloned"
	actionRestored  = "restored"
	actionScheduled = "scheduled"
	actionNoted     = "noted"
)

// publishFinalised announces committed work on the post on the shared event
// hub, so its other open tabs refresh. Topic is "entity:post:<id>", type
// "assistant.completed".
func publishFinalised(hub eventhub.Hub, postID, ownerID, action string) {
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
		Topic:   "entity:post:" + postID,
		UserID:  ownerID,
		Type:    "assistant.completed",
		Payload: map[string]any{"postId": postID, "action": action},
	}
	if err := hub.Publish(context.Background(), ev); err != nil {
		slog.Error("hub publish failed", logging.AttrComponent, logComponent, "post_id", postID, logging.AttrError, err)
	}
}
