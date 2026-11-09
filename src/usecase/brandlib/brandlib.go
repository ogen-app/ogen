// Package brandlib keeps the brand library: its voices, audiences and
// guardrails. REST and the assistant edit them through here, with the same
// defaults and rules.
package brandlib

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/brandcheck"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// IsRule reports whether err is brand material that breaks a rule (HTTP
// 422); its message is safe to show.
func IsRule(err error) bool {
	_, ok := errors.AsType[*brandcheck.Error](err)
	return ok
}

// Service edits the library.
type Service struct {
	repo repository.BrandRepository
	now  func() time.Time
}

// New returns a Service.
func New(repo repository.BrandRepository) *Service {
	return &Service{repo: repo, now: Now}
}

// Now is time.Now normalised to the microsecond Postgres stores, so a value
// written and echoed matches what a later read returns.
func Now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

// ── Voices ──────────────────────────────────────────────────────────────────

// NormalizeVoice fills a voice's unset lists and origin.
func NormalizeVoice(v *models.BrandVoice) {
	if v.Samples == nil {
		v.Samples = models.StringSlice{}
	}
	if v.ChannelNotes == nil {
		v.ChannelNotes = models.StringMap{}
	}
	if v.Origin.Kind == "" {
		v.Origin = models.BrandOrigin{Kind: "blank"}
	}
}

// CreateVoice adds a voice; a default one takes the flag from the others.
func (s *Service) CreateVoice(ctx context.Context, v *models.BrandVoice) (*models.BrandVoice, error) {
	NormalizeVoice(v)
	if err := brandcheck.Voice(v); err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	v.ID = id
	// The summary is read off the samples, never authored; until that runs it
	// is "", the honest rendering of a voice with nothing read back yet.
	v.Summary = ""
	v.CreatedAt, v.UpdatedAt = s.now(), s.now()
	if err := s.repo.CreateVoice(ctx, v); err != nil {
		return nil, err
	}
	v.Usage = models.BrandUsage{}
	return v, nil
}

// UpdateVoice replaces a voice's fields; sql.ErrNoRows when it doesn't exist.
func (s *Service) UpdateVoice(ctx context.Context, v *models.BrandVoice) (*models.BrandVoice, error) {
	NormalizeVoice(v)
	if err := brandcheck.Voice(v); err != nil {
		return nil, err
	}
	v.Summary = ""
	v.UpdatedAt = s.now()
	if err := s.repo.UpdateVoice(ctx, v); err != nil {
		return nil, err
	}
	v.Usage = models.BrandUsage{}
	return v, nil
}

// DeleteVoice removes a voice; false when it doesn't exist.
func (s *Service) DeleteVoice(ctx context.Context, id string) (bool, error) {
	return s.repo.DeleteVoice(ctx, id)
}

// ── Audiences ───────────────────────────────────────────────────────────────

// NormalizeAudience fills an audience's unset origin.
func NormalizeAudience(a *models.BrandAudience) {
	if a.Origin.Kind == "" {
		a.Origin = models.BrandOrigin{Kind: "blank"}
	}
}

// CreateAudience adds an audience.
func (s *Service) CreateAudience(ctx context.Context, a *models.BrandAudience) (*models.BrandAudience, error) {
	NormalizeAudience(a)
	if err := brandcheck.Audience(a); err != nil {
		return nil, err
	}
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	a.ID = id
	a.Summary = ""
	a.CreatedAt, a.UpdatedAt = s.now(), s.now()
	if err := s.repo.CreateAudience(ctx, a); err != nil {
		return nil, err
	}
	a.Usage = models.BrandUsage{}
	return a, nil
}

// UpdateAudience replaces an audience's fields; sql.ErrNoRows when it
// doesn't exist.
func (s *Service) UpdateAudience(ctx context.Context, a *models.BrandAudience) (*models.BrandAudience, error) {
	NormalizeAudience(a)
	if err := brandcheck.Audience(a); err != nil {
		return nil, err
	}
	a.Summary = ""
	a.UpdatedAt = s.now()
	if err := s.repo.UpdateAudience(ctx, a); err != nil {
		return nil, err
	}
	a.Usage = models.BrandUsage{}
	return a, nil
}

// DeleteAudience removes an audience; false when it doesn't exist.
func (s *Service) DeleteAudience(ctx context.Context, id string) (bool, error) {
	return s.repo.DeleteAudience(ctx, id)
}

// ── Guardrails ──────────────────────────────────────────────────────────────

// NormalizeGuardrails fills the rules' unset lists.
func NormalizeGuardrails(g *models.BrandGuardrails) {
	if g.MayClaim == nil {
		g.MayClaim = models.StringSlice{}
	}
	if g.NeverClaim == nil {
		g.NeverClaim = models.StringSlice{}
	}
	if g.BannedWords == nil {
		g.BannedWords = models.StringSlice{}
	}
}

