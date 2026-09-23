package post_assistant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"
	"github.com/pgvector/pgvector-go"

	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/embedopts"
	"github.com/ogen-app/ogen/src/infra/vendors/llm"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// Per-turn token budget for tool-retrieved chunks.
const chunkTokenBudget = 3000

// minSearchSimilarity is the cosine-similarity floor a chunk must clear to be
// returned by the semantic-search tool (searchAssetChunks).
// CON-101: a conservative starting threshold; revisit against Gemini Embedding
// 2's cosine distribution once there is real corpus data.
const minSearchSimilarity = 0.5

// ── Context key for per-request state ────────────────────────────────────────

type ctxKey int

const requestStateKey ctxKey = iota

type requestState struct {
	postID string
	// postStatus is the post's status at the start of the turn, captured so
	// the editPost write-tool can refuse on a submitted post (CON-251): the
	// assistant stays present and readable there but must not rewrite content
	// that already lives outside Ogen. Read-only within a turn.
	postStatus models.PostStatus
	assetIDs   []string
	repos      PostAssistantRepos
	embedder   ai.Embedder

	// Clone support (CON-59). cloneSvc/actor drive the clonePost tool;
	// platforms lets it resolve a platform name → ID; onEvent lets it
	// emit a clone_started SSE event mid-generation; cloneResult is set
	// by the tool and read by the runner after generation to finalise
	// the response.
	cloneSvc    *clone.Service
	actor       string
	platforms   []models.Platform
	onEvent     OnEventFunc
	cloneResult *clone.Result

	// Restore support (CON-68). restoreSvc backs the restoreVersion tool;
	// restoreResult is set by the tool and read by the runner after
	// generation to finalise the response.
	restoreSvc    *restore.Service
	restoreResult *restore.Result

	// Schedule support (CON-78). scheduleSvc backs the schedulePost tool;
	// scheduleResult is set by the tool and read by the runner after
	// generation to finalise the response.
	scheduleSvc    *schedule.Service
	scheduleResult *schedule.Result

	// Note support (CON-188). noteSvc backs the createNote tool; noteResults
	// collects every note created this turn, read by the runner after
	// generation to finalise the response.
	noteSvc     *notes.Service
	noteResults []*models.PostNote

	// Writer support (CON-128). In the hybrid path the planner loop runs on
	// the cheap planning model and delegates all copywriting to the Sonnet
	// writer via the editPost tool (and clonePost adaptation). g runs the
	// nested writer generation; provider/recorder resolve the generation model
	// and meter it; writerSystem is the writer prompt + context block assembled
	// per-request; writerMaxTokens caps the writer output. editResult holds the
	// content the writer produced this turn, read by the runner to finalise an
	// "edited" response — the content never flows back through the planner model.
	g               *genkit.Genkit
	provider        *llm.Provider
	recorder        *usage.Recorder
	writerSystem    string
	writerMaxTokens int64
	editResult      *editResult

	// retrieved holds the asset excerpts the planner pulled via the
	// asset-retrieval tools this turn (CON-128). The writer's context block
	// carries only short asset previews, so an asset-grounded edit must be fed
	// the full retrieved text — captured here and passed to the writer as
	// source material (see composeWriterInstruction).
	retrieved []retrievedExcerpt
}

// editResult is the internal outcome of the editPost write-tool (CON-128): the
// full post content the Sonnet writer produced this turn.
type editResult struct {
	Content string
}

// retrievedExcerpt is a chunk the planner pulled via an asset-retrieval tool
// this turn, captured so the writer sub-call can ground on the full retrieved
// text rather than the short preview in the context block.
type retrievedExcerpt struct {
	AssetID string
	ChunkID string
	Content string
}

// captureExcerpts records the chunks an asset-retrieval tool returned so the
// writer can use them as source material. Deduped by chunk ID across calls, so
// re-retrieving the same chunk in a later turn-step doesn't duplicate it.
func (st *requestState) captureExcerpts(assetID string, out *ChunksOutput) {
	if out == nil {
		return
	}
	for _, c := range out.Chunks {
		dup := false
		for _, e := range st.retrieved {
			if e.ChunkID == c.ID {
				dup = true
				break
			}
		}
		if !dup {
			st.retrieved = append(st.retrieved, retrievedExcerpt{AssetID: assetID, ChunkID: c.ID, Content: c.Content})
		}
	}
}

