package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// AnnouncementAudience is the delivery input for ActiveForTenant (CON-230): the
// caller's active workspace resolved to its tier + group ids, plus the user
// membership whose dismissals hide banners.
type AnnouncementAudience struct {
	TenantID string
	TierID   string
	GroupIDs []string
	UserID   string
}

// AnnouncementForUser is one deliverable announcement plus whether THIS user has
// already clicked its CTA (so the banner can render a "seen" affordance).
type AnnouncementForUser struct {
	models.Announcement
	Clicked bool `json:"clicked"`
}

// AnnouncementListFilter narrows and pages the operator history read
// (ListAnnouncements). Empty Status means "any status". Paging is keyset over
// (created_at DESC, id DESC): pass the last row's (CreatedAt, ID) as the cursor.
type AnnouncementListFilter struct {
	Status          models.AnnouncementStatus
	Limit           int
	CursorCreatedAt time.Time
	CursorID        string
}

// AnnouncementRepository persists operator-authored announcements (CON-230): the
// tenant-facing delivery + per-user click/dismiss tracking, and the operator
// (Harbor gRPC) authoring / history / stats. Announcements are a GLOBAL table
// (not tenant-scoped), so — like TenantRepository — this repo is NOT routed
// through the tenant-scoped query layer.
type AnnouncementRepository interface {
	// --- tenant delivery + tracking (/api/announcements) ---

	// ActiveForTenant returns the announcements currently deliverable to the
	// given audience: published, within their [starts_at, ends_at) window,
	// matching targeting (all / tier / any group), and NOT dismissed by the
	// caller's user. Newest-published first.
	ActiveForTenant(ctx context.Context, a AnnouncementAudience) ([]AnnouncementForUser, error)
	// RecordClick upserts the (announcement, user) interaction, stamping
	// clicked_at on the first click (first-click wins). Returns false when the id
	// is unknown or not published (treated as not-found by the handler).
	RecordClick(ctx context.Context, announcementID, userID, tenantID string) (bool, error)
	// RecordDismiss upserts the (announcement, user) interaction, stamping
	// dismissed_at. Returns false when the id is unknown or not published.
	RecordDismiss(ctx context.Context, announcementID, userID, tenantID string) (bool, error)

	// --- operator authoring / history / stats (Harbor gRPC) ---

	// Create inserts a new announcement plus its targeting rows in one tx. Mints
	// the id when a.ID is empty and defaults status to draft.
	Create(ctx context.Context, a *models.Announcement, groupIDs, tierIDs []string) error
	// Update whole-resource-updates the editable fields (not status /
	// published_at) and replaces the targeting rows. Returns false if the id is
	// unknown.
	Update(ctx context.Context, a *models.Announcement, groupIDs, tierIDs []string) (bool, error)
	// Get loads one announcement hydrated with its target group/tier ids;
	// sql.ErrNoRows when absent.
	Get(ctx context.Context, id string) (*models.Announcement, error)
	// List returns the announcement history (optionally status-filtered), each
	// hydrated with its target ids, newest-first, keyset-paged.
	List(ctx context.Context, f AnnouncementListFilter) ([]models.Announcement, error)
	// SetStatus transitions an announcement's lifecycle. On the first move to
	// published it stamps published_at. Returns false if the id is unknown.
	SetStatus(ctx context.Context, id string, status models.AnnouncementStatus) (bool, error)
	// Delete removes a draft announcement. found reports whether the id exists;
	// deleted is true only when it was a draft (published/archived are retained
	// for history — archive instead).
	Delete(ctx context.Context, id string) (found, deleted bool, err error)
	// Stats computes the engagement rollup + eligible-audience denominator for one
	// announcement; sql.ErrNoRows when the id is unknown.
	Stats(ctx context.Context, id string) (models.AnnouncementStats, error)
	// StatsFor computes the same rollup for an already-loaded announcement (with
	// its targeting hydrated) — lets a list read reuse the rows it already fetched
	// instead of re-Getting each one.
	StatsFor(ctx context.Context, an *models.Announcement) (models.AnnouncementStats, error)
}

type announcementRepository struct {
	db *bun.DB
}

// NewAnnouncementRepository returns a Bun-backed AnnouncementRepository.
func NewAnnouncementRepository(db *bun.DB) AnnouncementRepository {
	return &announcementRepository{db: db}
}

