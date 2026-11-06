package post_assistant

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// savedPosts serves one post, as it is now, and records each save.
type savedPosts struct {
	repository.PostRepository
	post  *models.Post
	saved []models.Post
}

func (r *savedPosts) GetByID(context.Context, string) (*models.Post, error) {
	if r.post == nil {
		return nil, sql.ErrNoRows
	}
	cp := *r.post
	return &cp, nil
}

func (r *savedPosts) Update(_ context.Context, p *models.Post, _ ...string) error {
	r.saved = append(r.saved, *p)
	return nil
}

type appendedLogs struct {
	repository.PostLogRepository
	entries []*models.PostLog
}

func (r *appendedLogs) Append(_ context.Context, e *models.PostLog) error {
	r.entries = append(r.entries, e)
	return nil
}

// Writing a planned post's body makes it a draft, and the audit log says so;
// any other edit leaves the status and the log alone.
func TestCommitEdit_PlannedBecomesDraft(t *testing.T) {
	ctx := context.WithValue(t.Context(), logging.UserIDKey, "u1")
	cases := []struct {
		status  models.PostStatus
		content string
		want    models.PostStatus
		logged  bool
	}{
		{models.PostStatusPlanned, "The body, written from the theses.", models.PostStatusDraft, true},
		{models.PostStatusPlanned, "  ", models.PostStatusPlanned, false},
		{models.PostStatusDraft, "A rewrite.", models.PostStatusDraft, false},
	}
	for _, tc := range cases {
		posts, logs := &savedPosts{post: &models.Post{ID: "p1", Status: tc.status}}, &appendedLogs{}
		st := &requestState{postID: "p1", postStatus: tc.status, repos: PostAssistantRepos{Posts: posts, Logs: logs}}
		if err := commitEdit(ctx, st, tc.content, false, ""); err != nil {
			t.Fatal(err)
		}
		if got := posts.saved[0].Status; got != tc.want || st.postStatus != tc.want {
			t.Fatalf("%s + %q saved as %s, want %s", tc.status, tc.content, got, tc.want)
		}
		if tc.logged != (len(logs.entries) == 1) {
			t.Fatalf("%s + %q: %d log entries", tc.status, tc.content, len(logs.entries))
		}
		if tc.logged && (logs.entries[0].Actor != "u1" || *logs.entries[0].FromStatus != models.PostStatusPlanned) {
			t.Fatalf("log = %+v", logs.entries[0])
		}
	}
}

// The edit is written over the post as it is now, so what another tool of
// the turn changed survives; a post deleted meanwhile is a refusal.
func TestCommitEdit_RereadsThePost(t *testing.T) {
	posts := &savedPosts{post: &models.Post{ID: "p1", Status: models.PostStatusScheduledForManualPublish, Content: "old"}}
	st := &requestState{postID: "p1", postStatus: models.PostStatusDraft, repos: PostAssistantRepos{Posts: posts}}
	if err := commitEdit(t.Context(), st, "new", false, ""); err != nil {
		t.Fatal(err)
	}
	if got := posts.saved[0]; got.Status != models.PostStatusScheduledForManualPublish || got.Content != "new" {
		t.Fatalf("saved = %+v", got)
	}

	posts.post = nil
	err := commitEdit(t.Context(), st, "newer", false, "")
	if r, ok := err.(*assistantkit.Refusal); !ok || r.Reason != postRemoved {
		t.Fatalf("deleted post: %v", err)
	}
}