// recordScheduled reflects a schedule committed this turn into the per-turn
// state (CON-251): it stashes the result for the runner to finalise AND
// advances postStatus to the routed status. The status bump is load-bearing —
// without it a schedulePost-then-editPost sequence in a single generation would
// still see the start-of-turn status and rewrite content that just locked at
// schedule time. editPost re-reads postStatus (via IsSubmitted), so the auto-
// publish case now refuses; a manual-publish schedule stays editable, which is
// correct (nothing is submitted yet).
func (st *requestState) recordScheduled(res *schedule.Result) {
	st.scheduleResult = res
	if res != nil {
		st.postStatus = res.Status
	}
}

func withRequestState(ctx context.Context, s *requestState) context.Context {
	return context.WithValue(ctx, requestStateKey, s)
}

func getRequestState(ctx context.Context) *requestState {
	return ctx.Value(requestStateKey).(*requestState)
}

// ���─ Tool input/output types ──────────────────────────────────────────────────

// AssetInfo is the output element for the listAssets tool.
type AssetInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	ChunkCount int    `json:"chunkCount"`
}

// GetChunksInput is the input for the getAssetChunks tool.
type GetChunksInput struct {
	AssetID  string   `json:"assetId"            jsonschema:"description=The asset ID to retrieve chunks from"`
	ChunkIDs []string `json:"chunkIds,omitempty"  jsonschema:"description=Specific chunk IDs to retrieve; omit to get all chunks for the asset"`
}

// ChunkContent is a single chunk returned by chunk-retrieval tools.
type ChunkContent struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	Content string `json:"content"`
}

// ChunksOutput wraps chunk results with a truncation indicator.
type ChunksOutput struct {
	Chunks    []ChunkContent `json:"chunks"`
	Truncated bool           `json:"truncated"`
}

// SearchChunksInput is the input for the searchAssetChunks tool.
type SearchChunksInput struct {
	AssetID string `json:"assetId" jsonschema:"description=The asset ID to search within"`
	Query   string `json:"query"   jsonschema:"description=Natural-language search query"`
}

// ClonePostInput is the input for the clonePost tool.
type ClonePostInput struct {
	TargetPlatform string `json:"targetPlatform,omitempty" jsonschema:"description=Platform name or ID for the clone (e.g. Threads). Omit to keep the source's platform."`
	TargetPostType string `json:"targetPostType,omitempty" jsonschema:"description=Post-type slug for the target platform (e.g. text-post). Omit to inherit or default."`
	Content        string `json:"content,omitempty"        jsonschema:"description=Full Markdown content for the clone. Normally leave empty — for a cross-platform clone the server's writer adapts the copy; provide content only to override it entirely. Omit to copy the source content verbatim (same-platform clone)."`
	Instruction    string `json:"instruction,omitempty"    jsonschema:"description=Optional steer for a cross-platform adaptation (e.g. 'keep it short'), forwarded to the writer. Ignored for a verbatim same-platform clone."`
	Title          string `json:"title,omitempty"          jsonschema:"description=Title for the clone. Omit to apply the default naming."`
}

// EditPostInput is the input for the editPost tool (CON-128). The planner
// forwards the user's change request verbatim; the Sonnet writer produces the
// full updated post from it.
type EditPostInput struct {
	Instruction string `json:"instruction" jsonschema:"description=The user's change request, forwarded verbatim (e.g. 'shorten the intro' or 'write the post from the draft thesis'). Do not paraphrase or pre-write the content."`
	Mode        string `json:"mode,omitempty" jsonschema:"description=edit for a change to existing content; draft to write the post body from a draft_thesis outline.,enum=edit,enum=draft"`
}

// EditPostOutput is the compact receipt returned to the planner after the
// writer runs. The full content deliberately does NOT come back through the
// planner model — it streams to the client and the runner finalises it from
// requestState — so the cheap planner stays cheap and can't mangle the copy.
type EditPostOutput struct {
	OK    bool `json:"ok"`
	Chars int  `json:"chars"`
	// Reason is set (with OK=false) when the edit was refused rather than
	// failed — e.g. the post is submitted and its content is locked (CON-251).
	// The planner relays it to the user instead of retrying.
	Reason string `json:"reason,omitempty"`
}

// ClonePostOutput is returned to the model after a clone is created.
type ClonePostOutput struct {
	NewPostID        string `json:"newPostId"`
	PlatformID       string `json:"platformId,omitempty"`
	PostType         string `json:"postType,omitempty"`
	Adapted          bool   `json:"adapted"`
	PostTypeFellBack bool   `json:"postTypeFellBack"`
}