func (r *announcementRepository) ActiveForTenant(ctx context.Context, a AnnouncementAudience) ([]AnnouncementForUser, error) {
	var rows []models.Announcement
	q := r.db.NewSelect().Model(&rows).
		Where("an.status = ?", models.AnnouncementStatusPublished).
		Where("(an.starts_at IS NULL OR an.starts_at <= now())").
		Where("(an.ends_at IS NULL OR an.ends_at > now())").
		// Not dismissed by this user.
		Where("NOT EXISTS (SELECT 1 FROM announcement_interactions ai WHERE ai.announcement_id = an.id AND ai.user_id = ? AND ai.dismissed_at IS NOT NULL)", a.UserID).
		// Targeting: everyone, OR this tenant's tier, OR any of its groups.
		WhereGroup(" AND ", func(sq *bun.SelectQuery) *bun.SelectQuery {
			sq = sq.WhereOr("an.target_all = TRUE").
				WhereOr("EXISTS (SELECT 1 FROM announcement_target_tiers att WHERE att.announcement_id = an.id AND att.tier_id = ?)", a.TierID)
			if len(a.GroupIDs) > 0 {
				sq = sq.WhereOr("EXISTS (SELECT 1 FROM announcement_target_groups atg WHERE atg.announcement_id = an.id AND atg.group_id IN (?))", bun.In(a.GroupIDs))
			}
			return sq
		})
	if err := q.OrderExpr("an.published_at DESC, an.id DESC").Scan(ctx); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []AnnouncementForUser{}, nil
	}

	// Which of these did this user already click? One extra query keeps the main
	// select simple; the set is small (the active banners).
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	var clickedIDs []string
	if err := r.db.NewSelect().Model((*models.AnnouncementInteraction)(nil)).
		Column("announcement_id").
		Where("ai.user_id = ?", a.UserID).
		Where("ai.announcement_id IN (?)", bun.In(ids)).
		Where("ai.clicked_at IS NOT NULL").
		Scan(ctx, &clickedIDs); err != nil {
		return nil, err
	}
	clicked := make(map[string]struct{}, len(clickedIDs))
	for _, id := range clickedIDs {
		clicked[id] = struct{}{}
	}

	out := make([]AnnouncementForUser, len(rows))
	for i := range rows {
		_, ok := clicked[rows[i].ID]
		out[i] = AnnouncementForUser{Announcement: rows[i], Clicked: ok}
	}
	return out, nil
}

func (r *announcementRepository) RecordClick(ctx context.Context, announcementID, userID, tenantID string) (bool, error) {
	return r.recordInteraction(ctx, announcementID, userID, tenantID, "clicked_at")
}

func (r *announcementRepository) RecordDismiss(ctx context.Context, announcementID, userID, tenantID string) (bool, error) {
	return r.recordInteraction(ctx, announcementID, userID, tenantID, "dismissed_at")
}

// recordInteraction upserts the one (announcement, user) row, stamping the given
// timestamp column on first occurrence (COALESCE keeps the earlier value) and
// leaving the other column untouched. The announcement must be published, else
// the caller treats the id as not-found.
func (r *announcementRepository) recordInteraction(ctx context.Context, announcementID, userID, tenantID, column string) (bool, error) {
	published, err := r.db.NewSelect().Model((*models.Announcement)(nil)).
		Where("an.id = ?", announcementID).
		Where("an.status = ?", models.AnnouncementStatusPublished).
		Exists(ctx)
	if err != nil {
		return false, err
	}
	if !published {
		return false, nil
	}

	id, err := models.NewID()
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	row := &models.AnnouncementInteraction{ID: id, AnnouncementID: announcementID, UserID: userID, TenantID: tenantID}
	switch column {
	case "clicked_at":
		row.ClickedAt = &now
	case "dismissed_at":
		row.DismissedAt = &now
	}
	// On conflict, keep the earliest timestamp for this column and leave the other
	// column (and thus a prior click/dismiss) alone. The existing row is qualified
	// by the model alias (bun inserts `... AS ai`); EXCLUDED is the proposed row.
	_, err = r.db.NewInsert().Model(row).
		On("CONFLICT (announcement_id, user_id) DO UPDATE").
		Set(column+" = COALESCE(ai."+column+", EXCLUDED."+column+")").
		Set("updated_at = now()").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *announcementRepository) Create(ctx context.Context, a *models.Announcement, groupIDs, tierIDs []string) error {
	if a.ID == "" {
		id, err := models.NewID()
		if err != nil {
			return err
		}
		a.ID = id
	}
	if a.Status == "" {
		a.Status = models.AnnouncementStatusDraft
	}
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(a).Exec(ctx); err != nil {
			return err
		}
		return insertAnnouncementTargets(ctx, tx, a.ID, groupIDs, tierIDs)
	})
}

