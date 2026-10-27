package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AssetAnalysisRepository persists asset analyses, one row per lens: the
// workspace lens (nil campaign) and one lens per campaign the asset is in.
// Every query runs through the bun Model API so TenantScoped scopes it.
//
// A campaign lens whose campaign is soft-deleted stays in the table until the
// campaign is purged but is hidden from every read.
type AssetAnalysisRepository interface {
	// Ensure inserts a pending lens when none exists and returns the lens.
	Ensure(ctx context.Context, db bun.IDB, assetID string, campaignID *string) (*models.AssetAnalysis, error)
	// GetLens returns sql.ErrNoRows for an absent lens or a deleted campaign.
	GetLens(ctx context.Context, assetID string, campaignID *string) (*models.AssetAnalysis, error)
	// LockLens reads the lens FOR UPDATE inside tx; sql.ErrNoRows when absent.
	LockLens(ctx context.Context, tx bun.Tx, assetID string, campaignID *string) (*models.AssetAnalysis, error)
	// ListByAsset returns the workspace lens first, then the campaign lenses.
	ListByAsset(ctx context.Context, assetID string) ([]models.AssetAnalysis, error)
	// ListByCampaign returns the campaign's lenses, most relevant first.
	ListByCampaign(ctx context.Context, campaignID string) ([]models.AssetAnalysis, error)
	// LensDigests maps each asset id to the status and gist of one lens, the
	// workspace lens for a nil campaignID, in one query; assets without that
	// lens are absent.
	LensDigests(ctx context.Context, assetIDs []string, campaignID *string) (map[string]LensDigest, error)
	// SetStatus writes status with its failure code and reason (both empty
	// unless failed or skipped).
	SetStatus(ctx context.Context, db bun.IDB, id string, status models.AssetAnalysisStatus, code, reason string) error
	// SaveResult writes a finished analysis and replaces its own untriaged
	// ideas with ideas. Triaged ideas and ideas a distill run added are never
	// touched. It must run inside the tx that locked the lens.
	SaveResult(ctx context.Context, tx bun.Tx, a *models.AssetAnalysis, ideas []models.Idea) error
	// DeleteLens removes a campaign lens and its untriaged ideas.
	DeleteLens(ctx context.Context, db bun.IDB, assetID, campaignID string) error
	// DeleteUntriagedIdeasForAsset removes the untriaged ideas of every lens
	// of the asset, ahead of the asset delete that cascades the lenses.
	DeleteUntriagedIdeasForAsset(ctx context.Context, db bun.IDB, assetID string) error
	// CampaignIDsWithAsset lists the live campaigns the asset is attached to.
	CampaignIDsWithAsset(ctx context.Context, assetID string) ([]string, error)
	// ClaimRunSlot gives the lens's detached run slot to token unless a run
	// holds it with a heartbeat at or after staleBefore. It reports whether
	// the slot was claimed.
	ClaimRunSlot(ctx context.Context, id, token string, staleBefore time.Time) (bool, error)
	// HeartbeatRunSlot refreshes the heartbeat of token's run. It reports
	// false when token no longer holds an unfinished slot: another run took it.
	HeartbeatRunSlot(ctx context.Context, id, token string) (bool, error)
	// FinishRunSlot records the outcome of token's run: errText empty on
	// success, code naming the failure.
	FinishRunSlot(ctx context.Context, id, token, errText, code string) error
	// TouchLens records activity of the lens's analysis job.
	TouchLens(ctx context.Context, id string) error
	// RecordRun logs one run of the analysis against the monthly cap and
	// returns the run's id.
	RecordRun(ctx context.Context, analysisID string) (string, error)
	// DeleteRun releases a recorded run that never reached a result.
	DeleteRun(ctx context.Context, id string) error
	// CountRunsSince counts the tenant's runs at or after since.
	CountRunsSince(ctx context.Context, since time.Time) (int64, error)
	// ListMissingLenses lists, across every active tenant, the lenses that
	// should exist but don't: analysable assets without a workspace lens and
	// campaign memberships without a campaign lens. It also lists stranded
	// lenses: pending for over pendingGrace with no analysis job stepping on
	// them for as long, because queueing failed after the row was made. At most perTenant per
	// tenant, oldest asset first.
	ListMissingLenses(ctx context.Context, perTenant int, pendingGrace time.Duration) ([]MissingLens, error)
}

// MissingLens names a lens with no row yet; a nil CampaignID is the
// workspace lens.
type MissingLens struct {
	TenantID   string  `bun:"tenant_id"`
	AssetID    string  `bun:"asset_id"`
	CampaignID *string `bun:"campaign_id"`
}

type assetAnalysisRepository struct {
	db *bun.DB
}

func NewAssetAnalysisRepository(db *bun.DB) AssetAnalysisRepository {
	return &assetAnalysisRepository{db: db}
}

