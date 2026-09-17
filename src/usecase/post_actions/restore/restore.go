// Package restore implements the shared "restore a Post to an earlier
// version" operation (CON-68). It is the single source of truth used by
// both the REST endpoint (POST /api/posts/:id/restore) and the Post
// Assistant's restoreVersion tool, so the two entry points can never
// drift.
//
// Restore is non-destructive and append-only: it copies the target
// version's content into a brand-new version that becomes the new HEAD.
// No existing version row is ever modified or deleted, so a restore is
// itself reversible. When the live post has edits not captured by any
// version (a "dirty HEAD", e.g. an assistant edit with saveVersion
// false), those edits are snapshotted first so nothing is lost.
package restore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

var (
	// ErrPostNotFound is returned when the post being restored does not exist.
	ErrPostNotFound = errors.New("post not found")
	// ErrVersionNotFound is returned when the requested version number
	// does not exist for the post (or is not positive).
	ErrVersionNotFound = errors.New("version not found")
	// ErrNotEditable is returned when the post is in a status whose live
	// content must not be silently rewritten (scheduled / published).
	ErrNotEditable = errors.New("post is not in an editable state")
)

// Trigger identifies which entry point initiated a restore (recorded in
// the audit log and used to attribute the new version's creator role).
const (
	TriggerAPI       = "api"
	TriggerAssistant = "assistant"
)

// editableStatuses are the post statuses whose content may be restored.
// Scheduled / ScheduledForManualPublish / Published are excluded: their
// content is in-flight or already out, so a bulk swap to an old version
// is blocked (CON-68 §9). Failed / NotPublished are editable again per
// the state machine, so a restore there is allowed.
var editableStatuses = map[models.PostStatus]bool{
	models.PostStatusDraft:           true,
	models.PostStatusReadyForPublish: true,
	models.PostStatusFailed:          true,
	models.PostStatusNotPublished:    true,
}

// Options controls a single restore.
type Options struct {
	// Actor is the user id recorded on the audit entry. Required.
	Actor string
	// Trigger is TriggerAPI or TriggerAssistant.
	Trigger string
	// VersionNumber is the (absolute) version to restore to. Required;
	// relative references ("previous") must be resolved by the caller.
	VersionNumber int
}

// Result is the outcome of a restore.
type Result struct {
	// Post is the post after the restore (its Content is the restored
	// content, or unchanged when NoOp).
	Post *models.Post
	// RestoredFromVersion is the version number that was restored.
	RestoredFromVersion int
	// NewVersionNumber is the appended version that now holds the restored
	// content. 0 when NoOp.
	NewVersionNumber int
	// AutoSnapshotCreated is true when a "dirty HEAD" safety snapshot of
	// the pre-restore content was appended before the restore version.
	AutoSnapshotCreated bool
	// NoOp is true when the target version's content already equals the
	// live content — nothing was changed and no version was appended.
	NoOp bool
}

// Service performs restores. Construct with New.
type Service struct {
	db       *bun.DB
	posts    repository.PostRepository
	versions repository.PostVersionRepository
	logs     repository.PostLogRepository
	hub      eventhub.Hub
}

// New wires a restore Service. logs and hub may be nil (audit / eventing
// disabled).
func New(
	db *bun.DB,
	posts repository.PostRepository,
	versions repository.PostVersionRepository,
	logs repository.PostLogRepository,
	hub eventhub.Hub,
) *Service {
	return &Service{db: db, posts: posts, versions: versions, logs: logs, hub: hub}
}