// RestoreVersionInput is the input for the restoreVersion tool. Provide
// either an absolute versionNumber or a relative reference.
type RestoreVersionInput struct {
	VersionNumber int    `json:"versionNumber,omitempty" jsonschema:"description=The version number to restore to (e.g. 2). Take it from the Version history in the context."`
	Relative      string `json:"relative,omitempty"      jsonschema:"description=Relative reference instead of a number; use \"previous\" for the version before the current one (also covers \"undo\" / \"go back\")."`
}

// RestoreVersionOutput is returned to the model after a restore.
type RestoreVersionOutput struct {
	RestoredFromVersion int  `json:"restoredFromVersion"`
	NewVersionNumber    int  `json:"newVersionNumber"`
	NoOp                bool `json:"noOp"`
}

// SchedulePostInput is the input for the schedulePost tool. Resolve any
// relative expression ("tomorrow 9am") to an absolute instant first,
// using the current time + workspace timezone from the scheduling
// context, and pass it here as ISO-8601 with an explicit offset (e.g.
// "2026-06-15T09:00:00+03:00" or a UTC "...Z").
type SchedulePostInput struct {
	ScheduledAt  string `json:"scheduledAt"            jsonschema:"description=Absolute publish time as ISO-8601 with a timezone offset (e.g. 2026-06-15T09:00:00+03:00). Resolve relative phrases against the scheduling context before calling."`
	AllowPromote bool   `json:"allowPromote,omitempty" jsonschema:"description=Set true to auto-promote a draft (run pre-publish validation then Draft → ReadyForPublish) before scheduling. Required when the post is still a draft."`
}

// SchedulePostOutput is returned to the model after a schedule commits.
type SchedulePostOutput struct {
	ScheduledAt string `json:"scheduledAt"`
	Status      string `json:"status"`
	AutoPublish bool   `json:"autoPublish"`
	Promoted    bool   `json:"promoted"`
}

// CreateNoteInput is the input for the createNote tool (CON-188).
type CreateNoteInput struct {
	Type  string `json:"type"            jsonschema:"description=The kind of note. Use image_prompt for an image-generation prompt (e.g. a Nano Banana prompt); use note for any other side note or idea.,enum=image_prompt,enum=note"`
	Title string `json:"title,omitempty" jsonschema:"description=Optional short title for the note."`
	Body  string `json:"body"            jsonschema:"description=The note content (the prompt text, idea, or note body)."`
}

// CreateNoteOutput is returned to the model after a note is created.
type CreateNoteOutput struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// ── Tool registration ────────────────────────────────────────────────────────

type toolSet struct {
	listAssets        ai.ToolRef
	getAssetChunks    ai.ToolRef
	searchAssetChunks ai.ToolRef
	getCurrentContent ai.ToolRef
	clonePost         ai.ToolRef
	restoreVersion    ai.ToolRef
	schedulePost      ai.ToolRef
	createNote        ai.ToolRef
	// editPost (CON-128) is the Sonnet write-tool used only in the hybrid
	// planner path; the legacy single-Sonnet loop does not attach it.
	editPost ai.ToolRef
}

