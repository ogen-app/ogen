package repository

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// IdeaListFilter narrows IdeaRepository.List. The zero value lists every idea in
// the workspace. CampaignID wins over Unfiled when both are set.
type IdeaListFilter struct {
	// CampaignID keeps only ideas attached to this (live) campaign.
	CampaignID string
	// Unfiled keeps only workspace-wide ideas (no live campaign).
	Unfiled bool
	// Origin keeps only ideas of this origin when set.
	Origin *models.IdeaOrigin
	// AssetID keeps only ideas an analysis of this asset produced.
	AssetID string
}

// IdeaRepository persists the workspace Ideas backlog. Every query
// runs through bun's Model API so the TenantScoped hooks scope tenant_id.
//
// Reads resolve campaign_id through a LEFT JOIN on live campaigns: an idea whose
// campaign has since been soft-deleted reads back with a null campaign_id, so
// the UI never links to a campaign that is gone.
type IdeaRepository interface {
	// List returns every idea the filter keeps, oldest first.
	List(ctx context.Context, filter IdeaListFilter) ([]models.Idea, error)
	// Query reads one page of the ideas list with its counts. It returns
	// sql.ErrNoRows when the campaign scope names no live campaign, and
	// ErrInvalidCursor for a cursor of another sort or grouping.
	Query(ctx context.Context, q models.IdeaQuery) (*models.IdeaQueryResult, error)
	// CountViews counts each query's match and its waiting ideas, in one
	// round trip.
	CountViews(ctx context.Context, qs []models.IdeaQuery) ([]models.IdeaViewCounts, error)
	// QueryIDs returns up to limit ids of the ideas q matches and the size of
	// the whole match; sql.ErrNoRows for an unknown campaign scope.
	QueryIDs(ctx context.Context, q models.IdeaQuery, limit int) ([]string, int, error)
	// DeleteMany hard-deletes the ideas of ids in one statement and returns
	// those it deleted.
	DeleteMany(ctx context.Context, ids []string) ([]string, error)
	// GetByID returns sql.ErrNoRows for an unknown or cross-tenant id.
	GetByID(ctx context.Context, id string) (*models.Idea, error)
	Create(ctx context.Context, idea *models.Idea) error
	// Update writes only the named columns plus updated_at, so concurrent edits
	// to different fields don't clobber each other. Returns false when no row
	// matched.
	Update(ctx context.Context, idea *models.Idea, columns ...string) (bool, error)
	Delete(ctx context.Context, id string) (bool, error)
	// LiveCampaignExists reports whether campaignID names a campaign in the
	// caller's tenant that is not soft-deleted (archived counts as live).
	LiveCampaignExists(ctx context.Context, campaignID string) (bool, error)
}

type ideaRepository struct {
	db *bun.DB
}

func NewIdeaRepository(db *bun.DB) IdeaRepository {
	return &ideaRepository{db: db}
}

// selectIdeas builds the shared read: every idea column, with campaign_id taken
// from the joined live campaign rather than the stored FK.
func (r *ideaRepository) selectIdeas(model any) *bun.SelectQuery {
	return r.db.NewSelect().
		Model(model).
		ColumnExpr("i.id, i.title, i.note, i.verdict, i.remind_at, i.decided_at, i.decided_by").
		ColumnExpr("i.origin, i.asset_analysis_id, i.distill_run_id, i.source_asset_id, i.angle, i.rationale, i.sources").
		ColumnExpr("i.fit_score, i.fit_breakdown, i.fit_rubric_version").
		ColumnExpr("i.created_by, i.created_by_name, i.created_at, i.updated_at").
		ColumnExpr("c.id AS campaign_id").
		Join("LEFT JOIN campaigns AS c ON c.id = i.campaign_id AND c.deleted_at IS NULL")
}

func (r *ideaRepository) List(ctx context.Context, filter IdeaListFilter) ([]models.Idea, error) {
	ideas := []models.Idea{}
	q := r.selectIdeas(&ideas)
	switch {
	case filter.CampaignID != "":
		q = q.Where("c.id = ?", filter.CampaignID)
	case filter.Unfiled:
		q = q.Where("c.id IS NULL")
	}
	if filter.Origin != nil {
		q = q.Where("i.origin = ?", *filter.Origin)
	}
	if filter.AssetID != "" {
		q = q.Where("i.source_asset_id = ?", filter.AssetID)
	}
	if err := q.OrderExpr("i.created_at ASC, i.id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydrate(ctx, ideas); err != nil {
		return nil, err
	}
	return ideas, nil
}

// hydrate fills the derived fields of ideas read back: the author, and the
// source asset while it exists, with the campaign lens the idea was read in.
func (r *ideaRepository) hydrate(ctx context.Context, ideas []models.Idea) error {
	var assetIDs []string
	for i := range ideas {
		ideas[i].DeriveAuthor()
		if id := ideas[i].SourceAssetID; id != nil {
			assetIDs = append(assetIDs, *id)
		}
	}
	if len(assetIDs) == 0 {
		return nil
	}
	assets, err := r.sourceAssets(ctx, assetIDs)
	if err != nil {
		return err
	}
	readings, err := r.readings(ctx, ideas)
	if err != nil {
		return err
	}
	for i := range ideas {
		id := ideas[i].SourceAssetID
		if id == nil {
			continue
		}
		if a, ok := assets[*id]; ok {
			a.Reading = readings[ideas[i].ID]
			ideas[i].SourceAsset = &a
		}
	}
	return nil
}