// whereLens narrows q to one lens of an asset.
func whereLens(q *bun.SelectQuery, assetID string, campaignID *string) *bun.SelectQuery {
	q = q.Where("aa.asset_id = ?", assetID)
	if campaignID == nil {
		return q.Where("aa.campaign_id IS NULL")
	}
	return q.Where("aa.campaign_id = ?", *campaignID)
}

// selectLive reads lenses with their campaign name, dropping lenses of
// soft-deleted campaigns.
func (r *assetAnalysisRepository) selectLive(db bun.IDB, model any) *bun.SelectQuery {
	return db.NewSelect().
		Model(model).
		ColumnExpr("aa.*").
		ColumnExpr("c.name AS campaign_name").
		Join("LEFT JOIN campaigns AS c ON c.id = aa.campaign_id").
		Where("aa.campaign_id IS NULL OR c.deleted_at IS NULL")
}

func (r *assetAnalysisRepository) Ensure(ctx context.Context, db bun.IDB, assetID string, campaignID *string) (*models.AssetAnalysis, error) {
	id, err := models.NewID()
	if err != nil {
		return nil, err
	}
	a := &models.AssetAnalysis{
		ID:         id,
		AssetID:    assetID,
		CampaignID: campaignID,
		Status:     models.AssetAnalysisStatusPending,
	}
	if _, err := db.NewInsert().
		Model(a).
		On("CONFLICT (asset_id, (COALESCE(campaign_id, ''))) DO NOTHING").
		Exec(ctx); err != nil {
		return nil, err
	}
	existing := new(models.AssetAnalysis)
	if err := whereLens(db.NewSelect().Model(existing), assetID, campaignID).Scan(ctx); err != nil {
		return nil, err
	}
	return existing, nil
}

func (r *assetAnalysisRepository) GetLens(ctx context.Context, assetID string, campaignID *string) (*models.AssetAnalysis, error) {
	a := new(models.AssetAnalysis)
	if err := whereLens(r.selectLive(r.db, a), assetID, campaignID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return a, nil
}

func (r *assetAnalysisRepository) LockLens(ctx context.Context, tx bun.Tx, assetID string, campaignID *string) (*models.AssetAnalysis, error) {
	a := new(models.AssetAnalysis)
	if err := whereLens(tx.NewSelect().Model(a), assetID, campaignID).For("UPDATE").Scan(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *assetAnalysisRepository) ListByAsset(ctx context.Context, assetID string) ([]models.AssetAnalysis, error) {
	out := []models.AssetAnalysis{}
	err := r.selectLive(r.db, &out).
		Where("aa.asset_id = ?", assetID).
		OrderExpr("aa.campaign_id IS NOT NULL, c.name ASC, aa.id ASC").
		Scan(ctx)
	return out, err
}

func (r *assetAnalysisRepository) ListByCampaign(ctx context.Context, campaignID string) ([]models.AssetAnalysis, error) {
	out := []models.AssetAnalysis{}
	err := r.selectLive(r.db, &out).
		Where("aa.campaign_id = ?", campaignID).
		OrderExpr("(aa.result->'relevance'->>'score')::int DESC NULLS LAST, aa.id ASC").
		Scan(ctx)
	return out, err
}

// LensDigest is what the asset list shows of a lens.
type LensDigest struct {
	Status models.AssetAnalysisStatus
	Gist   string
	// SummaryHead is the start of the summary, read only when Gist is empty
	// so the caller can derive a sentence from it.
	SummaryHead string
}

// summaryHeadRunes bounds the summary read for the gist fallback. It matches
// the flow's summary cap, so a summary that opens with long headings and
// lists still reaches its first prose paragraph.
const summaryHeadRunes = 4000

func (r *assetAnalysisRepository) LensDigests(ctx context.Context, assetIDs []string, campaignID *string) (map[string]LensDigest, error) {
	out := make(map[string]LensDigest, len(assetIDs))
	if len(assetIDs) == 0 {
		return out, nil
	}
	var rows []struct {
		AssetID     string                     `bun:"asset_id"`
		Status      models.AssetAnalysisStatus `bun:"status"`
		Gist        string                     `bun:"gist"`
		SummaryHead string                     `bun:"summary_head"`
	}
	q := r.db.NewSelect().
		Model((*models.AssetAnalysis)(nil)).
		Column("aa.asset_id", "aa.status", "aa.gist").
		ColumnExpr("CASE WHEN aa.gist = '' THEN left(aa.summary, ?) ELSE '' END AS summary_head", summaryHeadRunes).
		Where("aa.asset_id IN (?)", bun.List(assetIDs))
	if campaignID == nil {
		q = q.Where("aa.campaign_id IS NULL")
	} else {
		q = q.Where("aa.campaign_id = ?", *campaignID)
	}
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.AssetID] = LensDigest{Status: row.Status, Gist: row.Gist, SummaryHead: row.SummaryHead}
	}
	return out, nil
}