func defineTools(g *genkit.Genkit) *toolSet {
	list := genkit.DefineTool(g, "listAssets",
		"Returns the list of assets attached to the current post with their ID, name, type, and chunk count.",
		func(ctx *ai.ToolContext, _ struct{}) ([]AssetInfo, error) {
			return toolListAssets(ctx)
		},
	)

	getChunks := genkit.DefineTool(g, "getAssetChunks",
		"Retrieves text chunks from a specific asset. Omit chunkIds to get all chunks (subject to token budget).",
		func(ctx *ai.ToolContext, in GetChunksInput) (*ChunksOutput, error) {
			return toolGetAssetChunks(ctx, in)
		},
	)

	searchChunks := genkit.DefineTool(g, "searchAssetChunks",
		"Semantic search over an asset's chunks. Returns the most relevant chunks for the given query.",
		func(ctx *ai.ToolContext, in SearchChunksInput) (*ChunksOutput, error) {
			return toolSearchAssetChunks(ctx, in)
		},
	)

	getCurrentContent := genkit.DefineTool(g, "getCurrentContent",
		"Returns the latest post content as plain text.",
		func(ctx *ai.ToolContext, _ struct{}) (string, error) {
			return toolGetCurrentContent(ctx)
		},
	)

	clonePost := genkit.DefineTool(g, "clonePost",
		"Duplicates the current post as a new draft in the same campaign. "+
			"For a cross-platform clone, set targetPlatform and OMIT content — the server's writer "+
			"adapts the copy to the target platform (pass an optional instruction to steer it). "+
			"content is an explicit override only: provide it to set the clone's body yourself; omit it "+
			"for a verbatim same-platform copy. Returns the new draft's id.",
		func(ctx *ai.ToolContext, in ClonePostInput) (*ClonePostOutput, error) {
			return toolClonePost(ctx, in)
		},
	)

	restoreVersion := genkit.DefineTool(g, "restoreVersion",
		"Rolls the current post back to an earlier saved version. Pass versionNumber "+
			"(e.g. 2) for an explicit version, or relative:\"previous\" to undo the last change. "+
			"Non-destructive: it saves a new version, so nothing is lost. Returns the restored "+
			"version number and the new HEAD version number.",
		func(ctx *ai.ToolContext, in RestoreVersionInput) (*RestoreVersionOutput, error) {
			return toolRestoreVersion(ctx, in)
		},
	)

	schedulePost := genkit.DefineTool(g, "schedulePost",
		"Schedules the current post for publishing at an absolute time. Call ONLY after the "+
			"user has confirmed the resolved time and the auto/manual mode. Pass scheduledAt as "+
			"ISO-8601 with a timezone offset; set allowPromote:true when the post is a draft. "+
			"Returns the routed status (scheduled vs scheduled_for_manual_publishing) and whether "+
			"the draft was promoted.",
		func(ctx *ai.ToolContext, in SchedulePostInput) (*SchedulePostOutput, error) {
			return toolSchedulePost(ctx, in)
		},
	)

	createNote := genkit.DefineTool(g, "createNote",
		"Saves a standalone note attached to the current post — an image-generation prompt "+
			"(type image_prompt, e.g. a Nano Banana prompt) or a free-form side note/idea "+
			"(type note). Use this instead of putting such artifacts in the post body. You can "+
			"call it alongside an edit to both change the body AND capture a note in the same turn. "+
			"Returns the new note's id.",
		func(ctx *ai.ToolContext, in CreateNoteInput) (*CreateNoteOutput, error) {
			return toolCreateNote(ctx, in)
		},
	)

	editPost := genkit.DefineTool(g, "editPost",
		"Generates or revises the current post's content — this is how ALL content changes happen. "+
			"Pass the user's change request verbatim in instruction (e.g. 'shorten the intro', "+
			"'make it punchier', 'write the post from the draft thesis') and a mode (edit or draft). "+
			"The content is produced and applied by the server; you do not write it yourself.",
		func(ctx *ai.ToolContext, in EditPostInput) (*EditPostOutput, error) {
			return toolEditPost(ctx, in)
		},
	)

	return &toolSet{
		listAssets:        list,
		getAssetChunks:    getChunks,
		searchAssetChunks: searchChunks,
		getCurrentContent: getCurrentContent,
		clonePost:         clonePost,
		restoreVersion:    restoreVersion,
		schedulePost:      schedulePost,
		createNote:        createNote,
		editPost:          editPost,
	}
}

// ── Tool implementations ─────────────────────────────────────────────────────

func toolListAssets(ctx context.Context) ([]AssetInfo, error) {
	st := getRequestState(ctx)
	result := make([]AssetInfo, 0, len(st.assetIDs))
	for _, id := range st.assetIDs {
		asset, err := st.repos.Assets.GetByID(ctx, id)
		if err != nil {
			continue
		}
		chunks, err := st.repos.Chunks.GetByAssetID(ctx, id)
		if err != nil {
			continue
		}
		assetType := ""
		if asset.Type != nil {
			assetType = *asset.Type
		}
		result = append(result, AssetInfo{
			ID:         asset.ID,
			Name:       asset.Title,
			Type:       assetType,
			ChunkCount: len(chunks),
		})
	}
	return result, nil
}