// sourceAssets reads the source line of each asset, keyed by id.
func (r *ideaRepository) sourceAssets(ctx context.Context, ids []string) (map[string]models.IdeaSourceAsset, error) {
	slices.Sort(ids)
	var rows []struct {
		ID         string           `bun:"id"`
		Title      string           `bun:"title"`
		Kind       models.AssetKind `bun:"kind"`
		PageCount  *int             `bun:"page_count"`
		DurationMS *int64           `bun:"duration_ms"`
		SourceURL  *string          `bun:"source_url"`
	}
	if err := r.db.NewSelect().
		Model((*models.Asset)(nil)).
		ColumnExpr("a.id, a.title, a.source_url, af.page_count, nullif(af.duration_ms, 0) AS duration_ms").
		ColumnExpr(assetKindSQL+" AS kind").
		Join("LEFT JOIN asset_files AS af ON af.asset_id = a.id").
		Where("a.id IN (?)", bun.List(slices.Compact(ids))).
		Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]models.IdeaSourceAsset, len(rows))
	for _, row := range rows {
		a := models.IdeaSourceAsset{ID: row.ID, Title: row.Title, Kind: row.Kind, PageCount: row.PageCount, DurationMS: row.DurationMS}
		if row.Kind == models.AssetKindWeb && row.SourceURL != nil {
			a.Domain = webDomain(*row.SourceURL)
		}
		out[row.ID] = a
	}
	return out, nil
}

// webDomain is a URL's host without "www.", nil when it has none.
func webDomain(raw string) *string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return nil
	}
	return new(strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."))
}

// readings resolves the campaign lens each analysis idea was read in, keyed by
// idea id: the campaign of its analysis, or once the analysis is gone the
// campaign its fit breakdown names. Workspace-lens ideas have none.
func (r *ideaRepository) readings(ctx context.Context, ideas []models.Idea) (map[string]*models.IdeaReading, error) {
	var analysisIDs []string
	for _, idea := range ideas {
		if idea.AssetAnalysisID != nil {
			analysisIDs = append(analysisIDs, *idea.AssetAnalysisID)
		}
	}
	lensCampaign := map[string]string{}
	if len(analysisIDs) > 0 {
		var rows []struct {
			ID         string `bun:"id"`
			CampaignID string `bun:"campaign_id"`
		}
		if err := r.db.NewSelect().
			Model((*models.AssetAnalysis)(nil)).
			Column("aa.id", "aa.campaign_id").
			Where("aa.id IN (?)", bun.List(analysisIDs)).
			Where("aa.campaign_id IS NOT NULL").
			Scan(ctx, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			lensCampaign[row.ID] = row.CampaignID
		}
	}
	campaignOf := map[string]string{}
	var campaignIDs []string
	for _, idea := range ideas {
		var cid string
		switch {
		case idea.AssetAnalysisID != nil && lensCampaign[*idea.AssetAnalysisID] != "":
			cid = lensCampaign[*idea.AssetAnalysisID]
		case idea.AssetAnalysisID == nil && idea.FitBreakdown != nil && idea.FitBreakdown.CampaignID != nil:
			cid = *idea.FitBreakdown.CampaignID
		}
		if cid != "" {
			campaignOf[idea.ID] = cid
			campaignIDs = append(campaignIDs, cid)
		}
	}
	if len(campaignIDs) == 0 {
		return nil, nil
	}
	slices.Sort(campaignIDs)
	var names []struct {
		ID   string `bun:"id"`
		Name string `bun:"name"`
	}
	if err := r.db.NewSelect().
		Model((*models.Campaign)(nil)).
		Column("c.id", "c.name").
		Where("c.id IN (?)", bun.List(slices.Compact(campaignIDs))).
		Scan(ctx, &names); err != nil {
		return nil, err
	}
	nameOf := make(map[string]string, len(names))
	for _, n := range names {
		nameOf[n.ID] = n.Name
	}
	out := make(map[string]*models.IdeaReading, len(campaignOf))
	for ideaID, cid := range campaignOf {
		if name, ok := nameOf[cid]; ok {
			out[ideaID] = &models.IdeaReading{CampaignID: cid, CampaignName: name}
		}
	}
	return out, nil
}

func (r *ideaRepository) GetByID(ctx context.Context, id string) (*models.Idea, error) {
	idea := new(models.Idea)
	if err := r.selectIdeas(idea).Where("i.id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	one := []models.Idea{*idea}
	if err := r.hydrate(ctx, one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

func (r *ideaRepository) Create(ctx context.Context, idea *models.Idea) error {
	_, err := r.db.NewInsert().Model(idea).Exec(ctx)
	return err
}

func (r *ideaRepository) Update(ctx context.Context, idea *models.Idea, columns ...string) (bool, error) {
	res, err := r.db.NewUpdate().
		Model(idea).
		Column(append(columns, "updated_at")...).
		WherePK().
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *ideaRepository) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.db.NewDelete().
		Model((*models.Idea)(nil)).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteMany hard-deletes the ideas of ids in one statement and returns the
// ids it deleted; an unknown or cross-tenant id is skipped.
func (r *ideaRepository) DeleteMany(ctx context.Context, ids []string) ([]string, error) {
	deleted := []string{}
	if len(ids) == 0 {
		return deleted, nil
	}
	_, err := r.db.NewDelete().
		Model((*models.Idea)(nil)).
		Where("id IN (?)", bun.List(ids)).
		Returning("id").
		Exec(ctx, &deleted)
	return deleted, err
}

// LiveCampaignExists deliberately uses Scan, not Exists: bun's Exists skips the
// BeforeSelect hook, so TenantScoped would not add the tenant predicate and a
// campaign from another tenant would pass.
func (r *ideaRepository) LiveCampaignExists(ctx context.Context, campaignID string) (bool, error) {
	var id string
	err := r.db.NewSelect().
		Model((*models.Campaign)(nil)).
		Column("c.id").
		Where("c.id = ?", campaignID).
		Where("c.deleted_at IS NULL").
		Limit(1).
		Scan(ctx, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