// NormalizeFactStatements trims, drops blanks and de-duplicates, keeping the
// first occurrence. Never nil, so a present-but-empty list still reconciles
// (to an empty ledger).
func NormalizeFactStatements(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// GuardrailsSaved is a guardrails save: the rules as stored, and how the
// ledger was reconciled when facts were given.
type GuardrailsSaved struct {
	Guardrails *models.BrandGuardrails
	// Changed is false for a save that matched what was stored, which leaves
	// the row and its updated_at alone.
	Changed bool
	// Facts is set when facts were given and the save ran.
	Facts *repository.FactsReconciled
}

// SaveGuardrails sets the rules. A non-nil facts reconciles the ledger to
// those statements; nil leaves it alone. author is who saves (nil for none).
func (s *Service) SaveGuardrails(ctx context.Context, g *models.BrandGuardrails, facts []string, author *string) (*GuardrailsSaved, error) {
	if facts != nil {
		facts = NormalizeFactStatements(facts)
	}
	NormalizeGuardrails(g)
	if err := brandcheck.Guardrails(g, facts); err != nil {
		return nil, err
	}
	existing, err := s.repo.GetGuardrails(ctx)
	if err != nil {
		return nil, err
	}
	// An unchanged save must not restamp updated_at: "when were these last
	// checked" is a real question here. With facts given, unchanged also means
	// the ledger already holds exactly those statements.
	if existing != nil && GuardrailsEqual(existing, g) && (facts == nil || sameStatementSet(existing.Facts, facts)) {
		return &GuardrailsSaved{Guardrails: existing}, nil
	}
	now := s.now()
	g.UpdatedAt = now
	if existing == nil {
		if g.ID, err = models.NewID(); err != nil {
			return nil, err
		}
		g.CreatedAt = now
	} else {
		// The upsert leaves id and created_at alone, so carry the stored ones.
		g.ID, g.CreatedAt = existing.ID, existing.CreatedAt
	}
	rec, err := s.repo.SaveGuardrails(ctx, g, facts, author)
	if err != nil {
		return nil, err
	}
	saved, err := s.repo.GetGuardrails(ctx)
	if err != nil {
		return nil, err
	}
	out := &GuardrailsSaved{Guardrails: saved, Changed: true}
	if facts != nil {
		out.Facts = &rec
	}
	return out, nil
}

// DeleteGuardrails removes the rules; the ledger is left alone. False when
// there were none.
func (s *Service) DeleteGuardrails(ctx context.Context) (bool, error) {
	return s.repo.DeleteGuardrails(ctx)
}

// GuardrailsEqual reports whether two guardrails hold the same rules.
func GuardrailsEqual(a, b *models.BrandGuardrails) bool {
	return a.Disclaimer == b.Disclaimer &&
		slices.Equal(a.MayClaim, b.MayClaim) &&
		slices.Equal(a.NeverClaim, b.NeverClaim) &&
		slices.Equal(a.BannedWords, b.BannedWords)
}

func sameStatementSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	for _, s := range b {
		if !set[s] {
			return false
		}
	}
	return true
}

// ── Conditional edits ───────────────────────────────────────────────────────

// UpdateVoiceIf replaces a voice's fields while the stored voice still equals
// was (repository.ErrBrandChanged otherwise; sql.ErrNoRows when gone), and
// returns it as it was and as it is.
func (s *Service) UpdateVoiceIf(ctx context.Context, v, was *models.BrandVoice) (before, after *models.BrandVoice, err error) {
	NormalizeVoice(v)
	if err := brandcheck.Voice(v); err != nil {
		return nil, nil, err
	}
	v.Summary = ""
	v.UpdatedAt = s.now()
	if before, err = s.repo.UpdateVoiceIf(ctx, nil, v, was); err != nil {
		return nil, nil, err
	}
	v.Usage = models.BrandUsage{}
	return before, v, nil
}

// RemoveVoice deletes a voice and returns it with the campaigns and posts it
// was bound to; nil when it doesn't exist.
func (s *Service) RemoveVoice(ctx context.Context, id string) (*models.BrandVoice, repository.BrandBindings, error) {
	return s.repo.DeleteVoiceIf(ctx, nil, id, nil)
}

// UpdateAudienceIf replaces an audience's fields while the stored audience
// still equals was, and returns it as it was and as it is.
func (s *Service) UpdateAudienceIf(ctx context.Context, a, was *models.BrandAudience) (before, after *models.BrandAudience, err error) {
	NormalizeAudience(a)
	if err := brandcheck.Audience(a); err != nil {
		return nil, nil, err
	}
	a.Summary = ""
	a.UpdatedAt = s.now()
	if before, err = s.repo.UpdateAudienceIf(ctx, nil, a, was); err != nil {
		return nil, nil, err
	}
	a.Usage = models.BrandUsage{}
	return before, a, nil
}

// RemoveAudience deletes an audience and returns it with the campaigns and
// posts it was bound to; nil when it doesn't exist.
func (s *Service) RemoveAudience(ctx context.Context, id string) (*models.BrandAudience, repository.BrandBindings, error) {
	return s.repo.DeleteAudienceIf(ctx, nil, id, nil)
}

// SaveGuardrailsIf sets the rules while the stored ones still equal was (nil:
// none set), leaving the ledger alone, and returns them as they were and as
// they are.
func (s *Service) SaveGuardrailsIf(ctx context.Context, g, was *models.BrandGuardrails) (before, after *models.BrandGuardrails, err error) {
	NormalizeGuardrails(g)
	if err := brandcheck.Guardrails(g, nil); err != nil {
		return nil, nil, err
	}
	now := s.now()
	g.UpdatedAt = now
	if was == nil {
		if g.ID, err = models.NewID(); err != nil {
			return nil, nil, err
		}
		g.CreatedAt = now
	}
	if before, err = s.repo.SaveGuardrailsIf(ctx, nil, g, was); err != nil {
		return nil, nil, err
	}
	if before != nil {
		g.ID, g.CreatedAt = before.ID, before.CreatedAt
	}
	return before, g, nil
}
