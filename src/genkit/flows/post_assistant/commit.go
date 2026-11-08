package post_assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// isPostRemovedFKViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503) on a post_id constraint. Every post-referencing
// write of the pack goes through a *_post_id_fkey constraint, so a concurrent
// post deletion trips exactly these.
func isPostRemovedFKViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok &&
		pgErr.Code == "23503" &&
		strings.HasSuffix(pgErr.ConstraintName, "_post_id_fkey")
}

// writeErr maps a failed post-referencing write: a post deleted mid-turn is a
// refusal the model relays; anything else is wrapped with op.
func writeErr(err error, op string) error {
	if errors.Is(err, sql.ErrNoRows) || isPostRemovedFKViolation(err) {
		return assistantkit.Refused(postRemoved)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// ensureInitialVersion snapshots the user's content as version 1 before the
// assistant's first change, so it can always be restored.
func ensureInitialVersion(ctx context.Context, repos PostAssistantRepos, post *models.Post) error {
	count, err := repos.Versions.CountByPostID(ctx, post.ID)
	if err != nil {
		return fmt.Errorf("count versions: %w", err)
	}
	if count != 0 || post.Content == "" {
		return nil
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	if err := repos.Versions.Create(ctx, &models.PostVersion{
		ID:            id,
		PostID:        post.ID,
		VersionNumber: 1,
		Content:       post.Content,
		Note:          "Initial version",
		Creator:       models.PostVersionCreatorUser,
	}); err != nil {
		return writeErr(err, "create initial version")
	}
	return nil
}

// commitEdit writes the writer's content to the post, after saving it as a
// version when asked. The post is read again first, so what another tool of
// the turn changed (a schedule, a restore) isn't written back over. Writing
// a planned post's body makes it a draft. Content is stored as Markdown.
func commitEdit(ctx context.Context, st *requestState, content string, saveVersion bool, note string) error {
	if saveVersion {
		if err := createVersion(ctx, st, content, note); err != nil {
			return err
		}
	}
	post, err := st.repos.Posts.GetByID(ctx, st.postID)
	if err != nil {
		return writeErr(err, "load post")
	}
	prev := post.Status
	post.Content = content
	post.Status = prev.WithBody(content)
	post.UpdatedAt = time.Now().UTC()
	if err := st.repos.Posts.Update(ctx, post); err != nil {
		return writeErr(err, "update post")
	}
	if post.Status != prev {
		logTransition(ctx, st, post.ID, prev, post.Status)
	}
	st.postStatus = post.Status
	if saveVersion {
		st.qualityAuto.Schedule(ctx, post, models.QualityTriggerVersion)
	}
	invalidateContextCache(st.postID)
	assistantkit.Touch(ctx, "post", st.postID)
	return nil
}

// logTransition records a status change the edit made in the post's audit
// log. Best-effort: the edit has committed.
func logTransition(ctx context.Context, st *requestState, postID string, from, to models.PostStatus) {
	if st.repos.Logs == nil {
		return
	}
	id, err := models.NewID()
	if err != nil {
		return
	}
	actor, _ := logging.UserIDFrom(ctx)
	if err := st.repos.Logs.Append(ctx, &models.PostLog{
		ID:         id,
		PostID:     postID,
		EventType:  models.PostLogEventStateTransition,
		Actor:      actor,
		FromStatus: &from,
		ToStatus:   &to,
		Summary:    "status changed when the assistant wrote the body",
		Payload:    "{}",
	}); err != nil {
		slog.WarnContext(ctx, "post log append failed", logging.AttrComponent, logComponent, logging.AttrError, err)
	}
}

func createVersion(ctx context.Context, st *requestState, content, note string) error {
	id, err := models.NewID()
	if err != nil {
		return err
	}
	version := &models.PostVersion{
		ID:      id,
		PostID:  st.postID,
		Content: content,
		Note:    note,
		Creator: models.PostVersionCreatorAssistant,
	}
	// CreateNext locks the post row first, so a deleted post surfaces as
	// ErrNoRows rather than an FK violation.
	if err := st.repos.Versions.CreateNext(ctx, version); err != nil {
		return writeErr(err, "create version")
	}
	return nil
}
