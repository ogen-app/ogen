package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/billing"
	"github.com/ogen-app/ogen/src/domain/models"
)

// BillingGateSQL is the billing gate as a boolean SQL expression over the
// tenants alias t: the workspace must check out and has never had a
// subscription that still counts. Test-mode subscriptions stop counting when
// the cutover invalidates them; subscriptions from the other mode are never
// stored, so the expression needs no mode. The CASE keeps the EXISTS off the
// path of every workspace that is not flagged, so the auth query stays cheap.
const BillingGateSQL = `(CASE WHEN t.billing_required THEN NOT EXISTS (
	SELECT 1 FROM billing_subscriptions AS gate_bs
	WHERE gate_bs.tenant_id = t.id AND gate_bs.invalidated_at IS NULL) ELSE false END)`

// BillingRepository persists the billing mirror. The tables carry a plain
// tenant_id; every method takes the tenant explicitly (webhooks and sweeps run
// without a tenant context). Methods taking a bun.IDB join the caller's
// transaction.
type BillingRepository interface {
	// Subscriptions.

	// GetSubscriptionByProviderID returns sql.ErrNoRows when absent. With
	// forUpdate the row is locked for the caller's transaction.
	GetSubscriptionByProviderID(ctx context.Context, db bun.IDB, provider, providerSubID string, forUpdate bool) (*models.BillingSubscription, error)
	GetSubscription(ctx context.Context, id string) (*models.BillingSubscription, error)
	InsertSubscription(ctx context.Context, db bun.IDB, s *models.BillingSubscription) error
	UpdateSubscription(ctx context.Context, db bun.IDB, s *models.BillingSubscription) error
	// ListSubscriptionsByTenant returns every subscription of a tenant,
	// invalidated ones included, newest first.
	ListSubscriptionsByTenant(ctx context.Context, db bun.IDB, tenantID string) ([]models.BillingSubscription, error)
	// TrialUsed reports whether an account has had a free trial in a mode.
	TrialUsed(ctx context.Context, accountID string, testMode bool) (bool, error)
	// ListForReconcile pages non-invalidated, non-expired subscriptions of
	// a mode by id.
	ListForReconcile(ctx context.Context, testMode bool, afterID string, limit int) ([]models.BillingSubscription, error)
	// ListByStatus lists non-invalidated subscriptions of a mode in the given
	// statuses, optionally only those updated since a time.
	ListByStatus(ctx context.Context, testMode bool, statuses []string, updatedSince *time.Time) ([]models.BillingSubscription, error)
	// ListUnpaidExpired lists expired subscriptions with no payment and no
	// teardown yet.
	ListUnpaidExpired(ctx context.Context, testMode bool, limit int) ([]models.BillingSubscription, error)
	// ListTestModeLive lists non-invalidated test-mode subscriptions.
	ListTestModeLive(ctx context.Context) ([]models.BillingSubscription, error)
	// Invalidate marks subscriptions invalidated with a reason.
	Invalidate(ctx context.Context, db bun.IDB, ids []string, reason string, at time.Time) error

	// Webhook log.

	// InsertWebhookEvent records a delivery. When the body was seen before it
	// returns the earlier row and inserted=false.
	InsertWebhookEvent(ctx context.Context, e *models.BillingWebhookEvent) (existing *models.BillingWebhookEvent, inserted bool, err error)
	FinishWebhookEvent(ctx context.Context, id, outcome, errMsg string, tenantID *string, at time.Time) error
	ListWebhookEventsByTenant(ctx context.Context, tenantID string, limit int) ([]models.BillingWebhookEvent, error)
	// ListFailedWebhookEvents lists failed deliveries since a time; with
	// unresolvedOnly only those whose tenant could not be resolved.
	ListFailedWebhookEvents(ctx context.Context, since time.Time, unresolvedOnly bool) ([]models.BillingWebhookEvent, error)
	// HasUnresolvedSubscriptionEvent reports whether a delivery of one of the
	// named events for a subscription failed before its tenant was resolved.
	HasUnresolvedSubscriptionEvent(ctx context.Context, provider, providerSubID string, eventNames []string) (bool, error)
	DeleteWebhookEventsBefore(ctx context.Context, cutoff time.Time) (int, error)

	// Scheduled changes.

	InsertScheduledChange(ctx context.Context, db bun.IDB, c *models.BillingScheduledChange) error
	GetScheduledChange(ctx context.Context, db bun.IDB, id string, forUpdate bool) (*models.BillingScheduledChange, error)
	// OpenScheduledChange returns the subscription's pending or
	// provider-applied change, or sql.ErrNoRows.
	OpenScheduledChange(ctx context.Context, db bun.IDB, subscriptionID string) (*models.BillingScheduledChange, error)
	// SetScheduledChangeStatus moves a change to status if it is currently in
	// one of from; it reports whether it moved.
	SetScheduledChangeStatus(ctx context.Context, db bun.IDB, id string, from []string, status, errMsg string, at time.Time) (bool, error)
	ListScheduledChangesByTenant(ctx context.Context, tenantID string) ([]models.BillingScheduledChange, error)
	// ListDueScheduledChanges lists open changes whose next step is due.
	ListDueScheduledChanges(ctx context.Context, now time.Time, providerLead time.Duration) ([]models.BillingScheduledChange, error)
	ListScheduledChangesByStatus(ctx context.Context, status string, since time.Time) ([]models.BillingScheduledChange, error)

	// Tenants and tiers.

	SetBillingRequired(ctx context.Context, tenantID string, required bool, at time.Time) (bool, error)
	// SetComplimentary makes a workspace complimentary (billing_required
	// off) or ends that (billing_required on). false when the tenant is
	// unknown or deleted.
	SetComplimentary(ctx context.Context, tenantID string, complimentary bool, at time.Time) (bool, error)
	// Gated evaluates BillingGateSQL for one tenant.
	Gated(ctx context.Context, tenantID string) (bool, error)
	TenantNames(ctx context.Context, ids []string) (map[string]string, error)
	// FreeTrialTier returns the marked tier, or nil when the offer is off.
	FreeTrialTier(ctx context.Context) (*models.TenantTier, error)
	// SetFreeTrialTier marks one tier (clearing the previous mark) or, with
	// an empty id, clears the mark. It reports false when the tier is unknown.
	SetFreeTrialTier(ctx context.Context, tierID string, at time.Time) (bool, error)

	// Prices.

	// Prices resolves price rows with their tier, keyed by price id.
	Prices(ctx context.Context, ids []string) (map[string]billing.Price, error)
	// MappablePrices lists the prices a variant may be mapped to (the
	// non-zero prices of active, purchasable versions; a free price is never
	// sold through the provider) plus the given ids, by tier and version.
	MappablePrices(ctx context.Context, extraIDs []string) ([]billing.Price, error)

	// Variant mappings.

	// ListVariantMappings lists one mode's mappings.
	ListVariantMappings(ctx context.Context, testMode bool) ([]models.BillingVariantMapping, error)
	// UpsertVariantMapping maps a price in a mode, replacing its variant.
	// ErrVariantTaken when the variant sells another price in that mode.
	UpsertVariantMapping(ctx context.Context, m *models.BillingVariantMapping) error
	// DeleteVariantMapping unmaps a price in a mode; false when it was not
	// mapped.
	DeleteVariantMapping(ctx context.Context, priceID string, testMode bool) (bool, error)
	// SeedVariantMappings inserts the rows when the mode has no mapping yet,
	// once across concurrently booting replicas. It returns how many rows it
	// inserted and the price ids it skipped because they do not exist.
	SeedVariantMappings(ctx context.Context, testMode bool, rows []models.BillingVariantMapping) (int, []string, error)

	// CountConnectedAccounts counts a workspace's connected (not
	// disconnected) social accounts: the connected_accounts entitlement.
	CountConnectedAccounts(ctx context.Context, tenantID string) (int64, error)
}