func (r *announcementRepository) Update(ctx context.Context, a *models.Announcement, groupIDs, tierIDs []string) (bool, error) {
	a.UpdatedAt = time.Now().UTC()
	found := false
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model(a).
			// Whole-resource edit of the authored fields only; status + published_at
			// move via SetStatus, created_at is immutable.
			Column("title", "body", "image_url", "image_alt", "cta_label", "cta_url", "target_all", "starts_at", "ends_at", "updated_at").
			WherePK().
			Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // unknown id → found stays false
		}
		found = true
		// Replace the targeting sets wholesale.
		if _, err := tx.NewDelete().Model((*models.AnnouncementTargetGroup)(nil)).Where("announcement_id = ?", a.ID).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*models.AnnouncementTargetTier)(nil)).Where("announcement_id = ?", a.ID).Exec(ctx); err != nil {
			return err
		}
		return insertAnnouncementTargets(ctx, tx, a.ID, groupIDs, tierIDs)
	})
	return found, err
}

func (r *announcementRepository) Get(ctx context.Context, id string) (*models.Announcement, error) {
	a := new(models.Announcement)
	if err := r.db.NewSelect().Model(a).Where("an.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	if err := r.hydrateTargets(ctx, []*models.Announcement{a}); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *announcementRepository) List(ctx context.Context, f AnnouncementListFilter) ([]models.Announcement, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	var rows []models.Announcement
	q := r.db.NewSelect().Model(&rows)
	if f.Status != "" {
		q = q.Where("an.status = ?", f.Status)
	}
	// Keyset page: strictly-older than the cursor in (created_at DESC, id DESC).
	if !f.CursorCreatedAt.IsZero() && f.CursorID != "" {
		q = q.Where("(an.created_at, an.id) < (?, ?)", f.CursorCreatedAt, f.CursorID)
	}
	if err := q.OrderExpr("an.created_at DESC, an.id DESC").Limit(limit).Scan(ctx); err != nil {
		return nil, err
	}
	ptrs := make([]*models.Announcement, len(rows))
	for i := range rows {
		ptrs[i] = &rows[i]
	}
	if err := r.hydrateTargets(ctx, ptrs); err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *announcementRepository) SetStatus(ctx context.Context, id string, status models.AnnouncementStatus) (bool, error) {
	q := r.db.NewUpdate().Model((*models.Announcement)(nil)).
		Set("status = ?", status).
		Set("updated_at = ?", time.Now().UTC())
	if status == models.AnnouncementStatusPublished {
		// Stamp the first publish; a re-publish (already stamped) keeps the original.
		q = q.Set("published_at = COALESCE(published_at, now())")
	}
	res, err := q.Where("an.id = ?", id).Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *announcementRepository) Delete(ctx context.Context, id string) (found, deleted bool, err error) {
	var status string
	err = r.db.NewSelect().Model((*models.Announcement)(nil)).
		Column("status").
		Where("an.id = ?", id).
		Scan(ctx, &status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	if status != string(models.AnnouncementStatusDraft) {
		return true, false, nil // exists but retained (published/archived)
	}
	if _, err := r.db.NewDelete().Model((*models.Announcement)(nil)).Where("an.id = ?", id).Exec(ctx); err != nil {
		return true, false, err
	}
	return true, true, nil
}

func (r *announcementRepository) Stats(ctx context.Context, id string) (models.AnnouncementStats, error) {
	an, err := r.Get(ctx, id)
	if err != nil {
		return models.AnnouncementStats{}, err
	}
	return r.StatsFor(ctx, an)
}

func (r *announcementRepository) StatsFor(ctx context.Context, an *models.Announcement) (models.AnnouncementStats, error) {
	var out models.AnnouncementStats
	var err error
	if out.UniqueUsersClicked, err = r.countInteractions(ctx, an.ID, "clicked_at", false); err != nil {
		return out, err
	}
	if out.UniqueTenantsClicked, err = r.countInteractions(ctx, an.ID, "clicked_at", true); err != nil {
		return out, err
	}
	if out.UniqueUsersDismissed, err = r.countInteractions(ctx, an.ID, "dismissed_at", false); err != nil {
		return out, err
	}
	if out.UniqueTenantsDismissed, err = r.countInteractions(ctx, an.ID, "dismissed_at", true); err != nil {
		return out, err
	}
	if out.EligibleTenants, out.EligibleUsers, err = r.eligibleCounts(ctx, an); err != nil {
		return out, err
	}
	return out, nil
}

// countInteractions counts rows (or distinct tenants) for one announcement that
// have the given timestamp column set.
func (r *announcementRepository) countInteractions(ctx context.Context, announcementID, column string, distinctTenant bool) (int, error) {
	q := r.db.NewSelect().Model((*models.AnnouncementInteraction)(nil)).
		Where("ai.announcement_id = ?", announcementID).
		Where("ai." + column + " IS NOT NULL")
	if distinctTenant {
		var n int
		err := q.ColumnExpr("COUNT(DISTINCT ai.tenant_id)").Scan(ctx, &n)
		return n, err
	}
	return q.Count(ctx)
}

// eligibleCounts computes how many active tenants (and their users) the
// announcement targets — the denominator for the click/dismiss stats. target_all
// means every active tenant; otherwise the union of the listed tiers + groups.
// A non-all announcement with no tiers and no groups reaches nobody.
func (r *announcementRepository) eligibleCounts(ctx context.Context, an *models.Announcement) (tenants, users int, err error) {
	frag, args, nobody := targetingPredicate(an)
	if nobody {
		return 0, 0, nil
	}
	tq := r.db.NewSelect().TableExpr("tenants AS tn").Where("tn.status = ?", models.TenantStatusActive)
	if frag != "" {
		tq = tq.Where(frag, args...)
	}
	if tenants, err = tq.Count(ctx); err != nil {
		return 0, 0, err
	}
	uq := r.db.NewSelect().TableExpr("users AS u").
		Join("JOIN tenants AS tn ON tn.id = u.tenant_id").
		Where("tn.status = ?", models.TenantStatusActive)
	if frag != "" {
		uq = uq.Where(frag, args...)
	}
	if users, err = uq.Count(ctx); err != nil {
		return 0, 0, err
	}
	return tenants, users, nil
}

// targetingPredicate builds the "does tenant tn match" SQL fragment (over the
// `tn` alias) for an announcement's targeting. Returns nobody=true when a
// non-all announcement lists neither a tier nor a group. An empty fragment with
// nobody=false means "every active tenant" (target_all).
func targetingPredicate(an *models.Announcement) (frag string, args []any, nobody bool) {
	if an.TargetAll {
		return "", nil, false
	}
	var parts []string
	if len(an.TargetTierIDs) > 0 {
		parts = append(parts, "tn.tier_id IN (?)")
		args = append(args, bun.In(an.TargetTierIDs))
	}
	if len(an.TargetGroupIDs) > 0 {
		parts = append(parts, "EXISTS (SELECT 1 FROM tenant_group_assignments tga WHERE tga.tenant_id = tn.id AND tga.group_id IN (?))")
		args = append(args, bun.In(an.TargetGroupIDs))
	}
	if len(parts) == 0 {
		return "", nil, true
	}
	return "(" + strings.Join(parts, " OR ") + ")", args, false
}

// hydrateTargets loads each announcement's target group + tier ids in two
// batched queries and attaches them (as non-nil slices).
func (r *announcementRepository) hydrateTargets(ctx context.Context, list []*models.Announcement) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	byID := make(map[string]*models.Announcement, len(list))
	for i, a := range list {
		ids[i] = a.ID
		byID[a.ID] = a
		a.TargetGroupIDs = []string{}
		a.TargetTierIDs = []string{}
	}

	var grows []models.AnnouncementTargetGroup
	if err := r.db.NewSelect().Model(&grows).Where("atg.announcement_id IN (?)", bun.In(ids)).OrderExpr("atg.group_id ASC").Scan(ctx); err != nil {
		return err
	}
	for _, g := range grows {
		if a := byID[g.AnnouncementID]; a != nil {
			a.TargetGroupIDs = append(a.TargetGroupIDs, g.GroupID)
		}
	}

	var trows []models.AnnouncementTargetTier
	if err := r.db.NewSelect().Model(&trows).Where("att.announcement_id IN (?)", bun.In(ids)).OrderExpr("att.tier_id ASC").Scan(ctx); err != nil {
		return err
	}
	for _, t := range trows {
		if a := byID[t.AnnouncementID]; a != nil {
			a.TargetTierIDs = append(a.TargetTierIDs, t.TierID)
		}
	}
	return nil
}

// insertAnnouncementTargets bulk-inserts the targeting rows (deduped) for an
// announcement on the given DB/tx. No-op for empty sets.
func insertAnnouncementTargets(ctx context.Context, db bun.IDB, announcementID string, groupIDs, tierIDs []string) error {
	if tiers := dedupeStrings(tierIDs); len(tiers) > 0 {
		rows := make([]models.AnnouncementTargetTier, 0, len(tiers))
		for _, id := range tiers {
			rows = append(rows, models.AnnouncementTargetTier{AnnouncementID: announcementID, TierID: id})
		}
		if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return err
		}
	}
	if groups := dedupeStrings(groupIDs); len(groups) > 0 {
		rows := make([]models.AnnouncementTargetGroup, 0, len(groups))
		for _, id := range groups {
			rows = append(rows, models.AnnouncementTargetGroup{AnnouncementID: announcementID, GroupID: id})
		}
		if _, err := db.NewInsert().Model(&rows).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// dedupeStrings drops blanks and duplicates, preserving first-seen order.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