func toolGetAssetChunks(ctx context.Context, in GetChunksInput) (*ChunksOutput, error) {
	st := getRequestState(ctx)

	var chunks []models.AssetChunk
	var err error
	if len(in.ChunkIDs) > 0 {
		chunks, err = st.repos.Chunks.GetByIDs(ctx, in.ChunkIDs)
	} else {
		chunks, err = st.repos.Chunks.GetByAssetID(ctx, in.AssetID)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch chunks: %w", err)
	}

	out := packChunks(chunks)
	st.captureExcerpts(in.AssetID, out)
	return out, nil
}

func toolSearchAssetChunks(ctx context.Context, in SearchChunksInput) (*ChunksOutput, error) {
	st := getRequestState(ctx)
	if !embedopts.Available(st.embedder) {
		return nil, fmt.Errorf("semantic search is unavailable (no embedder configured); use getAssetChunks to retrieve chunks by ID instead")
	}

	qResp, err := st.embedder.Embed(ctx, &ai.EmbedRequest{
		Input:   []*ai.Document{ai.DocumentFromText(in.Query, nil)},
		Options: embedopts.Query(),
	})
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(qResp.Embeddings) == 0 {
		return &ChunksOutput{}, nil
	}

	// pgvector ANN search scoped to this asset, ordered closest-first and
	// filtered at the similarity floor — in-database via the HNSW index
	// (replaces the fetch-all-and-cosine-in-Go scan).
	ranked, err := st.repos.Chunks.SearchSimilar(ctx, pgvector.NewHalfVector(qResp.Embeddings[0].Embedding), []string{in.AssetID}, minSearchSimilarity, 0)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}

	out := packChunks(ranked)
	st.captureExcerpts(in.AssetID, out)
	return out, nil
}

func toolGetCurrentContent(ctx context.Context) (string, error) {
	st := getRequestState(ctx)
	post, err := st.repos.Posts.GetByID(ctx, st.postID)
	if err != nil {
		return "", fmt.Errorf("fetch post: %w", err)
	}
	return post.Content, nil
}

func toolClonePost(ctx context.Context, in ClonePostInput) (*ClonePostOutput, error) {
	st := getRequestState(ctx)
	if st.cloneSvc == nil {
		return nil, fmt.Errorf("cloning is not available")
	}

	opts := clone.DefaultOptions(st.actor, clone.TriggerAssistant)
	if in.TargetPlatform != "" {
		id, err := resolvePlatform(st.platforms, in.TargetPlatform)
		if err != nil {
			return nil, err
		}
		opts.TargetPlatformID = id
	}
	opts.TargetPostType = in.TargetPostType

	// CON-128: in the hybrid path the planner does not write copy, so a
	// cross-platform clone's adapted content is produced by the Sonnet writer
	// here rather than passed in. An explicit Content override still wins
	// (legacy path / deliberate override); a verbatim same-platform clone (no
	// targetPlatform) skips the writer entirely. writerSystem is empty in the
	// legacy path, so this branch is inert there.
	content := in.Content
	if content == "" && in.TargetPlatform != "" && st.writerSystem != "" {
		instr := fmt.Sprintf("Adapt the current post for the %s platform, following its conventions (length, tone, formatting, links). Output the full adapted post.", in.TargetPlatform)
		if s := strings.TrimSpace(in.Instruction); s != "" {
			instr += " " + s
		}
		adapted, err := runWriter(ctx, st, instr, false)
		if err != nil {
			return nil, fmt.Errorf("adapt content for %s: %w", in.TargetPlatform, err)
		}
		if adapted == "" {
			// The writer returned no content (not an error) — fail rather than
			// silently clone the source verbatim onto the target platform.
			return nil, fmt.Errorf("adapt content for %s: the writer produced no content", in.TargetPlatform)
		}
		content = adapted
	}
	if content != "" {
		opts.ContentOverride = &content
	}
	if in.Title != "" {
		opts.TitleOverride = &in.Title
	}

	emit(st.onEvent, SSEEventCloneStarted, CloneStartedEventPayload{TargetPlatform: in.TargetPlatform})

	res, err := st.cloneSvc.Clone(ctx, st.postID, opts)
	if err != nil {
		return nil, err
	}
	st.cloneResult = res

	return &ClonePostOutput{
		NewPostID:        res.Post.ID,
		PlatformID:       res.Post.PlatformID,
		PostType:         res.ResolvedPostType,
		Adapted:          res.Adapted,
		PostTypeFellBack: res.PostTypeFellBack,
	}, nil
}