func (r *billingRepository) CountConnectedAccounts(ctx context.Context, tenantID string) (int64, error) {
	// A raw table count: bun's Count skips the TenantScoped hook, so the
	// tenant is filtered explicitly.
	n, err := r.db.NewSelect().TableExpr("social_accounts").
		Where("tenant_id = ?", tenantID).
		Where("deleted_at IS NULL").
		Count(ctx)
	return int64(n), err
}

type billingRepository struct {
	db *bun.DB
}

// NewBillingRepository returns a Bun-backed BillingRepository.
func NewBillingRepository(db *bun.DB) BillingRepository {
	return &billingRepository{db: db}
}

func (r *billingRepository) idb(db bun.IDB) bun.IDB {
	if db == nil {
		return r.db
	}
	return db
}

func (r *billingRepository) GetSubscriptionByProviderID(ctx context.Context, db bun.IDB, provider, providerSubID string, forUpdate bool) (*models.BillingSubscription, error) {
	s := new(models.BillingSubscription)
	q := r.idb(db).NewSelect().Model(s).
		Where("bs.provider = ?", provider).
		Where("bs.provider_subscription_id = ?", providerSubID)
	if forUpdate {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (r *billingRepository) GetSubscription(ctx context.Context, id string) (*models.BillingSubscription, error) {
	s := new(models.BillingSubscription)
	if err := r.db.NewSelect().Model(s).Where("bs.id = ?", id).Scan(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (r *billingRepository) InsertSubscription(ctx context.Context, db bun.IDB, s *models.BillingSubscription) error {
	_, err := r.idb(db).NewInsert().Model(s).Exec(ctx)
	return err
}

func (r *billingRepository) UpdateSubscription(ctx context.Context, db bun.IDB, s *models.BillingSubscription) error {
	_, err := r.idb(db).NewUpdate().Model(s).WherePK().ExcludeColumn("id", "tenant_id", "provider", "provider_subscription_id", "created_at").Exec(ctx)
	return err
}

func (r *billingRepository) ListSubscriptionsByTenant(ctx context.Context, db bun.IDB, tenantID string) ([]models.BillingSubscription, error) {
	var out []models.BillingSubscription
	err := r.idb(db).NewSelect().Model(&out).
		Where("bs.tenant_id = ?", tenantID).
		OrderExpr("bs.created_at DESC, bs.id DESC").
		Scan(ctx)
	return out, err
}

func (r *billingRepository) TrialUsed(ctx context.Context, accountID string, testMode bool) (bool, error) {
	return r.db.NewSelect().Model((*models.BillingSubscription)(nil)).
		Where("bs.account_id = ?", accountID).
		Where("bs.test_mode = ?", testMode).
		Where("bs.trial_ends_at IS NOT NULL").
		Exists(ctx)
}

func (r *billingRepository) ListForReconcile(ctx context.Context, testMode bool, afterID string, limit int) ([]models.BillingSubscription, error) {
	var out []models.BillingSubscription
	err := r.db.NewSelect().Model(&out).
		Where("bs.test_mode = ?", testMode).
		Where("bs.invalidated_at IS NULL").
		Where("bs.status <> ?", models.SubscriptionStatusExpired).
		Where("bs.id > ?", afterID).
		OrderExpr("bs.id").
		Limit(limit).
		Scan(ctx)
	return out, err
}

func (r *billingRepository) ListByStatus(ctx context.Context, testMode bool, statuses []string, updatedSince *time.Time) ([]models.BillingSubscription, error) {
	var out []models.BillingSubscription
	q := r.db.NewSelect().Model(&out).
		Where("bs.test_mode = ?", testMode).
		Where("bs.invalidated_at IS NULL").
		Where("bs.status IN (?)", bun.List(statuses))
	if updatedSince != nil {
		q = q.Where("bs.updated_at >= ?", *updatedSince)
	}
	err := q.OrderExpr("bs.updated_at DESC, bs.id DESC").Scan(ctx)
	return out, err
}

func (r *billingRepository) ListUnpaidExpired(ctx context.Context, testMode bool, limit int) ([]models.BillingSubscription, error) {
	var out []models.BillingSubscription
	err := r.db.NewSelect().Model(&out).
		Where("bs.test_mode = ?", testMode).
		Where("bs.invalidated_at IS NULL").
		Where("bs.status = ?", models.SubscriptionStatusExpired).
		Where("bs.first_paid_at IS NULL").
		Where("bs.teardown_at IS NULL").
		// A workspace another subscription still entitles is never torn down;
		// filtering it here keeps those rows from filling every batch. Mirrors
		// billing.Live.
		Where(`NOT EXISTS (
			SELECT 1 FROM billing_subscriptions AS live_bs
			WHERE live_bs.tenant_id = bs.tenant_id
			  AND live_bs.test_mode = bs.test_mode
			  AND live_bs.invalidated_at IS NULL
			  AND (live_bs.status IN (?) OR (live_bs.status = ? AND live_bs.ends_at > now())))`,
			bun.List([]string{models.SubscriptionStatusOnTrial, models.SubscriptionStatusActive, models.SubscriptionStatusPastDue,
				models.SubscriptionStatusUnpaid, models.SubscriptionStatusPaused}),
			models.SubscriptionStatusCancelled).
		OrderExpr("bs.id").
		Limit(limit).
		Scan(ctx)
	return out, err
}

func (r *billingRepository) ListTestModeLive(ctx context.Context) ([]models.BillingSubscription, error) {
	var out []models.BillingSubscription
	err := r.db.NewSelect().Model(&out).
		Where("bs.test_mode").
		Where("bs.invalidated_at IS NULL").
		OrderExpr("bs.tenant_id, bs.id").
		Scan(ctx)
	return out, err
}

func (r *billingRepository) Invalidate(ctx context.Context, db bun.IDB, ids []string, reason string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := r.idb(db).NewUpdate().Model((*models.BillingSubscription)(nil)).
		Set("invalidated_at = ?", at).
		Set("invalidated_reason = ?", reason).
		Set("updated_at = ?", at).
		Where("id IN (?)", bun.List(ids)).
		Where("invalidated_at IS NULL").
		Exec(ctx)
	return err
}

func (r *billingRepository) InsertWebhookEvent(ctx context.Context, e *models.BillingWebhookEvent) (*models.BillingWebhookEvent, bool, error) {
	res, err := r.db.NewInsert().Model(e).On("CONFLICT (body_sha256) DO NOTHING").Exec(ctx)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, true, nil
	}
	existing := new(models.BillingWebhookEvent)
	if err := r.db.NewSelect().Model(existing).Where("bwe.body_sha256 = ?", e.BodySHA256).Scan(ctx); err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (r *billingRepository) FinishWebhookEvent(ctx context.Context, id, outcome, errMsg string, tenantID *string, at time.Time) error {
	q := r.db.NewUpdate().Model((*models.BillingWebhookEvent)(nil)).
		Set("outcome = ?", outcome).
		Set("error = ?", errMsg).
		Set("processed_at = ?", at).
		Where("id = ?", id)
	if tenantID != nil {
		q = q.Set("tenant_id = ?", *tenantID)
	}
	_, err := q.Exec(ctx)
	return err
}

func (r *billingRepository) ListWebhookEventsByTenant(ctx context.Context, tenantID string, limit int) ([]models.BillingWebhookEvent, error) {
	var out []models.BillingWebhookEvent
	err := r.db.NewSelect().Model(&out).
		ExcludeColumn("payload").
		Where("bwe.tenant_id = ?", tenantID).
		OrderExpr("bwe.received_at DESC, bwe.id DESC").
		Limit(limit).
		Scan(ctx)
	return out, err
}

func (r *billingRepository) ListFailedWebhookEvents(ctx context.Context, since time.Time, unresolvedOnly bool) ([]models.BillingWebhookEvent, error) {
	var out []models.BillingWebhookEvent
	q := r.db.NewSelect().Model(&out).
		ExcludeColumn("payload").
		Where("bwe.outcome = ?", models.WebhookOutcomeFailed).
		Where("bwe.received_at >= ?", since)
	if unresolvedOnly {
		q = q.Where("bwe.tenant_id IS NULL")
	}
	err := q.OrderExpr("bwe.received_at DESC").Scan(ctx)
	return out, err
}

func (r *billingRepository) HasUnresolvedSubscriptionEvent(ctx context.Context, provider, providerSubID string, eventNames []string) (bool, error) {
	return r.db.NewSelect().Model((*models.BillingWebhookEvent)(nil)).
		Where("bwe.provider = ?", provider).
		Where("bwe.provider_object_id = ?", providerSubID).
		Where("bwe.event_name IN (?)", bun.List(eventNames)).
		Where("bwe.outcome = ?", models.WebhookOutcomeFailed).
		Where("bwe.tenant_id IS NULL").
		Exists(ctx)
}

func (r *billingRepository) DeleteWebhookEventsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := r.db.NewDelete().Model((*models.BillingWebhookEvent)(nil)).
		Where("received_at < ?", cutoff).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *billingRepository) InsertScheduledChange(ctx context.Context, db bun.IDB, c *models.BillingScheduledChange) error {
	_, err := r.idb(db).NewInsert().Model(c).Exec(ctx)
	return err
}

func (r *billingRepository) GetScheduledChange(ctx context.Context, db bun.IDB, id string, forUpdate bool) (*models.BillingScheduledChange, error) {
	c := new(models.BillingScheduledChange)
	q := r.idb(db).NewSelect().Model(c).Where("bsc.id = ?", id)
	if forUpdate {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *billingRepository) OpenScheduledChange(ctx context.Context, db bun.IDB, subscriptionID string) (*models.BillingScheduledChange, error) {
	c := new(models.BillingScheduledChange)
	err := r.idb(db).NewSelect().Model(c).
		Where("bsc.billing_subscription_id = ?", subscriptionID).
		Where("bsc.status IN (?)", bun.List([]string{models.ScheduledChangePending, models.ScheduledChangeProviderApplied})).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (r *billingRepository) SetScheduledChangeStatus(ctx context.Context, db bun.IDB, id string, from []string, status, errMsg string, at time.Time) (bool, error) {
	res, err := r.idb(db).NewUpdate().Model((*models.BillingScheduledChange)(nil)).
		Set("status = ?", status).
		Set("error = ?", errMsg).
		Set("updated_at = ?", at).
		Where("id = ?", id).
		Where("status IN (?)", bun.List(from)).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *billingRepository) ListScheduledChangesByTenant(ctx context.Context, tenantID string) ([]models.BillingScheduledChange, error) {
	var out []models.BillingScheduledChange
	err := r.db.NewSelect().Model(&out).
		Where("bsc.tenant_id = ?", tenantID).
		OrderExpr("bsc.created_at DESC, bsc.id DESC").
		Scan(ctx)
	return out, err
}

func (r *billingRepository) ListDueScheduledChanges(ctx context.Context, now time.Time, providerLead time.Duration) ([]models.BillingScheduledChange, error) {
	var out []models.BillingScheduledChange
	err := r.db.NewSelect().Model(&out).
		WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			return q.
				WhereGroup(" OR ", func(q *bun.SelectQuery) *bun.SelectQuery {
					return q.Where("bsc.status = ?", models.ScheduledChangePending).Where("bsc.effective_at <= ?", now.Add(providerLead))
				}).
				WhereGroup(" OR ", func(q *bun.SelectQuery) *bun.SelectQuery {
					return q.Where("bsc.status = ?", models.ScheduledChangeProviderApplied).Where("bsc.effective_at <= ?", now)
				})
		}).
		OrderExpr("bsc.effective_at").
		Scan(ctx)
	return out, err
}

func (r *billingRepository) ListScheduledChangesByStatus(ctx context.Context, status string, since time.Time) ([]models.BillingScheduledChange, error) {
	var out []models.BillingScheduledChange
	err := r.db.NewSelect().Model(&out).
		Where("bsc.status = ?", status).
		Where("bsc.updated_at >= ?", since).
		OrderExpr("bsc.updated_at DESC").
		Scan(ctx)
	return out, err
}

func (r *billingRepository) SetBillingRequired(ctx context.Context, tenantID string, required bool, at time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.Tenant)(nil)).
		Set("billing_required = ?", required).
		Set("updated_at = ?", at).
		Where("id = ?", tenantID).
		Where("deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *billingRepository) SetComplimentary(ctx context.Context, tenantID string, complimentary bool, at time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.Tenant)(nil)).
		Set("complimentary = ?", complimentary).
		Set("billing_required = (billing_required OR complimentary) AND ?", !complimentary).
		Set("updated_at = ?", at).
		Where("id = ?", tenantID).
		Where("deleted_at IS NULL").
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *billingRepository) Gated(ctx context.Context, tenantID string) (bool, error) {
	var gated bool
	err := r.db.NewSelect().TableExpr("tenants AS t").
		ColumnExpr(BillingGateSQL).
		Where("t.id = ?", tenantID).
		Scan(ctx, &gated)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return gated, err
}

func (r *billingRepository) TenantNames(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID   string `bun:"id"`
		Name string `bun:"name"`
	}
	if err := r.db.NewSelect().TableExpr("tenants").Column("id", "name").Where("id IN (?)", bun.List(ids)).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row.Name
	}
	return out, nil
}

func (r *billingRepository) FreeTrialTier(ctx context.Context) (*models.TenantTier, error) {
	t := new(models.TenantTier)
	err := r.db.NewSelect().Model(t).Where("tt.free_trial").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (r *billingRepository) SetFreeTrialTier(ctx context.Context, tierID string, at time.Time) (bool, error) {
	found := tierID == ""
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Model((*models.TenantTier)(nil)).
			Set("free_trial = false").
			Set("updated_at = ?", at).
			Where("free_trial").
			Where("id <> ?", tierID).
			Exec(ctx); err != nil {
			return err
		}
		if tierID == "" {
			return nil
		}
		res, err := tx.NewUpdate().Model((*models.TenantTier)(nil)).
			Set("free_trial = true").
			Set("updated_at = ?", at).
			Where("id = ?", tierID).
			Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		found = n == 1
		if !found {
			// Unknown tier: keep the previous mark rather than turn the offer off.
			return errUnknownTier
		}
		return nil
	})
	if errors.Is(err, errUnknownTier) {
		return false, nil
	}
	return found, err
}

var errUnknownTier = errors.New("unknown tier")
