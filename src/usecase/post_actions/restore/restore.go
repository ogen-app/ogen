// Package restore implements the shared "restore a Post to an earlier
// version" operation. It is the single source of truth used by
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
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/post_actions/internal/postaudit"
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
	TriggerAPI       = postaudit.TriggerAPI
	TriggerAssistant = postaudit.TriggerAssistant
)

// editableStatuses are the post statuses whose content may be restored.
// Scheduled / ScheduledForManualPublish / Published are excluded: their
// content is in-flight or already out, so a bulk swap to an old version
// is blocked. Failed / NotPublished are editable again per
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
	post, target, err := s.load(ctx, postID, opts.VersionNumber)
	if err != nil {
		return nil, err
	}
	// Appending a version identical to the live content would only
	// clutter the history.
	if target.Content == post.Content {
		return &Result{
			Post:                post,
			RestoredFromVersion: opts.VersionNumber,
			NoOp:                true,
		}, nil
	}

	preRestoreContent := post.Content
	post.Content = target.Content
	post.UpdatedAt = time.Now().UTC()

	appended, err := s.appendVersions(ctx, post, preRestoreContent, opts)
	if err != nil {
		return nil, err
	}

	// Dotted bus wire type; the persisted post_logs / activity taxonomy
	// stays "post_restored".
	postaudit.Publish(s.hub, post.ID, "post.restored", opts.Actor, map[string]any{
		"restoredFromVersion": opts.VersionNumber,
		"newVersionNumber":    appended.versionNumber,
	})

	return &Result{
		Post:                post,
		RestoredFromVersion: opts.VersionNumber,
		NewVersionNumber:    appended.versionNumber,
		AutoSnapshotCreated: appended.autoSnapshot,
	}, nil
}

// load fetches the post and the version to restore, rejecting posts in
// a non-editable status.
func (s *Service) load(ctx context.Context, postID string, versionNumber int) (*models.Post, *models.PostVersion, error) {
	if versionNumber <= 0 {
		return nil, nil, ErrVersionNotFound
	}
	post, err := s.posts.GetByID(ctx, postID)
	if err != nil {
		return nil, nil, postaudit.NotFound(err, ErrPostNotFound)
	}
	if !editableStatuses[post.Status] {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotEditable, post.Status)
	}
	target, err := s.versions.GetByPostIDAndVersionNumber(ctx, postID, versionNumber)
	if err != nil {
		return nil, nil, err
	}
	if target == nil {
		return nil, nil, ErrVersionNotFound
	}
	return post, target, nil
}

type appendResult struct {
	versionNumber int
	autoSnapshot  bool
}

// appendVersions appends the restore version (preceded by a dirty-HEAD
// snapshot of preRestoreContent when the latest version doesn't hold
// it), saves post and writes the audit entry in one transaction.
//
// The post row is locked FOR UPDATE before the latest version is read,
// so a concurrent restore of the same post blocks until this one
// finishes and then numbers its versions after ours, avoiding a
// duplicate version_number.
func (s *Service) appendVersions(ctx context.Context, post *models.Post, preRestoreContent string, opts Options) (appendResult, error) {
	var res appendResult
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		latest, err := lockAndReadLatest(ctx, tx, post)
		if err != nil {
			return err
		}

		nextNum := 1
		if latest != nil {
			nextNum = latest.VersionNumber + 1
		}
		// Dirty HEAD: live edits not captured by any version (e.g. an
		// assistant edit with saveVersion=false) are snapshotted first.
		if latest != nil && latest.Content != preRestoreContent {
			if err := insertVersion(ctx, tx, post.ID, nextNum, preRestoreContent, "Auto-saved before restore", postaudit.CreatorUser); err != nil {
				return err
			}
			res.autoSnapshot = true
			nextNum++
		}
		note := fmt.Sprintf("Restored from v%d", opts.VersionNumber)
		if err := insertVersion(ctx, tx, post.ID, nextNum, post.Content, note, postaudit.CreatorFor(opts.Trigger)); err != nil {
			return err
		}
		res.versionNumber = nextNum

		if _, err := tx.NewUpdate().Model(post).WherePK().Exec(ctx); err != nil {
			return err
		}
		return s.appendLogTx(ctx, tx, post.ID, opts, res)
	})
	return res, err
}

// lockAndReadLatest locks the post row and returns its latest version,
// or nil when it has none.
func lockAndReadLatest(ctx context.Context, tx bun.Tx, post *models.Post) (*models.PostVersion, error) {
	if _, err := tx.NewRaw(`SELECT 1 FROM posts WHERE id = ? AND tenant_id = ? FOR UPDATE`, post.ID, post.TenantID).Exec(ctx); err != nil {
		return nil, err
	}
	latest := new(models.PostVersion)
	err := tx.NewSelect().
		Model(latest).
		Where("pv.post_id = ?", post.ID).
		OrderExpr("pv.version_number DESC").
		Limit(1).
		Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return latest, nil
}

func insertVersion(ctx context.Context, tx bun.Tx, postID string, number int, content, note, creator string) error {
	id, err := models.NewID()
	if err != nil {
		return err
	}
	_, err = tx.NewInsert().Model(&models.PostVersion{
		ID:            id,
		PostID:        postID,
		VersionNumber: number,
		Content:       content,
		Note:          note,
		Creator:       creator,
	}).Exec(ctx)
	return err
}

func (s *Service) appendLogTx(ctx context.Context, tx bun.Tx, postID string, opts Options, res appendResult) error {
	if s.logs == nil {
		return nil
	}
	entry, err := postaudit.NewLog(postID, models.PostLogEventPostRestored, opts.Actor,
		fmt.Sprintf("post restored to v%d", opts.VersionNumber),
		map[string]any{
			"restored_from_version": opts.VersionNumber,
			"new_version_number":    res.versionNumber,
			"auto_snapshot_created": res.autoSnapshot,
			"trigger":               opts.Trigger,
		})
	if err != nil {
		return err
	}
	return s.logs.AppendTx(ctx, tx, entry)
}