func toolRestoreVersion(ctx context.Context, in RestoreVersionInput) (*RestoreVersionOutput, error) {
	st := getRequestState(ctx)
	if st.restoreSvc == nil {
		return nil, fmt.Errorf("restore is not available")
	}

	versionNumber := in.VersionNumber
	if versionNumber <= 0 && in.Relative != "" {
		resolved, err := resolvePreviousVersion(ctx, st, in.Relative)
		if err != nil {
			return nil, err
		}
		versionNumber = resolved
	}
	if versionNumber <= 0 {
		return nil, fmt.Errorf("specify which version to restore: a version number (e.g. 2) or relative:\"previous\"")
	}

	emit(st.onEvent, SSEEventRestoreStarted, RestoreStartedEventPayload{TargetVersion: versionNumber})

	res, err := st.restoreSvc.Restore(ctx, st.postID, restore.Options{
		Actor:         st.actor,
		Trigger:       restore.TriggerAssistant,
		VersionNumber: versionNumber,
	})
	if err != nil {
		return nil, err
	}
	st.restoreResult = res

	return &RestoreVersionOutput{
		RestoredFromVersion: res.RestoredFromVersion,
		NewVersionNumber:    res.NewVersionNumber,
		NoOp:                res.NoOp,
	}, nil
}

func toolSchedulePost(ctx context.Context, in SchedulePostInput) (*SchedulePostOutput, error) {
	st := getRequestState(ctx)
	if st.scheduleSvc == nil {
		return nil, fmt.Errorf("scheduling is not available")
	}

	when, err := parseScheduledAt(in.ScheduledAt)
	if err != nil {
		return nil, err
	}

	emit(st.onEvent, SSEEventScheduleStarted, ScheduleStartedEventPayload{ScheduledAt: when.UTC().Format(time.RFC3339)})

	res, err := st.scheduleSvc.Schedule(ctx, st.postID, schedule.Options{
		ScheduledAt:  when,
		AllowPromote: in.AllowPromote,
		Actor:        st.actor,
		Trigger:      schedule.TriggerAssistant,
	})
	if err != nil {
		return nil, scheduleToolError(err)
	}
	st.recordScheduled(res)

	return &SchedulePostOutput{
		ScheduledAt: res.ScheduledAt.Format(time.RFC3339),
		Status:      string(res.Status),
		AutoPublish: res.AutoPublish,
		Promoted:    res.Promoted,
	}, nil
}

func toolCreateNote(ctx context.Context, in CreateNoteInput) (*CreateNoteOutput, error) {
	st := getRequestState(ctx)
	if st.noteSvc == nil {
		return nil, fmt.Errorf("notes are not available")
	}

	// draft_thesis is reserved for the content-plan flow; the assistant may
	// only create image_prompt or note. Anything else (incl. empty) defaults
	// to a free-form note.
	nt := models.PostNoteType(in.Type)
	if nt != models.PostNoteTypeImagePrompt && nt != models.PostNoteTypeNote {
		nt = models.PostNoteTypeNote
	}

	note, err := st.noteSvc.Create(ctx, notes.CreateInput{
		PostID:    st.postID,
		Type:      nt,
		Title:     in.Title,
		Body:      in.Body,
		Origin:    models.PostNoteOriginAssistant,
		CreatedBy: st.actor,
	})
	if err != nil {
		return nil, err
	}
	st.noteResults = append(st.noteResults, note)
	// Notes aren't part of the context-cache fingerprint, so bust the cached
	// block for this post — the next turn must see the note just created.
	invalidateContextCache(st.postID)

	emit(st.onEvent, SSEEventNoteCreated, NoteCreatedEventPayload{
		ID:    note.ID,
		Type:  string(note.Type),
		Title: note.Title,
		Body:  note.Body,
	})

	return &CreateNoteOutput{ID: note.ID, Type: string(note.Type)}, nil
}