func (r *assetAnalysisRepository) SetStatus(ctx context.Context, db bun.IDB, id string, status models.AssetAnalysisStatus, code, reason string) error {
	_, err := db.NewUpdate().
		Model((*models.AssetAnalysis)(nil)).
		Set("status = ?", status).
		Set("failure_code = ?", code).
		Set("failure_reason = ?", reason).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", id).
		Exec(ctx)
	return err
}

func (r *assetAnalysisRepository) SaveResult(ctx context.Context, tx bun.Tx, a *models.AssetAnalysis, ideas []models.Idea) error {
	a.UpdatedAt = time.Now().UTC()
	if _, err := tx.NewUpdate().
		Model(a).
		Column("status", "strategy", "gist", "summary", "result", "language", "partial_source", "truncated",
			"source_tokens", "context_fingerprint", "model_id", "failure_code", "failure_reason",
			"analyzed_at", "updated_at").
		WherePK().
		Exec(ctx); err != nil {
		return err
	}
	// Ideas a distill run added were asked for, so a re-run leaves them.
	if err := deleteUntriagedIdeas(ctx, tx, "asset_analysis_id = ? AND distill_run_id IS NULL", a.ID); err != nil {
		return err
	}
	if len(ideas) == 0 {
		return nil
	}
	for i := range ideas {
		if ideas[i].SourceAssetID == nil {
			ideas[i].SourceAssetID = new(a.AssetID)
		}
	}
	_, err := tx.NewInsert().Model(&ideas).Exec(ctx)
	return err
}

func (r *assetAnalysisRepository) DeleteLens(ctx context.Context, db bun.IDB, assetID, campaignID string) error {
	lensIDs := db.NewSelect().
		Model((*models.AssetAnalysis)(nil)).
		Column("aa.id").
		Where("aa.asset_id = ?", assetID).
		Where("aa.campaign_id = ?", campaignID)
	if err := deleteUntriagedIdeas(ctx, db, "asset_analysis_id IN (?)", lensIDs); err != nil {
		return err
	}
	_, err := db.NewDelete().
		Model((*models.AssetAnalysis)(nil)).
		Where("asset_id = ?", assetID).
		Where("campaign_id = ?", campaignID).
		Exec(ctx)
	return err
}

func (r *assetAnalysisRepository) DeleteUntriagedIdeasForAsset(ctx context.Context, db bun.IDB, assetID string) error {
	lensIDs := db.NewSelect().
		Model((*models.AssetAnalysis)(nil)).
		Column("aa.id").
		Where("aa.asset_id = ?", assetID)
	return deleteUntriagedIdeas(ctx, db, "asset_analysis_id IN (?)", lensIDs)
}

func (r *assetAnalysisRepository) CampaignIDsWithAsset(ctx context.Context, assetID string) ([]string, error) {
	needle, err := json.Marshal([]string{assetID})
	if err != nil {
		return nil, err
	}
	var ids []string
	err = r.db.NewSelect().
		Model((*models.Campaign)(nil)).
		Column("c.id").
		Where("c.deleted_at IS NULL").
		Where("c.asset_ids @> ?::jsonb", string(needle)).
		OrderExpr("c.id").
		Scan(ctx, &ids)
	return ids, err
}

func (r *assetAnalysisRepository) RecordRun(ctx context.Context, analysisID string) (string, error) {
	id, err := models.NewID()
	if err != nil {
		return "", err
	}
	if _, err := r.db.NewInsert().Model(&models.AssetAnalysisRun{ID: id, AnalysisID: analysisID}).Exec(ctx); err != nil {
		return "", err
	}
	return id, nil
}

func (r *assetAnalysisRepository) DeleteRun(ctx context.Context, id string) error {
	_, err := r.db.NewDelete().Model((*models.AssetAnalysisRun)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (r *assetAnalysisRepository) ClaimRunSlot(ctx context.Context, id, token string, staleBefore time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.AssetAnalysis)(nil)).
		Set("run_token = ?", token).
		Set("run_heartbeat_at = now()").
		Set("run_done_at = NULL").
		Set("run_error = ''").
		Set("run_error_code = ''").
		Where("aa.id = ?", id).
		Where("aa.run_token IS NULL OR aa.run_done_at IS NOT NULL OR aa.run_heartbeat_at < ?", staleBefore).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *assetAnalysisRepository) HeartbeatRunSlot(ctx context.Context, id, token string) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.AssetAnalysis)(nil)).
		Set("run_heartbeat_at = now()").
		Where("aa.id = ? AND aa.run_token = ? AND aa.run_done_at IS NULL", id, token).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *assetAnalysisRepository) FinishRunSlot(ctx context.Context, id, token, errText, code string) error {
	_, err := r.db.NewUpdate().Model((*models.AssetAnalysis)(nil)).
		Set("run_done_at = now()").
		Set("run_error = ?", errText).
		Set("run_error_code = ?", code).
		Where("aa.id = ? AND aa.run_token = ?", id, token).
		Exec(ctx)
	return err
}