// Restore rolls the post identified by postID back to the content of
// version opts.VersionNumber, appending new version rows rather than
// mutating existing ones. The optional dirty-HEAD safety snapshot, the
// restore version, the post update, and the audit entry all commit in a
// single transaction so they succeed or roll back together.
func (s *Service) Restore(ctx context.Context, postID string, opts Options) (*Result, error) {
	if opts.VersionNumber <= 0 {
		return nil, ErrVersionNotFound
	}

	post, err := s.posts.GetByID(ctx, postID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPostNotFound
		}
		return nil, err
	}
	if !editableStatuses[post.Status] {
		return nil, fmt.Errorf("%w: %s", ErrNotEditable, post.Status)
	}

	target, err := s.versions.GetByPostIDAndVersionNumber(ctx, postID, opts.VersionNumber)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, ErrVersionNotFound
	}

	// No-op: the live content already equals the target — nothing to do,
	// and appending a redundant version would only clutter the history.
	if target.Content == post.Content {
		return &Result{
			Post:                post,
			RestoredFromVersion: opts.VersionNumber,
			NoOp:                true,
		}, nil
	}

	// post_versions.creator is a role enum ('user' | 'assistant'), not a
	// user id — an assistant-driven restore is "assistant", REST is "user".
	creator := "user"
	if opts.Trigger == TriggerAssistant {
		creator = "assistant"
	}

	// Capture the pre-restore live content (for the dirty-HEAD snapshot)
	// before overwriting it with the restored content.
	preRestoreContent := post.Content
	post.Content = target.Content
	post.UpdatedAt = time.Now().UTC()

	// Version numbering must be serialized with the inserts, so the latest
	// version is read and nextNum computed INSIDE the transaction — using
	// the tx handle, not a pooled repo call. Postgres runs writers
	// concurrently (SQLite did not), so the transaction first locks the post
	// row (SELECT ... FOR UPDATE): a concurrent restore of the same post then
	// blocks until this one commits, after which it reads the version we just
	// appended and computes the next number — no duplicate version_number, no
	// UNIQUE-constraint collision.
	var (
		restoredVersionNum  int
		autoSnapshotCreated bool
	)
	err = s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewRaw(`SELECT 1 FROM posts WHERE id = ? AND tenant_id = ? FOR UPDATE`, postID, post.TenantID).Exec(ctx); err != nil {
			return err
		}
		latest := new(models.PostVersion)
		err := tx.NewSelect().
			Model(latest).
			Where("pv.post_id = ?", postID).
			OrderExpr("pv.version_number DESC").
			Limit(1).
			Scan(ctx)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			latest = nil
		case err != nil:
			return err
		}

		nextNum := 1
		if latest != nil {
			nextNum = latest.VersionNumber + 1
		}

		// Dirty HEAD: the live content isn't captured by the latest
		// snapshot (e.g. an assistant edit with saveVersion=false).
		// Snapshot it first so the pre-restore state is recoverable.
		if latest != nil && latest.Content != preRestoreContent {
			snapID, err := models.NewID()
			if err != nil {
				return err
			}
			if _, err := tx.NewInsert().Model(&models.PostVersion{
				ID:            snapID,
				PostID:        postID,
				VersionNumber: nextNum,
				Content:       preRestoreContent,
				Note:          "Auto-saved before restore",
				Creator:       "user",
			}).Exec(ctx); err != nil {
				return err
			}
			autoSnapshotCreated = true
			nextNum++
		}

		restoreID, err := models.NewID()
		if err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(&models.PostVersion{
			ID:            restoreID,
			PostID:        postID,
			VersionNumber: nextNum,
			Content:       target.Content,
			Note:          fmt.Sprintf("Restored from v%d", opts.VersionNumber),
			Creator:       creator,
		}).Exec(ctx); err != nil {
			return err
		}
		restoredVersionNum = nextNum

		if _, err := tx.NewUpdate().Model(post).WherePK().Exec(ctx); err != nil {
			return err
		}

		if s.logs != nil {
			logEntry, err := s.buildLogEntry(post.ID, opts, nextNum, autoSnapshotCreated)
			if err != nil {
				return err
			}
			if err := s.logs.AppendTx(ctx, tx, logEntry); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.publishRestored(post.ID, opts, restoredVersionNum)

	return &Result{
		Post:                post,
		RestoredFromVersion: opts.VersionNumber,
		NewVersionNumber:    restoredVersionNum,
		AutoSnapshotCreated: autoSnapshotCreated,
	}, nil
}

func (s *Service) buildLogEntry(postID string, opts Options, newVersion int, autoSnapshot bool) (*models.PostLog, error) {
	logID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]any{
		"restored_from_version": opts.VersionNumber,
		"new_version_number":    newVersion,
		"auto_snapshot_created": autoSnapshot,
		"trigger":               opts.Trigger,
	})
	return &models.PostLog{
		ID:        logID,
		PostID:    postID,
		EventType: models.PostLogEventPostRestored,
		Actor:     opts.Actor,
		Summary:   fmt.Sprintf("post restored to v%d", opts.VersionNumber),
		Payload:   logs.SanitizeAndCap(string(payload)),
	}, nil
}

func (s *Service) publishRestored(postID string, opts Options, newVersion int) {
	if s.hub == nil {
		return
	}
	evID, err := models.NewID()
	if err != nil {
		return
	}
	_ = s.hub.Publish(context.Background(), eventhub.Event{
		ID:     evID,
		Topic:  "entity:post:" + postID,
		// Dotted bus wire type (CON-285). The post_logs.event_type and
		// tenant_activity_events taxonomy constants stay "post_restored".
		Type:   "post.restored",
		UserID: opts.Actor,
		Payload: map[string]any{
			"restoredFromVersion": opts.VersionNumber,
			"newVersionNumber":    newVersion,
		},
	})
}
