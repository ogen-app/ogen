package packs

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
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
		`Call this when the user asks who changed, edited, scheduled or moved a post, or what happened to it — "who changed this post?", "when was this scheduled?", "why is it back in draft?". postId defaults to the post on screen, the one selected, or the thread's. since and changes narrow it when the user asks for part of it ("schedule changes since Monday"). When source is assistant, the assistant made the change and by is who asked for it.`)}
}

// PostHistoryInput is the post whose history to read, and how much of it.
type PostHistoryInput struct {
	PostID string `json:"postId,omitempty" jsonschema:"description=Defaults to the post on screen, the one selected, or the thread's"`
	Limit  int    `json:"limit,omitempty"  jsonschema:"description=How many entries, newest first; default 20, at most 50"`
	// Since and Changes narrow the history; the output's scope says so.
	Since   string   `json:"since,omitempty"   jsonschema:"description=Only changes on or after this day, YYYY-MM-DD in the workspace's timezone"`
	Changes []string `json:"changes,omitempty" jsonschema:"description=Only these kinds of change: version, scheduled, rescheduled, unscheduled, status, failed, retried, copied, compiled, restored"`
}

// The kinds of change an entry is.
const (
	changeVersion     = "version"
	changeScheduled   = "scheduled"
	changeRescheduled = "rescheduled"
	changeUnscheduled = "unscheduled"
	changeStatus      = "status"
	changeFailed      = "failed"
	changeRetried     = "retried"
	changeCopied      = "copied"
	changeCompiled    = "compiled"
	changeRestored    = "restored"
)

var historyChanges = []string{changeVersion, changeScheduled, changeRescheduled, changeUnscheduled, changeStatus,
	changeFailed, changeRetried, changeCopied, changeCompiled, changeRestored}

// HistoryEntry is one change to a post.
type HistoryEntry struct {
	At time.Time `json:"at"`
	// Kind is version for a saved version of the text, event for anything
	// else done to the post; Change is which change it was.
	Kind    string `json:"kind"`
	Change  string `json:"change"`
	What    string `json:"what"`
	Version int    `json:"version,omitempty"`
	// By is who did it, by name; ByID their user id when known.
	By   string `json:"by"`
	ByID string `json:"byId,omitempty"`
	// Source is who made the change: user, assistant or system. For the
	// assistant, By is who asked it.
	Source string `json:"source,omitempty"`
	// From and To are a schedule's times: the one it had and the one it
	// got.
	From *time.Time `json:"from,omitempty"`
	To   *time.Time `json:"to,omitempty"`
	// Reason is why a post failed to go out, on its latest failure.
	Reason string `json:"reason,omitempty"`
}

// HistoryScope is how a history was narrowed; empty fields weren't.
type HistoryScope struct {
	Since   string   `json:"since,omitempty"`
	Changes []string `json:"changes,omitempty"`
}

// PostHistoryOutput is a post's history.
type PostHistoryOutput struct {
	PostID  string         `json:"postId"`
	Title   string         `json:"title"`
	Entries []HistoryEntry `json:"entries"`
	// Timezone is the workspace's, for showing the times.
	Timezone string        `json:"timezone"`
	Scope    *HistoryScope `json:"scope,omitempty"`
	// More is set when older entries were left out.
	More bool `json:"more,omitempty"`
	// UnrecordedBefore is when versions started saying who saved them:
	// earlier ones say only whether a person, the assistant or the system
	// did.
	UnrecordedBefore *time.Time         `json:"unrecordedBefore,omitempty"`
	Note             string             `json:"note,omitempty"`
	Step             *assistantkit.Step `json:"step,omitempty"`
}

// historyEvents are the log events a person would call a change, with the
// change each is and how to say it; the rest (polls, task runs) are
// machinery.
var historyEvents = map[models.PostLogEventType]struct{ change, what string }{
	models.PostLogEventStateTransition: {changeStatus, "status changed"},
	models.PostLogEventUserSchedule:    {changeScheduled, "scheduled"},
	models.PostLogEventUserCancel:      {changeUnscheduled, "unscheduled"},
	models.PostLogEventUserRetry:       {changeRetried, "retried publishing"},
	models.PostLogEventPostCloned:      {changeCopied, "created as a copy of another post"},
	models.PostLogEventPostCompiled:    {changeCompiled, "made from ideas"},
	models.PostLogEventPostRestored:    {changeRestored, "restored an earlier version"},
}

// historyLogWindow is how many of a post's newest log entries a history
// reads.
const historyLogWindow = 500

const unrecordedNote = "Versions saved before people were recorded on them only say whether a person, the assistant or the system saved them."

// logPayload is what a log entry's payload says that the history reads.
type logPayload struct {
	ScheduledAt string `json:"scheduled_at"`
	Trigger     string `json:"trigger"`
}

