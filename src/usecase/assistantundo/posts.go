package assistantundo

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
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

// WithPosts lets s undo posts taken off the calendar, posts changed to
// another post type, and posts made from ideas.
func (s *Service) WithPosts(posts repository.PostRepository) *Service {
	s.undoers[models.AssistantActionPostSchedules] = postSchedules{posts}
	s.undoers[models.AssistantActionPostTypes] = postTypes{posts}
	s.undoers[models.AssistantActionPostCreated] = postCreated{posts}
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

// createdPost is a post as the assistant made it: what any later change to
// it, its theses or its versions would alter.
type createdPost struct {
	Status      models.PostStatus `json:"status"`
	Content     string            `json:"content"`
	ScheduledAt *time.Time        `json:"scheduledAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	Notes       []createdNote     `json:"notes"`
}

type createdNote struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PostCreated is a post the assistant made and its notes, as an undo log
// stores them, at the microsecond Postgres keeps.
func PostCreated(p *models.Post, notes []models.PostNote) models.JSONMap {
	c := createdPost{Status: p.Status, Content: p.Content, UpdatedAt: p.UpdatedAt.UTC().Truncate(time.Microsecond), Notes: []createdNote{}}
	if p.ScheduledAt != nil {
		c.ScheduledAt = new(p.ScheduledAt.UTC().Truncate(time.Microsecond))
	}
	for _, n := range notes {
		c.Notes = append(c.Notes, createdNote{ID: n.ID, UpdatedAt: n.UpdatedAt.UTC().Truncate(time.Microsecond)})
	}
	slices.SortFunc(c.Notes, func(a, b createdNote) int { return strings.Compare(a.ID, b.ID) })
	return toMap(c)
}

type postCreated struct{ posts repository.PostRepository }

// Undo deletes the post while it is as the assistant made it: same status,
// content, date and notes, and no version saved or file attached since.
// The post's row is locked first, so nothing lands between the check and
// the delete; its links, notes and log go with it.
func (u postCreated) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var p models.Post
	err := tx.NewSelect().Model(&p).Where("po.id = ?", a.EntityID).For("UPDATE OF po").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChangedSince
	}
	if err != nil {
		return err
	}
	var notes []models.PostNote
	if err := tx.NewSelect().Model(&notes).Where("pn.post_id = ?", p.ID).Scan(ctx); err != nil {
		return err
	}
	if !same(PostCreated(&p, notes), a.After) {
		return ErrChangedSince
	}
	for _, m := range []any{(*models.PostVersion)(nil), (*models.PostAttachment)(nil)} {
		var n int
		if err := tx.NewSelect().Model(m).ColumnExpr("count(*)").Where("post_id = ?", p.ID).Scan(ctx, &n); err != nil {
			return err
		}
		if n > 0 {
			return ErrChangedSince
		}
	}
	deleted, err := u.posts.DeleteTx(ctx, tx, p.ID)
	if err != nil {
		return err
	}
	if deleted == nil {
		return ErrChangedSince
	}
	return nil
}
