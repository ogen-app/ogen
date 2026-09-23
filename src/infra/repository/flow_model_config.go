package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// FlowModelConfigRepository persists the (tier, flow, slot) -> model assignments
// (CON-308). A nil tierID addresses the GLOBAL-DEFAULT scope; a non-nil tierID
// addresses that tier's override. The resolver loads the full set via List and
// serves it from an in-memory snapshot; GetByScope/Upsert/Delete back the gRPC
// admin writes.
type FlowModelConfigRepository interface {
	// List returns every assignment (global defaults + tier overrides), ordered
	// deterministically so the resolver snapshot is stable.
	List(ctx context.Context) ([]models.FlowModelConfig, error)
	// GetByScope returns the single assignment for a scope, or sql.ErrNoRows.
	// A nil tierID selects the global-default row (tier_id IS NULL).
	GetByScope(ctx context.Context, tierID *string, flowKey, slotKey string) (*models.FlowModelConfig, error)
	// Upsert inserts or replaces the assignment for a (scope, flow, slot). The
	// id is server-minted on first insert and preserved on update.
	Upsert(ctx context.Context, cfg *models.FlowModelConfig) error
	// Delete removes the assignment for a scope, reporting whether a row existed.
	// A nil tierID would delete the global default — callers must guard against
	// that (a global default must always exist); this method does not.
	Delete(ctx context.Context, tierID *string, flowKey, slotKey string) (bool, error)
}

type flowModelConfigRepository struct {
	db *bun.DB
}

// NewFlowModelConfigRepository returns a Bun-backed repository.
func NewFlowModelConfigRepository(db *bun.DB) FlowModelConfigRepository {
	return &flowModelConfigRepository{db: db}
}

func (r *flowModelConfigRepository) List(ctx context.Context) ([]models.FlowModelConfig, error) {
	var rows []models.FlowModelConfig
	if err := r.db.NewSelect().
		Model(&rows).
		Order("flow_key", "slot_key").
		Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *flowModelConfigRepository) GetByScope(ctx context.Context, tierID *string, flowKey, slotKey string) (*models.FlowModelConfig, error) {
	cfg := new(models.FlowModelConfig)
	q := r.db.NewSelect().
		Model(cfg).
		Where("fmc.flow_key = ?", flowKey).
		Where("fmc.slot_key = ?", slotKey)
	if tierID == nil {
		q = q.Where("fmc.tier_id IS NULL")
	} else {
		q = q.Where("fmc.tier_id = ?", *tierID)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (r *flowModelConfigRepository) Upsert(ctx context.Context, cfg *models.FlowModelConfig) error {
	if cfg.ID == "" {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		cfg.ID = id
	}
	cfg.UpdatedAt = time.Now().UTC()
	// Conflict target matches ux_flow_model_config (the COALESCE expression index),
	// so a repeat write to the same scope updates the model in place rather than
	// minting a second row.
	_, err := r.db.NewInsert().
		Model(cfg).
		On("CONFLICT (COALESCE(tier_id, ''), flow_key, slot_key) DO UPDATE").
		Set("model_id = EXCLUDED.model_id").
		Set("updated_at = EXCLUDED.updated_at").
		Set("updated_by = EXCLUDED.updated_by").
		Exec(ctx)
	return err
}

func (r *flowModelConfigRepository) Delete(ctx context.Context, tierID *string, flowKey, slotKey string) (bool, error) {
	q := r.db.NewDelete().
		Model((*models.FlowModelConfig)(nil)).
		Where("flow_key = ?", flowKey).
		Where("slot_key = ?", slotKey)
	if tierID == nil {
		q = q.Where("tier_id IS NULL")
	} else {
		q = q.Where("tier_id = ?", *tierID)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
