// Package ideas implements the workspace Ideas backlog (CON-315): capture,
// presence-aware edits, and the triage verdict. It owns the invariants the UI
// relies on — remind_at exists only for `later`, a decision stamps
// decided_at/decided_by, returning to the inbox clears all of them, and the
// author is stamped from the session and never changes.
package ideas

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Length bounds guard the unbounded TEXT columns (CON-315 §9).
const (
	MaxTitleLen = 500
	MaxNoteLen  = 10000
)

// remind_at bounds (CON-315 §9): a minute of clock-skew tolerance in the past,
// at most a year ahead.
const (
	remindSkew   = time.Minute
	remindMaxAge = 365 * 24 * time.Hour
)

// largeBacklog is the per-tenant size above which List logs a warning — the
// signal to add pagination and server-side counts (CON-315 §10).
const largeBacklog = 2000

// Validation errors. The handler maps these to HTTP 400.
var (
	ErrEmptyTitle         = errors.New("idea title is required")
	ErrTitleTooLong       = errors.New("idea title is too long")
	ErrNoteTooLong        = errors.New("idea note is too long")
	ErrInvalidVerdict     = errors.New("invalid verdict")
	ErrRemindAtRequired   = errors.New("remind_at is required for verdict later")
	ErrRemindAtNotAllowed = errors.New("remind_at is only allowed for verdict later")
	ErrRemindAtOutOfRange = errors.New("remind_at must be between now and one year ahead")
	// ErrCampaignNotFound means the campaign is unknown, in another tenant, or
	// soft-deleted. It is a 400 (bad reference), not a 404 on the idea.
	ErrCampaignNotFound = errors.New("campaign not found")
)

// ErrNotFound is returned when no idea matches (unknown id or cross-tenant).
var ErrNotFound = errors.New("idea not found")