// toolEditPost is the editPost write-tool (CON-128). In the hybrid path the
// planner (Haiku) calls it for any content change, forwarding the user's
// instruction verbatim; the Sonnet writer produces the full post, which
// streams to the client and is stashed on requestState for the runner. Only a
// compact receipt goes back to the planner.
func toolEditPost(ctx context.Context, in EditPostInput) (*EditPostOutput, error) {
	st := getRequestState(ctx)
	// CON-251: a submitted post (scheduled or published) holds a copy outside
	// Ogen, so its content is locked — the assistant can read and advise but
	// must not write. Refuse via the return value, not a Go error, so the
	// planner loop continues and relays the reason (a tool error would abort
	// Generate). editResult stays nil, so the runner finalises this as a
	// non-edit turn.
	if st.postStatus.IsSubmitted() {
		return &EditPostOutput{
			OK:     false,
			Reason: "This post is " + string(st.postStatus) + "; a copy already lives outside Ogen, so its content is locked and can't be edited. Unschedule it first (or duplicate it into a new draft) to make changes.",
		}, nil
	}
	instruction := strings.TrimSpace(in.Instruction)
	if instruction == "" {
		return nil, fmt.Errorf("instruction is required to edit the post")
	}

	content, err := runWriter(ctx, st, instruction, true)
	if err != nil {
		return nil, fmt.Errorf("write content: %w", err)
	}
	if content == "" {
		return nil, fmt.Errorf("the writer produced no content")
	}
	st.editResult = &editResult{Content: content}
	return &EditPostOutput{OK: true, Chars: len(content)}, nil
}

// runWriter executes the Sonnet copywriting sub-call (CON-128). When stream is
// true it fans the generated Markdown out to the client as content_delta events
// (the editPost path, feeding the live editor); for clone adaptation it stays
// silent (the copy lands in a new draft, not the open editor). It returns the
// full generated post content. Usage is metered under the post_assistant_edit
// flow so the writer's Sonnet spend is attributable separately from the cheap
// planner loop.
func runWriter(ctx context.Context, st *requestState, instruction string, stream bool) (string, error) {
	if st.g == nil || st.provider == nil || st.writerSystem == "" {
		return "", fmt.Errorf("content writing is not available")
	}

	var buf strings.Builder
	streamCb := func(_ context.Context, chunk *ai.ModelResponseChunk) error {
		if chunk == nil || chunk.Aggregated {
			return nil
		}
		for _, part := range chunk.Content {
			if part.IsText() {
				buf.WriteString(part.Text)
				if stream {
					emit(st.onEvent, SSEEventContentDelta, DeltaEventPayload{Delta: part.Text})
				}
			}
		}
		return nil
	}

	maxTokens := st.writerMaxTokens
	if maxTokens == 0 {
		maxTokens = 64000
	}

	resp, err := genkit.Generate(ctx, st.g,
		ai.WithModelName(modelconfig.Ref(ctx, modelconfig.FlowPostAssistant, modelconfig.SlotWriter)),
		ai.WithSystem(st.writerSystem),
		ai.WithPrompt(composeWriterInstruction(instruction, st.retrieved)),
		ai.WithStreaming(streamCb),
		st.provider.CallConfig(maxTokens),
	)
	if err != nil {
		return "", err
	}
	if st.recorder != nil {
		st.recorder.RecordResp(ctx, modelconfig.Vendor(ctx, modelconfig.FlowPostAssistant, modelconfig.SlotWriter), modelconfig.Model(ctx, modelconfig.FlowPostAssistant, modelconfig.SlotWriter), "post_assistant_edit", resp)
	}

	content := strings.TrimSpace(buf.String())
	if content == "" {
		content = strings.TrimSpace(resp.Text())
	}
	return content, nil
}

