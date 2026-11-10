package packs

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func historySpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	if d.Posts == nil || d.Versions == nil || d.PostLogs == nil {
		return nil
	}
	tool := genkit.DefineTool(g, "postHistory",
		"Returns a post's change history, newest first: each saved version and who saved it, and what was done to it — status changes, scheduling, unscheduling, retries, copies, restores — by whom and when.",
		assistantkit.Wrap(recorded(workspacePackName, "postHistory", nil, func(ctx context.Context, in PostHistoryInput) (*PostHistoryOutput, error) {
			return postHistory(ctx, d, in)
		})), ai.WithStrictSchema(false))
	return []assistantkit.Spec{spec(workspacePackName, tool, assistantkit.TierRead,
		`Call this when the user asks who changed, edited, scheduled or moved a post, or what happened to it — "who changed this post?", "when was this scheduled?", "why is it back in draft?". postId defaults to the post on screen, the one selected, or the thread's.`)}
}

// PostHistoryInput is the post whose history to read.
type PostHistoryInput struct {
	PostID string `json:"postId,omitempty" jsonschema:"description=Defaults to the post on screen, the one selected, or the thread's"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"description=How many entries, newest first; default 20, at most 50"`
}

// HistoryEntry is one change to a post.
type HistoryEntry struct {
	At time.Time `json:"at"`
	// Kind is version for a saved version of the text, event for anything
	// else done to the post.
	Kind    string `json:"kind"`
	What    string `json:"what"`
	Version int    `json:"version,omitempty"`
	// By is who did it, by name; ByID their user id when known.
	By   string `json:"by"`
	ByID string `json:"byId,omitempty"`
	// Source is a version's saver: user, assistant or system.
	Source string `json:"source,omitempty"`
}

// PostHistoryOutput is a post's history.
type PostHistoryOutput struct {
	PostID  string         `json:"postId"`
	Title   string         `json:"title"`
	Entries []HistoryEntry `json:"entries"`
	// More is set when older entries were left out.
	More bool   `json:"more,omitempty"`
	Note string `json:"note,omitempty"`
}

// historyEvents are the log events a person would call a change, with how
// to say each; the rest (polls, task runs) are machinery.
var historyEvents = map[models.PostLogEventType]string{
	models.PostLogEventStateTransition: "status changed",
	models.PostLogEventUserSchedule:    "scheduled",
	models.PostLogEventUserCancel:      "unscheduled",
	models.PostLogEventUserRetry:       "retried publishing",
	models.PostLogEventPostCloned:      "created as a copy of another post",
	models.PostLogEventPostCompiled:    "made from ideas",
	models.PostLogEventPostRestored:    "restored an earlier version",
}

const unrecordedNote = "Versions saved before people were recorded on them only say whether a person, the assistant or the system saved them."

func postHistory(ctx context.Context, d Deps, in PostHistoryInput) (*PostHistoryOutput, error) {
	id := historyPost(ctx, in.PostID)
	if id == "" {
		return nil, assistantkit.Refused("which post? open it or name it")
	}
	post, err := d.Posts.GetByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, assistantkit.Refused("that post doesn't exist")
	}
	if err != nil {
		return nil, err
	}
	versions, err := d.Versions.ListByPostID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read versions: %w", err)
	}
	logs, err := d.PostLogs.ListFiltered(ctx, repository.PostLogFilter{PostID: id, Limit: 500})
	if err != nil {
		return nil, fmt.Errorf("read post log: %w", err)
	}
	names := memberNames(ctx, d)
	out := &PostHistoryOutput{PostID: id, Title: post.Title, Entries: []HistoryEntry{}}
	for _, v := range versions {
		e := HistoryEntry{At: v.CreatedAt, Kind: "version", Version: v.VersionNumber, What: cmp.Or(v.Note, "saved a version"), Source: v.Creator, ByID: v.CreatedBy}
		e.By = versionBy(v, names)
		if v.CreatedBy == "" && v.Creator != models.PostVersionCreatorSystem {
			out.Note = unrecordedNote
		}
		out.Entries = append(out.Entries, e)
	}
	for _, l := range logs {
		what, ok := historyEvents[l.EventType]
		if !ok {
			continue
		}
		if l.EventType == models.PostLogEventStateTransition && l.FromStatus != nil && l.ToStatus != nil {
			what = fmt.Sprintf("status %s → %s", *l.FromStatus, *l.ToStatus)
		}
		e := HistoryEntry{At: l.EventTimestamp, Kind: "event", What: what, By: actorName(l.Actor, names)}
		if l.Actor != models.ActorSystem {
			e.ByID = l.Actor
		}
		out.Entries = append(out.Entries, e)
	}
	slices.SortStableFunc(out.Entries, func(a, b HistoryEntry) int { return b.At.Compare(a.At) })
	if n := historyLimit(in.Limit); len(out.Entries) > n {
		out.Entries, out.More = out.Entries[:n], true
	}
	return out, nil
}

// historyPost is the post asked about: named, on screen, the one selected,
// or the thread's.
func historyPost(ctx context.Context, id string) string {
	if id != "" {
		return id
	}
	if id := assistant.Focused(ctx, viewcontext.KindPost); id != "" {
		return id
	}
	if sel := assistant.Selected(ctx, viewcontext.KindPost); len(sel) == 1 {
		return sel[0]
	}
	if s, ok := assistant.Scope(ctx); ok && s.Kind == models.AssistantScopePost {
		return s.ID
	}
	return ""
}

func historyLimit(n int) int {
	if n <= 0 {
		return 20
	}
	return min(n, 50)
}

// memberNames is the workspace's members' names by id; empty when they
// can't be read, so entries fall back to their role.
func memberNames(ctx context.Context, d Deps) map[string]string {
	names := map[string]string{}
	if d.Users == nil {
		return names
	}
	users, err := d.Users.List(ctx)
	if err != nil {
		return names
	}
	for _, u := range users {
		names[u.ID] = cmp.Or(u.Name, u.Email)
	}
	return names
}

func actorName(id string, names map[string]string) string {
	switch {
	case id == "" || id == models.ActorSystem:
		return "the system"
	case names[id] != "":
		return names[id]
	}
	return "a former member"
}

// versionBy says who saved v: the person, and the assistant or system on
// their behalf.
func versionBy(v models.PostVersion, names map[string]string) string {
	if v.CreatedBy == "" {
		switch v.Creator {
		case models.PostVersionCreatorAssistant:
			return "the assistant"
		case models.PostVersionCreatorSystem:
			return "the system"
		}
		return "a person (not recorded)"
	}
	name := actorName(v.CreatedBy, names)
	switch v.Creator {
	case models.PostVersionCreatorAssistant:
		return "the assistant, for " + name
	case models.PostVersionCreatorSystem:
		return "the system, when " + name + " acted on it"
	}
	return name
}
