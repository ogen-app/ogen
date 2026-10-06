// Package withdraw removes posts from Ogen together with their copies queued in
// Zernio: deleting a post, and deleting a campaign, which unschedules its
// posts. Each Ogen change commits in the same transaction as the
// withdraw_zernio_post tasks it needs, so nothing Ogen has dropped can stay
// queued to publish.
package withdraw

import (
	"context"
	"database/sql"
	"errors"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// ErrPublished refuses deleting a published post: it is live on the network,
// and its row anchors the post's analytics.
var ErrPublished = errors.New("a published post can't be deleted")

// Enqueuer queues a Zernio withdrawal inside a transaction. *queues.Enqueuer
// satisfies it.
type Enqueuer interface {
	EnqueueWithdrawTx(ctx context.Context, tx *sql.Tx, task queues.WithdrawZernioPostTask) error
}

// Service deletes posts and campaigns. DB and Jobs are required for the
// Zernio side; Logs is optional (nil skips the audit entries).
type Service struct {
	db        *bun.DB
	posts     repository.PostRepository
	campaigns repository.CampaignRepository
	logs      repository.PostLogRepository
	jobs      Enqueuer
}

// New builds the service.
func New(db *bun.DB, posts repository.PostRepository, campaigns repository.CampaignRepository, postLogs repository.PostLogRepository, jobs Enqueuer) *Service {
	return &Service{db: db, posts: posts, campaigns: campaigns, logs: postLogs, jobs: jobs}
}

// DeletePost deletes post and, when it is scheduled with a copy queued in
// Zernio, queues that copy's withdrawal in the same transaction. A scheduled
// post whose submit is still in flight has no copy yet; the submit worker
// withdraws the one it creates once it finds the row gone. Other statuses hold
// nothing queued: a failed post's Zernio copy is either finished (it may be a
// partial publish) or already being withdrawn by the reconciler. A published
// post is refused with ErrPublished. Reports false when the post was already
// gone.
func (s *Service) DeletePost(ctx context.Context, post *models.Post, actor string) (bool, error) {
	if post.Status == models.PostStatusPublished {
		return false, ErrPublished
	}
	var deleted bool
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		deleted, err = s.posts.DeleteTx(ctx, tx, post.ID)
		if err != nil || !deleted || post.Status != models.PostStatusScheduled || post.PublisherPostID == "" {
			return err
		}
		return s.jobs.EnqueueWithdrawTx(ctx, tx.Tx, queues.WithdrawZernioPostTask{
			PublisherPostID: post.PublisherPostID,
			PostID:          post.ID,
			TenantID:        post.TenantID,
			Reason:          queues.WithdrawReasonPostDeleted,
			Actor:           actor,
		})
	})
	return deleted, err
}

// DeleteCampaign soft-deletes the campaign and unschedules its posts: each
// scheduled or manually-scheduled post goes back to draft, and every one that
// Zernio holds is withdrawn. Archived campaigns are not affected by this; an
// archive keeps its schedule. Returns whether the campaign existed and how
// many posts were unscheduled.
func (s *Service) DeleteCampaign(ctx context.Context, campaignID, actor string) (bool, int, error) {
	var (
		deleted     bool
		unscheduled int
	)
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		deleted, err = s.campaigns.DeleteTx(ctx, tx, campaignID)
		if err != nil || !deleted {
			return err
		}
		posts, err := s.posts.UnscheduleByCampaignTx(ctx, tx, campaignID)
		if err != nil {
			return err
		}
		for i := range posts {
			if err := s.unscheduled(ctx, tx, &posts[i], actor); err != nil {
				return err
			}
		}
		unscheduled = len(posts)
		return nil
	})
	return deleted, unscheduled, err
}

// unscheduled records one post the campaign delete moved to draft and queues
// the withdrawal of its Zernio copy.
func (s *Service) unscheduled(ctx context.Context, tx bun.Tx, post *models.Post, actor string) error {
	if s.logs != nil {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		from, to := post.Status, models.PostStatusDraft
		if err := s.logs.AppendTx(ctx, tx, &models.PostLog{
			ID:         id,
			PostID:     post.ID,
			EventType:  models.PostLogEventStateTransition,
			Actor:      actor,
			FromStatus: &from,
			ToStatus:   &to,
			Summary:    "campaign deleted: post unscheduled",
			Payload: logs.MarshalCapped(map[string]string{
				"reason":            queues.WithdrawReasonCampaignDeleted,
				"publisher_post_id": post.PublisherPostID,
			}),
		}); err != nil {
			return err
		}
	}
	if post.PublisherPostID == "" {
		return nil
	}
	return s.jobs.EnqueueWithdrawTx(ctx, tx.Tx, queues.WithdrawZernioPostTask{
		PublisherPostID: post.PublisherPostID,
		PostID:          post.ID,
		TenantID:        post.TenantID,
		Reason:          queues.WithdrawReasonCampaignDeleted,
		Actor:           actor,
	})
}
