package repository

import (
	"context"
	"slices"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AutoPublishAllowlistRepository is the persistence surface for the
// workspace-scoped auto-publish allowlist. Set is a
// transactional full-replace — callers send the desired final set, not
// a diff. Contains is the predicate the future scheduler will call
// when deciding whether to auto-dispatch a Scheduled post.
//
// Z-membership (each platform_id ∈ zernio.SupportedPlatforms) is
// validated in the handler layer; the repository stays as a dumb
// persistence boundary so it can't import publishers/zernio (which
// itself depends on repository).
type AutoPublishAllowlistRepository interface {
	List(ctx context.Context) ([]string, error)
	Set(ctx context.Context, platformIDs []string) error
	Contains(ctx context.Context, platformID string) (bool, error)
	// ReplaceIf sets the list to `to` only while it is exactly `from` (in
	// any order), on db (nil: the pool); false when it was changed since.
	// It locks the workspace's rows, so two replaces don't both pass.
	ReplaceIf(ctx context.Context, db bun.IDB, from, to []string) (bool, error)
}

type autoPublishAllowlistRepository struct {
	db *bun.DB
}

func NewAutoPublishAllowlistRepository(db *bun.DB) AutoPublishAllowlistRepository {
	return &autoPublishAllowlistRepository{db: db}
}

func (r *autoPublishAllowlistRepository) List(ctx context.Context) ([]string, error) {
	var rows []models.AutoPublishAllowlistEntry
	if err := r.db.NewSelect().Model(&rows).
		OrderExpr("platform_id ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.PlatformID)
	}
	return out, nil
}

func (r *autoPublishAllowlistRepository) Set(ctx context.Context, platformIDs []string) error {
	// Dedupe before opening the tx — the table's PK would reject
	// duplicates anyway, but rejecting up-front avoids a wasted
	// DELETE round-trip when the caller sends repeats.
	seen := make(map[string]struct{}, len(platformIDs))
	for _, id := range platformIDs {
		seen[id] = struct{}{}
	}

	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().
			Model((*models.AutoPublishAllowlistEntry)(nil)).
			Where("1 = 1").
			Exec(ctx); err != nil {
			return err
		}
		if len(seen) == 0 {
			return nil
		}
		rows := make([]models.AutoPublishAllowlistEntry, 0, len(seen))
		for id := range seen {
			rows = append(rows, models.AutoPublishAllowlistEntry{PlatformID: id})
		}
		_, err := tx.NewInsert().Model(&rows).Exec(ctx)
		return err
	})
}

func (r *autoPublishAllowlistRepository) Contains(ctx context.Context, platformID string) (bool, error) {
	count, err := r.db.NewSelect().
		Model((*models.AutoPublishAllowlistEntry)(nil)).
		Where("platform_id = ?", platformID).
		Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *autoPublishAllowlistRepository) ReplaceIf(ctx context.Context, db bun.IDB, from, to []string) (bool, error) {
	if db == nil {
		db = r.db
	}
	replaced := false
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var cur []string
		if err := tx.NewSelect().Model((*models.AutoPublishAllowlistEntry)(nil)).
			Column("platform_id").For("UPDATE").Scan(ctx, &cur); err != nil {
			return err
		}
		slices.Sort(cur)
		want := slices.Clone(from)
		slices.Sort(want)
		if !slices.Equal(cur, slices.Compact(want)) {
			return nil
		}
		if _, err := tx.NewDelete().Model((*models.AutoPublishAllowlistEntry)(nil)).Where("1 = 1").Exec(ctx); err != nil {
			return err
		}
		next := slices.Compact(slices.Sorted(slices.Values(to)))
		if len(next) > 0 {
			rows := make([]models.AutoPublishAllowlistEntry, len(next))
			for i, id := range next {
				rows[i] = models.AutoPublishAllowlistEntry{PlatformID: id}
			}
			if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
				return err
			}
		}
		replaced = true
		return nil
	})
	return replaced, err
}
