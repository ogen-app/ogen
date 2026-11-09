package post_assistant

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
)

// A schedule offered for one post and confirmed while another is open is
// refused rather than scheduling the open one.
func TestToolSchedulePost_RefusesAnotherPostsOffer(t *testing.T) {
	st := &requestState{postID: "p2", scheduleSvc: &schedule.Service{}}
	ctx := withRequestState(t.Context(), st)
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, err := toolSchedulePost(ctx, SchedulePostInput{ScheduledAt: at, PostID: "p1"})
	if _, ok := errors.AsType[*assistantkit.Refusal](err); !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if st.scheduleResult != nil {
		t.Fatal("nothing must be scheduled")
	}
}

// The restore service's expected rejections reach the model as refusals; an
// infrastructure failure still aborts the turn.
func TestRestoreToolError(t *testing.T) {
	for _, err := range []error{restore.ErrVersionNotFound, fmt.Errorf("%w: scheduled", restore.ErrNotEditable), restore.ErrPostNotFound} {
		if _, ok := errors.AsType[*assistantkit.Refusal](restoreToolError(err, 7)); !ok {
			t.Fatalf("%v is not a refusal", err)
		}
	}
	boom := errors.New("db down")
	if got := restoreToolError(boom, 7); got != boom {
		t.Fatalf("infrastructure error = %v", got)
	}
}

// takenVersions fails every insert with err, as when another writer took
// version 1 first.
type takenVersions struct {
	repository.PostVersionRepository
	err error
}

func (takenVersions) CountByPostID(context.Context, string) (int, error) { return 0, nil }

func (v takenVersions) Create(context.Context, *models.PostVersion) error { return v.err }

// Two turns starting on a post with no versions both try to save version 1;
// the one that loses goes on, since the post has its first version.
func TestEnsureInitialVersion_TakenByAnotherWriter(t *testing.T) {
	post := &models.Post{ID: "p1", Content: "The user's copy."}
	taken := &pgconn.PgError{Code: "23505", ConstraintName: "idx_post_versions_post_version"}
	if err := ensureInitialVersion(t.Context(), PostAssistantRepos{Versions: takenVersions{err: taken}}, post); err != nil {
		t.Fatalf("taken version 1: %v", err)
	}
	if err := ensureInitialVersion(t.Context(), PostAssistantRepos{Versions: takenVersions{err: errors.New("db down")}}, post); err == nil {
		t.Fatal("a failed insert must still fail")
	}
}

// sentNotifications records each notification and the tenant it was for.
type sentNotifications struct {
	repository.NotificationRepository
	rows    []*models.Notification
	tenants []string
}

func (r *sentNotifications) Insert(ctx context.Context, n *models.Notification) (bool, error) {
	tenant, _ := tenantctx.From(ctx)
	r.rows, r.tenants = append(r.rows, n), append(r.tenants, tenant)
	return true, nil
}

// A turn that changed the post leaves its owner a notification in the post's
// workspace; one that changed nothing leaves none.
func TestEnd_NotifiesTheOwner(t *testing.T) {
	sent := &sentNotifications{}
	st := &requestState{postID: "p1", owner: "u1", tenant: "t1", notifier: notify.New(sent, nil)}
	if _, err := end(withRequestState(t.Context(), st)); err != nil || len(sent.rows) != 0 {
		t.Fatalf("nothing done: err = %v, sent = %d", err, len(sent.rows))
	}
	st.noteResults = []*models.PostNote{{ID: "n1", Type: models.PostNoteTypeNote}}
	if _, err := end(withRequestState(t.Context(), st)); err != nil {
		t.Fatal(err)
	}
	if len(sent.rows) != 1 || sent.rows[0].UserID != "u1" || sent.rows[0].Type != "assistant.completed" ||
		sent.rows[0].DedupeKey != "assistant.completed:p1" || sent.tenants[0] != "t1" {
		t.Fatalf("sent = %+v, tenants = %v", sent.rows, sent.tenants)
	}
}

// failingVersions fails every version insert.
type failingVersions struct {
	repository.PostVersionRepository
}

func (failingVersions) CreateNext(context.Context, *models.PostVersion) error {
	return errors.New("db down")
}

// The post is written before its version, so a failed version leaves the
// edit standing, unversioned, rather than a version of content the post
// never had.
func TestCommitEdit_FailedVersionLeavesTheEditUnversioned(t *testing.T) {
	posts := &savedPosts{post: &models.Post{ID: "p1", Status: models.PostStatusDraft, Content: "old"}}
	st := &requestState{postID: "p1", repos: PostAssistantRepos{Posts: posts, Versions: failingVersions{}}}
	saved, err := commitEdit(t.Context(), st, "new", true, "Rewrite")
	if err != nil || saved {
		t.Fatalf("saved = %v, err = %v", saved, err)
	}
	if posts.post.Content != "new" || st.content != "new" {
		t.Fatalf("post = %q, content = %q", posts.post.Content, st.content)
	}
}
