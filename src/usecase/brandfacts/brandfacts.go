// Package brandfacts keeps the workspace's fact ledger: the claims its posts
// may make, each with its kind, source and dates. REST and the assistant edit
// it through here, with the same defaults and rules.
package brandfacts

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/brandcheck"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// RuleError is a fact the ledger's rules reject (HTTP 422).
type RuleError struct{ Err error }

func (e *RuleError) Error() string { return e.Err.Error() }
func (e *RuleError) Unwrap() error { return e.Err }

// Input is a fact's editable fields. Subject and Kind default to "us" and
// "documented" on create, and AddedAt to today.
type Input struct {
	Statement string
	Subject   models.FactSubject
	Kind      models.FactKind
	Source    string
	AddedAt   *models.CalendarDate
	CheckedAt *models.CalendarDate
	ExpiresAt *models.CalendarDate
}

// Service edits the ledger.
type Service struct {
	repo repository.BrandRepository
	now  func() time.Time
}

// New returns a Service.
func New(repo repository.BrandRepository) *Service {
	return &Service{repo: repo, now: func() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }}
}

func (in Input) fact() (*models.BrandFact, error) {
	f := &models.BrandFact{
		Statement: strings.TrimSpace(in.Statement),
		Subject:   in.Subject,
		Kind:      in.Kind,
		Source:    strings.TrimSpace(in.Source),
		AddedAt:   in.AddedAt, CheckedAt: in.CheckedAt, ExpiresAt: in.ExpiresAt,
	}
	if err := brandcheck.Fact(f); err != nil {
		return nil, &RuleError{Err: err}
	}
	return f, nil
}

// Create adds a fact by userID (nil for none). Errors are *RuleError,
// repository.ErrFactDuplicate and repository.ErrFactLimit.
func (s *Service) Create(ctx context.Context, in Input, userID *string) (*models.BrandFact, error) {
	now := s.now()
	if in.Subject == "" {
		in.Subject = models.FactSubjectUs
	}
	if in.Kind == "" {
		in.Kind = models.FactKindDocumented
	}
	if in.AddedAt == nil {
		today := models.CalendarDateOf(now)
		in.AddedAt = &today
	}
	f, err := in.fact()
	if err != nil {
		return nil, err
	}
	if f.ID, err = models.NewID(); err != nil {
		return nil, err
	}
	f.CreatedBy = userID
	f.CreatedAt, f.UpdatedAt = now, now
	if err := s.repo.CreateFact(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// Update replaces a fact's editable fields and returns it before and after.
// sql.ErrNoRows is an unknown fact.
func (s *Service) Update(ctx context.Context, id string, in Input) (before, after *models.BrandFact, err error) {
	return s.update(ctx, id, in, nil)
}

// UpdateIf is Update, applied only while the fact still holds was's editable
// fields: an edit merged onto was can't overwrite a change made since.
// repository.ErrFactChanged is a fact changed since was was read.
func (s *Service) UpdateIf(ctx context.Context, id string, in Input, was *models.BrandFact) (before, after *models.BrandFact, err error) {
	return s.update(ctx, id, in, was)
}

// update is Update, conditional on was when it is non-nil.
func (s *Service) update(ctx context.Context, id string, in Input, was *models.BrandFact) (before, after *models.BrandFact, err error) {
	f, err := in.fact()
	if err != nil {
		return nil, nil, err
	}
	f.ID, f.UpdatedAt = id, s.now()
	if was != nil {
		before, err = s.repo.UpdateFactIf(ctx, nil, f, was)
	} else {
		before, err = s.repo.UpdateFact(ctx, f)
	}
	if err != nil {
		return nil, nil, err
	}
	return before, f, nil
}

// Delete removes a fact and returns it, or nil for an unknown fact.
func (s *Service) Delete(ctx context.Context, id string) (*models.BrandFact, error) {
	return s.repo.DeleteFact(ctx, id)
}

// Current lists the facts still current on today: expired ones are kept in
// the ledger but no longer usable.
func (s *Service) Current(ctx context.Context) ([]models.BrandFact, error) {
	all, err := s.repo.ListFacts(ctx)
	if err != nil {
		return nil, err
	}
	today := models.CalendarDateOf(s.now())
	out := make([]models.BrandFact, 0, len(all))
	for _, f := range all {
		if !f.ExpiredOn(today) {
			out = append(out, f)
		}
	}
	return out, nil
}

// InputOf is a fact's editable fields, to restore it.
func InputOf(f *models.BrandFact) Input {
	return Input{Statement: f.Statement, Subject: f.Subject, Kind: f.Kind, Source: f.Source,
		AddedAt: f.AddedAt, CheckedAt: f.CheckedAt, ExpiresAt: f.ExpiresAt}
}

// IsRule reports whether err is a fact the rules reject.
func IsRule(err error) bool {
	_, ok := errors.AsType[*RuleError](err)
	return ok
}
