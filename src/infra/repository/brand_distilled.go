package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/brandcheck"
	"github.com/ogen-app/ogen/src/domain/models"
)

// Reasons a committed item is skipped.
const (
	SkipDuplicate = "duplicate"
	SkipLimit     = "limit"
)

// DistilledLine is a guardrail line or banned word to add.
type DistilledLine struct {
	ProposalID string
	Text       string
}

// DistilledVoice, DistilledAudience and DistilledFact are entries to create,
// each with the proposal item it came from. Ids and timestamps are stamped
// here.
type DistilledVoice struct {
	ProposalID string
	Voice      models.BrandVoice
	// WantDefault asks for the workspace default; it is honoured only when
	// the workspace has no voices yet.
	WantDefault bool
}

type DistilledAudience struct {
	ProposalID string
	Audience   models.BrandAudience
}

type DistilledFact struct {
	ProposalID string
	Fact       models.BrandFact
}

// DistilledBrand is everything one commit adds.
type DistilledBrand struct {
	Voices      []DistilledVoice
	Audiences   []DistilledAudience
	MayClaim    []DistilledLine
	NeverClaim  []DistilledLine
	BannedWords []DistilledLine
	// Disclaimer replaces the workspace's own when set.
	Disclaimer *DistilledLine
	Facts      []DistilledFact
	Author     *string
}

// SkippedItem is a committed item that was not saved.
type SkippedItem struct {
	ProposalID string `json:"proposalId"`
	Reason     string `json:"reason"`
}

// DistilledResult says what a commit saved: the id each voice, audience and
// fact became, the guardrail items merged, and what was skipped.
type DistilledResult struct {
	Voices     map[string]string
	Audiences  map[string]string
	Facts      map[string]string
	Guardrails []string
	Skipped    []SkippedItem
	// StanceCleared is set when the "no guardrails" stance was withdrawn.
	StanceCleared bool
}

