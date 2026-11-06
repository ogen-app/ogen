package post_assistant

import (
	"context"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

type savedPosts struct {
	repository.PostRepository
	saved []models.Post
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
		posts, logs := &savedPosts{}, &appendedLogs{}
		tr := &turn{
			post:   &models.Post{ID: "p1", Status: tc.status},
			repos:  PostAssistantRepos{Posts: posts, Logs: logs},
			result: PostAssistantResponse{Action: actionEdited, UpdatedContent: tc.content},
		}
		if err := tr.commitEdit(ctx); err != nil {
			t.Fatal(err)
		}
		if got := posts.saved[0].Status; got != tc.want {
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