func (r *assetAnalysisRepository) TouchLens(ctx context.Context, id string) error {
	_, err := r.db.NewUpdate().Model((*models.AssetAnalysis)(nil)).
		Set("last_activity_at = now()").
		Where("aa.id = ?", id).
		Exec(ctx)
	return err
}

// CountRunsSince scans a count(*) select: bun's Count skips the BeforeSelect
// hook, so it would count every tenant's runs.
func (r *assetAnalysisRepository) CountRunsSince(ctx context.Context, since time.Time) (int64, error) {
	var n int64
	err := r.db.NewSelect().
		Model((*models.AssetAnalysisRun)(nil)).
		ColumnExpr("count(*)").
		Where("aar.created_at >= ?", since).
		Scan(ctx, &n)
	return n, err
}

// missingLensesSQL finds the absent and stranded lenses of active tenants. A
// NULL type is legacy markdown, analysable like models.IsAnalyzableAssetType
// says. A brand source never gets a workspace lens on its own (its ingest
// writes a skipped one). A stranded lens is a pending one whose analysis job
// hasn't stepped within the grace either: the job's steps stamp
// last_activity_at, so ogen never reads the job queue, which lives in
// river-service.
const missingLensesSQL = `
WITH live AS (
	SELECT a.tenant_id, a.id, a.created_at, a.origin
	FROM assets AS a
	JOIN tenants AS t ON t.id = a.tenant_id AND t.status = ? AND t.deleted_at IS NULL
	WHERE (a.type IS NULL OR a.type IN (?)) AND a.status IN (?)
), missing AS (
	SELECT l.tenant_id, l.id AS asset_id, NULL::text AS campaign_id, l.created_at
	FROM live AS l
	WHERE l.origin <> ? AND NOT EXISTS (
		SELECT 1 FROM asset_analyses AS aa WHERE aa.asset_id = l.id AND aa.campaign_id IS NULL)
	UNION ALL
	SELECT l.tenant_id, l.id, c.id, l.created_at
	FROM campaigns AS c
	CROSS JOIN LATERAL jsonb_array_elements_text(c.asset_ids) AS m(asset_id)
	JOIN live AS l ON l.id = m.asset_id AND l.tenant_id = c.tenant_id
	WHERE c.deleted_at IS NULL
		AND NOT EXISTS (
			SELECT 1 FROM asset_analyses AS aa WHERE aa.asset_id = l.id AND aa.campaign_id = c.id)
	UNION ALL
	SELECT l.tenant_id, l.id, aa.campaign_id, l.created_at
	FROM asset_analyses AS aa
	JOIN live AS l ON l.id = aa.asset_id
	LEFT JOIN campaigns AS c ON c.id = aa.campaign_id
	WHERE aa.status = ? AND aa.updated_at < ?
		AND (aa.campaign_id IS NULL OR c.deleted_at IS NULL)
		AND (aa.last_activity_at IS NULL OR aa.last_activity_at < ?)
)
SELECT tenant_id, asset_id, campaign_id FROM (
	SELECT missing.*, row_number() OVER (
		PARTITION BY tenant_id ORDER BY created_at, asset_id, campaign_id NULLS FIRST) AS n
	FROM missing
) AS ranked
WHERE n <= ?
ORDER BY tenant_id, n`

func (r *assetAnalysisRepository) ListMissingLenses(ctx context.Context, perTenant int, pendingGrace time.Duration) ([]MissingLens, error) {
	out := []MissingLens{}
	cutoff := time.Now().Add(-pendingGrace)
	err := r.db.NewRaw(missingLensesSQL,
		models.TenantStatusActive,
		bun.List(models.AnalyzableAssetTypes),
		bun.List(models.AnalyzableAssetStatuses),
		models.AssetOriginBrandDistill,
		models.AssetAnalysisStatusPending,
		cutoff,
		cutoff,
		perTenant,
	).Scan(ctx, &out)
	return out, err
}

// deleteUntriagedIdeas deletes the inbox (verdict NULL) ideas matching where.
func deleteUntriagedIdeas(ctx context.Context, db bun.IDB, where string, args ...any) error {
	_, err := db.NewDelete().
		Model((*models.Idea)(nil)).
		Where(where, args...).
		Where("verdict IS NULL").
		Exec(ctx)
	return err
}