// composeWriterInstruction assembles the writer's prompt: the user's verbatim
// instruction, then any asset excerpts the planner retrieved this turn as
// clearly-delimited source material (CON-128). Appending the excerpts rather
// than folding them into the instruction keeps the instruction unchanged while
// giving an asset-grounded edit the full retrieved text — not just the short
// preview in the context block. Returns the instruction untouched when nothing
// was retrieved.
func composeWriterInstruction(instruction string, excerpts []retrievedExcerpt) string {
	if len(excerpts) == 0 {
		return instruction
	}
	var b strings.Builder
	b.WriteString(instruction)
	b.WriteString("\n\n## Source material (retrieved from the attached assets)\n")
	b.WriteString("Use these retrieved excerpts as the source of truth for any asset-based facts; prefer them over the shorter previews in the context above.\n")
	for _, e := range excerpts {
		b.WriteString("\n---\n")
		if e.AssetID != "" {
			b.WriteString("Asset " + e.AssetID)
			if e.ChunkID != "" {
				b.WriteString(", chunk " + e.ChunkID)
			}
			b.WriteString(":\n")
		}
		b.WriteString(e.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// parseScheduledAt accepts the ISO-8601 instant the model resolved. It
// requires an explicit timezone (offset or Z) so the stored UTC instant
// is unambiguous; a naive local datetime is rejected with guidance the
// model relays to the user.
func parseScheduledAt(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("scheduledAt is required (ISO-8601 with a timezone offset, e.g. 2026-06-15T09:00:00+03:00)")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("could not parse scheduledAt %q; use ISO-8601 with a timezone offset, e.g. 2026-06-15T09:00:00+03:00 or 2026-06-15T06:00:00Z", s)
}

// scheduleToolError rewrites the shared service's errors into messages
// the model can relay to the user. The CON-74 validation failure is
// expanded into the concrete reasons so the assistant can tell the user
// exactly what's missing instead of a generic "not ready".
func scheduleToolError(err error) error {
	var verr *schedule.ValidationError
	switch {
	case errors.As(err, &verr):
		var reasons []string
		for _, errs := range verr.Errors {
			for _, e := range errs {
				reasons = append(reasons, e.Message)
			}
		}
		if len(reasons) == 0 {
			return fmt.Errorf("the post is not ready to publish yet")
		}
		return fmt.Errorf("the post can't be scheduled until these are fixed: %s", strings.Join(reasons, "; "))
	case errors.Is(err, schedule.ErrScheduledAtInPast):
		return fmt.Errorf("that time is in the past — propose the next valid time and confirm again")
	case errors.Is(err, schedule.ErrNoPlatform):
		return fmt.Errorf("this post has no platform set, so it can't be scheduled — ask the user to choose a platform first")
	case errors.Is(err, schedule.ErrNotSchedulable):
		return fmt.Errorf("%v — only a ready-for-publish post (or a draft you promote) can be scheduled; if it's already scheduled, it must be cancelled first", err)
	case errors.Is(err, schedule.ErrPostNotFound):
		return fmt.Errorf("post not found")
	}
	return err
}

// resolvePreviousVersion maps a relative reference ("previous", "undo",
// "back", "last") to a concrete version number: the version representing
// the state immediately before the current HEAD. Returns an error (which
// the model relays to the user) when there is nothing earlier to restore.
func resolvePreviousVersion(ctx context.Context, st *requestState, relative string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(relative)) {
	case "previous", "prev", "undo", "back", "last", "earlier":
	default:
		return 0, fmt.Errorf("unrecognized version reference %q; pass a version number instead", relative)
	}
	latest, err := st.repos.Versions.GetLatestByPostID(ctx, st.postID)
	if err != nil {
		return 0, fmt.Errorf("look up versions: %w", err)
	}
	if latest == nil {
		return 0, fmt.Errorf("there are no saved versions to restore")
	}
	post, err := st.repos.Posts.GetByID(ctx, st.postID)
	if err != nil {
		return 0, fmt.Errorf("load post: %w", err)
	}
	// "previous" = the state before the current HEAD. When the latest
	// snapshot already equals the live content, step back one; otherwise
	// the live content is ahead of every snapshot, so the latest snapshot
	// itself is the previous state.
	target := latest.VersionNumber
	if post.Content == latest.Content {
		target = latest.VersionNumber - 1
	}
	if target < 1 {
		return 0, fmt.Errorf("there is no earlier version to restore")
	}
	return target, nil
}

// resolvePlatform maps a platform identifier from the model (an exact ID
// or a case-insensitive name) to a platform ID. Returns an error listing
// the available platforms when nothing matches, so the model can ask the
// user to clarify rather than guess.
func resolvePlatform(platforms []models.Platform, ident string) (string, error) {
	for _, p := range platforms {
		if p.ID == ident {
			return p.ID, nil
		}
	}
	for _, p := range platforms {
		if strings.EqualFold(p.Name, ident) {
			return p.ID, nil
		}
	}
	names := make([]string, 0, len(platforms))
	for _, p := range platforms {
		names = append(names, p.Name)
	}
	return "", fmt.Errorf("unknown platform %q; available platforms: %s", ident, strings.Join(names, ", "))
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func packChunks(chunks []models.AssetChunk) *ChunksOutput {
	var out ChunksOutput
	tokens := 0
	for _, c := range chunks {
		if tokens+c.TokenCount > chunkTokenBudget && len(out.Chunks) > 0 {
			out.Truncated = true
			break
		}
		out.Chunks = append(out.Chunks, ChunkContent{
			ID:      c.ID,
			Index:   c.ChunkIndex,
			Content: c.Content,
		})
		tokens += c.TokenCount
	}
	return &out
}
