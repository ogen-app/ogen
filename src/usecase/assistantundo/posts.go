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
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// postContent is a post's copy as a rewrite found and left it.
type postContent struct {
	Content string            `json:"content"`
	Status  models.PostStatus `json:"status"`
}

// PostContent is a post's copy and status for the undo log of a rewrite.
func PostContent(p *models.Post) models.JSONMap {
	return toMap(postContent{Content: p.Content, Status: p.Status})
}

type postRewrite struct{}

// Undo puts back the copy a rewrite replaced, and its status, while the post
// still holds the rewrite, and saves it as the newest version so the post's
// history follows. The post's row is locked first.
func (postRewrite) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var before, after postContent
	if err := fromMap(a.Before, &before); err != nil {
		return err
	}
	if err := fromMap(a.After, &after); err != nil {
		return err
	}
	var p models.Post
	err := tx.NewSelect().Model(&p).Where("po.id = ?", a.EntityID).For("UPDATE OF po").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrChangedSince
	}
	if err != nil {
		return err
	}
	if p.Content != after.Content || p.Status != after.Status {
		return ErrChangedSince
	}
	p.Content, p.Status, p.UpdatedAt = before.Content, before.Status, time.Now().UTC()
	if _, err := tx.NewUpdate().Model(&p).Column("content", "status", "updated_at").WherePK().Exec(ctx); err != nil {
		return err
	}
	var latest int
	if err := tx.NewSelect().Model((*models.PostVersion)(nil)).ColumnExpr("COALESCE(MAX(pv.version_number), 0)").
		Where("pv.post_id = ?", p.ID).Scan(ctx, &latest); err != nil {
		return err
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	by, _ := logging.UserIDFrom(ctx)
	_, err = tx.NewInsert().Model(&models.PostVersion{ID: id, PostID: p.ID, VersionNumber: latest + 1, Content: before.Content,
		Note: "Rewrite undone", Creator: models.PostVersionCreatorUser, CreatedBy: by}).Exec(ctx)
	return err
}

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

// WithPosts lets s undo posts taken off the calendar, rewritten or changed
// to another post type, and posts made from ideas.
func (s *Service) WithPosts(posts repository.PostRepository) *Service {
	s.undoers[models.AssistantActionPostSchedules] = postSchedules{posts}
	s.undoers[models.AssistantActionPostTypes] = postTypes{posts}
	s.undoers[models.AssistantActionPostCreated] = postCreated{posts}
	s.undoers[models.AssistantActionPostRewritten] = postRewrite{}
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
// it, its theses or its versions would alter. ID names it among several made
// in one call.
type createdPost struct {
	ID          string            `json:"id,omitempty"`
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
	return toMap(created(p, notes))
}

// MadePost is a post the assistant made and its notes.
type MadePost struct {
	Post  *models.Post
	Notes []models.PostNote
}

type createdPosts struct {
	Posts []createdPost `json:"posts"`
}

// PostsCreated is several posts the assistant made in one call, each with
// its notes, as one undo log entry.
func PostsCreated(made []MadePost) models.JSONMap {
	out := createdPosts{Posts: make([]createdPost, len(made))}
	for i, m := range made {
		out.Posts[i] = created(m.Post, m.Notes)
		out.Posts[i].ID = m.Post.ID
	}
	return toMap(out)
}

func created(p *models.Post, notes []models.PostNote) createdPost {
	c := createdPost{Status: p.Status, Content: p.Content, UpdatedAt: p.UpdatedAt.UTC().Truncate(time.Microsecond), Notes: []createdNote{}}
	if p.ScheduledAt != nil {
		c.ScheduledAt = new(p.ScheduledAt.UTC().Truncate(time.Microsecond))
	}
	for _, n := range notes {
		c.Notes = append(c.Notes, createdNote{ID: n.ID, UpdatedAt: n.UpdatedAt.UTC().Truncate(time.Microsecond)})
	}
	slices.SortFunc(c.Notes, func(a, b createdNote) int { return strings.Compare(a.ID, b.ID) })
	return c
}

type postCreated struct{ posts repository.PostRepository }

// Undo deletes the posts made, all of them or none, each while it is as the
// assistant made it: same status, content, date and notes, and no version
// saved or file attached since. An entry of one post names it by EntityID.
func (u postCreated) Undo(ctx context.Context, tx bun.IDB, a *models.AssistantAction) error {
	var many createdPosts
	if err := fromMap(a.After, &many); err != nil {
		return err
	}
	if len(many.Posts) == 0 {
		return u.undoOne(ctx, tx, a.EntityID, a.After)
	}
	for _, c := range many.Posts {
		id := c.ID
		c.ID = ""
		if err := u.undoOne(ctx, tx, id, toMap(c)); err != nil {
			return err // rolls back the ones deleted
		}
	}
	return nil
}

// undoOne deletes one post while it matches after. Its row is locked first,
// so nothing lands between the check and the delete; its links, notes and
// log go with it.
func (u postCreated) undoOne(ctx context.Context, tx bun.IDB, id string, after models.JSONMap) error {
	var p models.Post
	err := tx.NewSelect().Model(&p).Where("po.id = ?", id).For("UPDATE OF po").Scan(ctx)
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
	if !same(PostCreated(&p, notes), after) {
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
