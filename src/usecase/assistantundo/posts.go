package assistantundo

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// PostState is the part of a post a fix changed: its date, or its post type
// and status.
type PostState struct {
	ID          string            `json:"id"`
	ScheduledAt *time.Time        `json:"scheduledAt,omitempty"`
	PostType    string            `json:"postType,omitempty"`
	Status      models.PostStatus `json:"status,omitempty"`
}

type postStates struct {
	Posts []PostState `json:"posts"`
}

// Posts is posts' states for the undo log, in the same order before and
// after a change.
func Posts(posts []PostState) models.JSONMap { return toMap(postStates{Posts: posts}) }

// beforeAfter pairs a change's posts as they were and as it left them.
func beforeAfter(a *models.AssistantAction) (before, after []PostState, err error) {
	var b, f postStates
	if err := fromMap(a.Before, &b); err != nil {
		return nil, nil, err
	}
	if err := fromMap(a.After, &f); err != nil {
		return nil, nil, err
	}
	if len(b.Posts) != len(f.Posts) {
		return nil, nil, ErrChangedSince
	}
	return b.Posts, f.Posts, nil
}

// WithPosts lets s undo posts taken off the calendar and posts changed to
// another post type.
func (s *Service) WithPosts(posts repository.PostRepository) *Service {
	s.undoers[models.AssistantActionPostSchedules] = postSchedules{posts}
	s.undoers[models.AssistantActionPostTypes] = postTypes{posts}
	return s
}

type postSchedules struct{ posts repository.PostRepository }

// Undo dates the posts again, all of them or none: one dated, scheduled or
// deleted since leaves the change in place.
func (u postSchedules) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	before, after, err := beforeAfter(a)
	if err != nil {
		return err
	}
	changes := make([]repository.ScheduleChange, len(before))
	for i := range before {
		changes[i] = repository.ScheduleChange{PostID: before[i].ID, From: after[i].ScheduledAt, To: before[i].ScheduledAt}
	}
	set, err := u.posts.SetScheduleIf(ctx, tx, changes, time.Now().UTC())
	if err != nil {
		return err
	}
	if len(set) != len(changes) {
		return ErrChangedSince // rolls back the ones that were set
	}
	return nil
}

type postTypes struct{ posts repository.PostRepository }

// Undo gives the posts their post type and status back, all of them or
// none.
func (u postTypes) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	before, after, err := beforeAfter(a)
	if err != nil {
		return err
	}
	changes := make([]repository.PostTypeChange, len(before))
	for i := range before {
		changes[i] = repository.PostTypeChange{PostID: before[i].ID, From: after[i].PostType, To: before[i].PostType,
			FromStatus: after[i].Status, ToStatus: before[i].Status}
	}
	set, err := u.posts.SetPostTypeIf(ctx, tx, changes, time.Now().UTC())
	if err != nil {
		return err
	}
	if len(set) != len(changes) {
		return ErrChangedSince
	}
	return nil
}