// IsValidation reports whether err is a 400-worthy input error.
func IsValidation(err error) bool {
	for _, target := range []error{
		ErrEmptyTitle, ErrTitleTooLong, ErrNoteTooLong, ErrInvalidVerdict,
		ErrRemindAtRequired, ErrRemindAtNotAllowed, ErrRemindAtOutOfRange,
		ErrCampaignNotFound,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// Service is the Ideas use case over the idea and user repositories.
type Service struct {
	repo  repository.IdeaRepository
	users repository.UserRepository
	now   func() time.Time
}

// New returns an Ideas service. users resolves the author's name snapshot.
func New(repo repository.IdeaRepository, users repository.UserRepository) *Service {
	return &Service{repo: repo, users: users, now: time.Now}
}

// stamp is the current time at the microsecond resolution Postgres stores, so
// a value echoed on a write matches what a later read returns.
func (s *Service) stamp() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

// List returns the ideas in scope, oldest first. It is never nil.
func (s *Service) List(ctx context.Context, filter repository.IdeaListFilter) ([]models.Idea, error) {
	list, err := s.repo.List(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(list) > largeBacklog {
		slog.WarnContext(ctx, "ideas: large backlog, consider pagination", "count", len(list))
	}
	return list, nil
}

// Get returns one idea, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id string) (*models.Idea, error) {
	idea, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return idea, nil
}

// CreateInput is a captured idea. UserID is the session user and becomes the
// immutable author; it is never read from the request body.
type CreateInput struct {
	Title      string
	Note       string
	CampaignID *string
	UserID     string
}

// Create validates and persists a new, undecided idea.
func (s *Service) Create(ctx context.Context, in CreateInput) (*models.Idea, error) {
	title := strings.TrimSpace(in.Title)
	note := strings.TrimSpace(in.Note)
	if err := validateText(title, note); err != nil {
		return nil, err
	}
	if err := s.checkCampaign(ctx, in.CampaignID); err != nil {
		return nil, err
	}
	author, err := s.users.GetByID(ctx, in.UserID)
	if err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	now := s.stamp()
	idea := &models.Idea{
		ID:            id,
		Title:         title,
		Note:          note,
		CampaignID:    in.CampaignID,
		CreatedBy:     &author.ID,
		CreatedByName: authorName(author),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.repo.Create(ctx, idea); err != nil {
		return nil, err
	}
	return idea, nil
}

// UpdateInput is a presence-aware edit: a nil Title/Note is left unchanged, and
// the campaign is only touched when SetCampaign is true (CampaignID nil then
// detaches the idea back to workspace-wide).
type UpdateInput struct {
	Title       *string
	Note        *string
	SetCampaign bool
	CampaignID  *string
}

// Update applies the edit and returns the idea plus the names of the fields it
// wrote. Only those columns are persisted.
func (s *Service) Update(ctx context.Context, id string, in UpdateInput) (*models.Idea, []string, error) {
	idea, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	var columns []string
	if in.Title != nil {
		idea.Title = strings.TrimSpace(*in.Title)
		columns = append(columns, "title")
	}
	if in.Note != nil {
		idea.Note = strings.TrimSpace(*in.Note)
		columns = append(columns, "note")
	}
	if in.SetCampaign {
		if err := s.checkCampaign(ctx, in.CampaignID); err != nil {
			return nil, nil, err
		}
		idea.CampaignID = in.CampaignID
		columns = append(columns, "campaign_id")
	}
	if err := validateText(idea.Title, idea.Note); err != nil {
		return nil, nil, err
	}
	idea.UpdatedAt = s.stamp()
	if err := s.write(ctx, idea, columns...); err != nil {
		return nil, nil, err
	}
	return idea, columns, nil
}

// SetVerdict records a triage decision (CON-315 FR4) and returns the idea plus
// the verdict it replaced. A nil verdict returns the idea to the inbox and
// clears every decision field. A non-nil verdict — including re-deciding with
// the same one — stamps decided_at/decided_by afresh.
func (s *Service) SetVerdict(ctx context.Context, id string, verdict *models.IdeaVerdict, remindAt *time.Time, userID string) (*models.Idea, *models.IdeaVerdict, error) {
	now := s.stamp()
	if err := validateVerdict(verdict, remindAt, now); err != nil {
		return nil, nil, err
	}
	idea, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	previous := idea.Verdict

	idea.Verdict = verdict
	idea.RemindAt = nil
	idea.DecidedAt = nil
	idea.DecidedBy = nil
	if verdict != nil {
		if remindAt != nil {
			r := remindAt.UTC().Truncate(time.Microsecond)
			idea.RemindAt = &r
		}
		idea.DecidedAt = &now
		idea.DecidedBy = &userID
	}
	idea.UpdatedAt = now
	if err := s.write(ctx, idea, "verdict", "remind_at", "decided_at", "decided_by"); err != nil {
		return nil, nil, err
	}
	return idea, previous, nil
}

// Delete hard-deletes an idea and returns it as it was, or ErrNotFound.
func (s *Service) Delete(ctx context.Context, id string) (*models.Idea, error) {
	idea, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	ok, err := s.repo.Delete(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	return idea, nil
}

func (s *Service) write(ctx context.Context, idea *models.Idea, columns ...string) error {
	ok, err := s.repo.Update(ctx, idea, columns...)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// checkCampaign verifies a non-nil campaign id names a live campaign in the
// caller's tenant. Archived campaigns are allowed.
func (s *Service) checkCampaign(ctx context.Context, campaignID *string) error {
	if campaignID == nil {
		return nil
	}
	ok, err := s.repo.LiveCampaignExists(ctx, *campaignID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCampaignNotFound
	}
	return nil
}

func validateText(title, note string) error {
	if title == "" {
		return ErrEmptyTitle
	}
	if len([]rune(title)) > MaxTitleLen {
		return ErrTitleTooLong
	}
	if len([]rune(note)) > MaxNoteLen {
		return ErrNoteTooLong
	}
	return nil
}

func validateVerdict(verdict *models.IdeaVerdict, remindAt *time.Time, now time.Time) error {
	if verdict != nil && !verdict.Valid() {
		return ErrInvalidVerdict
	}
	later := verdict != nil && *verdict == models.IdeaVerdictLater
	switch {
	case later && remindAt == nil:
		return ErrRemindAtRequired
	case !later && remindAt != nil:
		return ErrRemindAtNotAllowed
	case later && (remindAt.Before(now.Add(-remindSkew)) || remindAt.After(now.Add(remindMaxAge))):
		return ErrRemindAtOutOfRange
	}
	return nil
}

// authorName is the capture-time author snapshot: the member's name, falling
// back to their email so the column is never blank.
func authorName(u *models.User) string {
	if name := strings.TrimSpace(u.Name); name != "" {
		return name
	}
	return u.Email
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