func postHistory(ctx context.Context, d Deps, in PostHistoryInput) (*PostHistoryOutput, error) {
	id := historyPost(ctx, in.PostID)
	if id == "" {
		return nil, assistantkit.Refused("which post? open it or name it")
	}
	tz, loc := workspaceZone(ctx, d)
	scope, since, err := historyScope(in, loc)
	if err != nil {
		return nil, err
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
	filter := repository.PostLogFilter{PostID: id, Limit: historyLogWindow}
	if since != nil {
		filter.Since = *since
	}
	logs, err := d.PostLogs.ListFiltered(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("read post log: %w", err)
	}
	// A full window may have cut older changes off, so nothing older than
	// its oldest entry is shown as if the history were complete.
	var cut *time.Time
	if len(logs) >= historyLogWindow {
		cut = new(slices.MinFunc(logs, func(a, b models.PostLog) int { return a.EventTimestamp.Compare(b.EventTimestamp) }).EventTimestamp)
	}
	names := memberNames(ctx, d)
	out := &PostHistoryOutput{PostID: id, Title: post.Title, Entries: []HistoryEntry{}, Timezone: tz, Scope: scope}
	for _, v := range versions {
		e := HistoryEntry{At: v.CreatedAt, Kind: "version", Change: changeVersion, Version: v.VersionNumber, What: cmp.Or(v.Note, "saved a version"), Source: v.Creator, ByID: v.CreatedBy}
		e.By = versionBy(v, names)
		if v.CreatedBy == "" && v.Creator != models.PostVersionCreatorSystem {
			out.Note = unrecordedNote
			if out.UnrecordedBefore == nil || v.CreatedAt.After(*out.UnrecordedBefore) {
				out.UnrecordedBefore = new(v.CreatedAt)
			}
		}
		out.Entries = append(out.Entries, e)
	}
	out.Entries = append(out.Entries, logEntries(logs, names, post.FailureReason)...)
	slices.SortStableFunc(out.Entries, func(a, b HistoryEntry) int { return b.At.Compare(a.At) })
	out.Entries = slices.DeleteFunc(out.Entries, func(e HistoryEntry) bool {
		return (since != nil && e.At.Before(*since)) || (cut != nil && e.At.Before(*cut)) || (len(in.Changes) > 0 && !slices.Contains(in.Changes, e.Change))
	})
	out.More = cut != nil
	if n := historyLimit(in.Limit); len(out.Entries) > n {
		out.Entries, out.More = out.Entries[:n], true
	}
	out.Step = &assistantkit.Step{Subject: post.Title}
	return out, nil
}

// historyScope checks how in narrows the history: since as the start of
// that day in loc.
func historyScope(in PostHistoryInput, loc *time.Location) (*HistoryScope, *time.Time, error) {
	for _, c := range in.Changes {
		if !slices.Contains(historyChanges, c) {
			return nil, nil, assistantkit.Refused(fmt.Sprintf("%q isn't a kind of change", c), historyChanges...)
		}
	}
	var since *time.Time
	if in.Since != "" {
		day, err := time.ParseInLocation(time.DateOnly, in.Since, loc)
		if err != nil {
			return nil, nil, assistantkit.Refused("since is a YYYY-MM-DD")
		}
		since = &day
	}
	if since == nil && len(in.Changes) == 0 {
		return nil, nil, nil
	}
	return &HistoryScope{Since: in.Since, Changes: in.Changes}, since, nil
}

// logEntries are the log's changes: who asked, a schedule's from and to,
// and why the latest failure failed.
func logEntries(logs []models.PostLog, names map[string]string, failureReason string) []HistoryEntry {
	logs = slices.Clone(logs)
	slices.SortStableFunc(logs, func(a, b models.PostLog) int { return a.EventTimestamp.Compare(b.EventTimestamp) })
	var entries []HistoryEntry
	var scheduled *time.Time
	lastFailed := -1
	for _, l := range logs {
		ev, ok := historyEvents[l.EventType]
		if !ok {
			continue
		}
		var p logPayload
		_ = json.Unmarshal([]byte(l.Payload), &p)
		e := HistoryEntry{At: l.EventTimestamp, Kind: "event", Change: ev.change, What: ev.what, By: actorName(l.Actor, names)}
		if l.Actor != models.ActorSystem {
			e.ByID = l.Actor
		}
		if p.Trigger == "assistant" {
			e.Source = string(models.PostVersionCreatorAssistant)
		}
		switch l.EventType {
		case models.PostLogEventStateTransition:
			if l.FromStatus != nil && l.ToStatus != nil {
				e.What = fmt.Sprintf("status %s → %s", *l.FromStatus, *l.ToStatus)
			}
			if l.ToStatus != nil && *l.ToStatus == models.PostStatusFailed {
				e.Change, e.What = changeFailed, "didn't go out"
				lastFailed = len(entries)
			}
			// Failing, going out or going back to draft ends the schedule,
			// so the next one is new, not a move.
			if l.ToStatus != nil && (*l.ToStatus == models.PostStatusFailed || *l.ToStatus == models.PostStatusPublished || *l.ToStatus == models.PostStatusDraft) {
				scheduled = nil
			}
		case models.PostLogEventUserSchedule:
			at, err := time.Parse(time.RFC3339, p.ScheduledAt)
			if err == nil {
				e.To = &at
				if scheduled != nil {
					e.Change, e.What, e.From = changeRescheduled, "rescheduled", scheduled
				}
				scheduled = &at
			}
		case models.PostLogEventUserCancel:
			scheduled = nil
		}
		entries = append(entries, e)
	}
	if lastFailed >= 0 {
		entries[lastFailed].Reason = failureReason
	}
	return entries
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