func (r *brandRepository) ApplyDistilled(ctx context.Context, in DistilledBrand, done func(ctx context.Context, tx bun.Tx, res *DistilledResult) error) (*DistilledResult, error) {
	res := &DistilledResult{Voices: map[string]string{}, Audiences: map[string]string{}, Facts: map[string]string{}}
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		*res = DistilledResult{Voices: map[string]string{}, Audiences: map[string]string{}, Facts: map[string]string{}}
		if err := lockGuardrails(ctx, tx); err != nil {
			return err
		}
		if err := lockFacts(ctx, tx); err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		if err := addVoices(ctx, tx, in.Voices, now, res); err != nil {
			return err
		}
		for _, a := range in.Audiences {
			row := a.Audience
			if err := stamp(&row.ID, &row.CreatedAt, &row.UpdatedAt, now); err != nil {
				return err
			}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
			res.Audiences[a.ProposalID] = row.ID
		}
		changed, err := mergeGuardrails(ctx, tx, in, now, res)
		if err != nil {
			return err
		}
		added, err := addFacts(ctx, tx, in.Facts, in.Author, now, res)
		if err != nil {
			return err
		}
		if changed || added > 0 {
			stance, err := getStance(ctx, tx)
			if err != nil {
				return err
			}
			if stance != nil {
				if err := deleteStance(ctx, tx); err != nil {
					return err
				}
				res.StanceCleared = true
			}
		}
		if done != nil {
			return done(ctx, tx, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func stamp(id *string, created, updated *time.Time, now time.Time) error {
	newID, err := models.NewID()
	if err != nil {
		return err
	}
	*id, *created, *updated = newID, now, now
	return nil
}

// countRows counts the tenant's rows of model with a count(*) select: bun's
// Count skips the BeforeSelect hook, so it would count every tenant's rows.
func countRows(ctx context.Context, db bun.IDB, model any) (int, error) {
	var n int
	err := db.NewSelect().Model(model).ColumnExpr("count(*)").Scan(ctx, &n)
	return n, err
}

// addVoices creates the voices. A workspace without voices gets a default:
// the one asked for, else the first; an existing default is never moved.
func addVoices(ctx context.Context, tx bun.Tx, voices []DistilledVoice, now time.Time, res *DistilledResult) error {
	if len(voices) == 0 {
		return nil
	}
	existing, err := countRows(ctx, tx, (*models.BrandVoice)(nil))
	if err != nil {
		return err
	}
	pick := -1
	if existing == 0 {
		pick = 0
		for i, v := range voices {
			if v.WantDefault {
				pick = i
				break
			}
		}
	}
	for i, v := range voices {
		row := v.Voice
		row.IsDefault = i == pick
		if err := stamp(&row.ID, &row.CreatedAt, &row.UpdatedAt, now); err != nil {
			return err
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		res.Voices[v.ProposalID] = row.ID
	}
	return nil
}

// lineKey is how two guardrail lines are told apart: case and spacing do not
// count.
func lineKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// mergeList appends the new lines to list, skipping repeats and anything
// beyond limit. It reports whether it added any.
func mergeList(list models.StringSlice, add []DistilledLine, limit int, lower bool, res *DistilledResult) (models.StringSlice, bool) {
	seen := make(map[string]bool, len(list))
	for _, s := range list {
		seen[lineKey(s)] = true
	}
	added := false
	for _, l := range add {
		text := strings.TrimSpace(l.Text)
		if lower {
			text = strings.ToLower(text)
		}
		switch key := lineKey(text); {
		case key == "" || seen[key]:
			res.Skipped = append(res.Skipped, SkippedItem{ProposalID: l.ProposalID, Reason: SkipDuplicate})
		case len(list) >= limit:
			res.Skipped = append(res.Skipped, SkippedItem{ProposalID: l.ProposalID, Reason: SkipLimit})
		default:
			seen[key] = true
			list = append(list, text)
			res.Guardrails = append(res.Guardrails, l.ProposalID)
			added = true
		}
	}
	return list, added
}

// mergeGuardrails adds the new lines to the singleton (creating it when
// absent) and replaces the disclaimer when one is given. It writes only when
// something changed.
func mergeGuardrails(ctx context.Context, tx bun.Tx, in DistilledBrand, now time.Time, res *DistilledResult) (bool, error) {
	if len(in.MayClaim) == 0 && len(in.NeverClaim) == 0 && len(in.BannedWords) == 0 && in.Disclaimer == nil {
		return false, nil
	}
	g := new(models.BrandGuardrails)
	err := tx.NewSelect().Model(g).Limit(1).Scan(ctx)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		id, err := models.NewID()
		if err != nil {
			return false, err
		}
		g = &models.BrandGuardrails{ID: id, MayClaim: models.StringSlice{}, NeverClaim: models.StringSlice{}, BannedWords: models.StringSlice{}, CreatedAt: now}
	default:
		return false, err
	}
	var a, b, c bool
	g.MayClaim, a = mergeList(g.MayClaim, in.MayClaim, brandcheck.MaxStatements, false, res)
	g.NeverClaim, b = mergeList(g.NeverClaim, in.NeverClaim, brandcheck.MaxStatements, false, res)
	g.BannedWords, c = mergeList(g.BannedWords, in.BannedWords, brandcheck.MaxBannedWords, true, res)
	changed := a || b || c
	if d := in.Disclaimer; d != nil {
		text := strings.TrimSpace(d.Text)
		if text == "" || lineKey(text) == lineKey(g.Disclaimer) {
			res.Skipped = append(res.Skipped, SkippedItem{ProposalID: d.ProposalID, Reason: SkipDuplicate})
		} else {
			g.Disclaimer = text
			res.Guardrails = append(res.Guardrails, d.ProposalID)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	g.UpdatedAt = now
	row := *g
	row.Facts = models.StringSlice{}
	_, err = tx.NewInsert().Model(&row).
		On("CONFLICT (tenant_id) DO UPDATE").
		Set("may_claim = EXCLUDED.may_claim").
		Set("never_claim = EXCLUDED.never_claim").
		Set("banned_words = EXCLUDED.banned_words").
		Set("disclaimer = EXCLUDED.disclaimer").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err == nil, err
}

// addFacts inserts the facts the ledger has room for, skipping statements it
// already holds. It returns how many it added.
func addFacts(ctx context.Context, tx bun.Tx, facts []DistilledFact, author *string, now time.Time, res *DistilledResult) (int, error) {
	if len(facts) == 0 {
		return 0, nil
	}
	current, err := listFacts(ctx, tx)
	if err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(current))
	for _, f := range current {
		seen[lineKey(f.Statement)] = true
	}
	name, err := authorName(ctx, tx, author)
	if err != nil {
		return 0, err
	}
	count, added := len(current), 0
	for _, f := range facts {
		row := f.Fact
		key := lineKey(row.Statement)
		if key == "" || seen[key] {
			res.Skipped = append(res.Skipped, SkippedItem{ProposalID: f.ProposalID, Reason: SkipDuplicate})
			continue
		}
		if count >= MaxBrandFacts {
			res.Skipped = append(res.Skipped, SkippedItem{ProposalID: f.ProposalID, Reason: SkipLimit})
			continue
		}
		if err := stamp(&row.ID, &row.CreatedAt, &row.UpdatedAt, now); err != nil {
			return added, err
		}
		row.CreatedBy, row.CreatedByName = author, name
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return added, err
		}
		seen[key] = true
		count++
		added++
		res.Facts[f.ProposalID] = row.ID
	}
	return added, nil
}
