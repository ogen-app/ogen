package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// ActivityRepository persists tenant_activity_events in the isolated analytics
// database. It is constructed with the analytics *bun.DB, not the
// main pool. Writes come from the async activity.Recorder in a system context
// (tenant pre-set per row). A read/query surface is deferred to the consumers;
// v1 is collection-only.
type ActivityRepository interface {
	// Insert writes a batch of events. A nil/empty batch is a no-op.
	Insert(ctx context.Context, events []*models.ActivityEvent) error
}

type activityRepository struct {
	db *bun.DB
}

// NewActivityRepository builds an ActivityRepository. Pass the analytics *bun.DB.
func NewActivityRepository(db *bun.DB) ActivityRepository {
	return &activityRepository{db: db}
}

func (r *activityRepository) Insert(ctx context.Context, events []*models.ActivityEvent) error {
	if len(events) == 0 {
		return nil
	}
	_, err := r.db.NewInsert().Model(&events).Exec(ctx)
	return err
}

// postLogBackfillPrefix marks tenant_activity_events rows that were migrated from
// post_logs. Migrated rows reuse the source post_log id (prefixed) as their
// activity id, which makes the backfill idempotent and restart-safe: a re-run
// preloads the already-migrated ids and skips them. The prefix also
// distinguishes them from natively-recorded rows.
const postLogBackfillPrefix = "plog_"

// postLogActivityMap curates which post_logs event types become activity events
// and how they map onto the live activity taxonomy. Types absent from
// this map — the internal River task lifecycle (task_*), Zernio polling/retry,
// and rejected (blocked) transitions — are operational noise and are skipped, so
// tenant_activity_events stays a clean behavioural stream whose type names match the
// live instrumentation.
var postLogActivityMap = map[models.PostLogEventType]struct{ category, typ string }{
	models.PostLogEventStateTransition:       {"post", "post_state_transition"},
	models.PostLogEventUserRetry:             {"post", "post_state_transition"},
	models.PostLogEventValidationPassed:      {"post", "post_validation_passed"},
	models.PostLogEventValidationFailed:      {"post", "post_validation_failed"},
	models.PostLogEventUserSchedule:          {"post", "post_scheduled"},
	models.PostLogEventUserCancel:            {"post", "post_schedule_cancelled"},
	models.PostLogEventPostCloned:            {"post", "post_cloned"},
	models.PostLogEventPostRestored:          {"post", "post_restored"},
	models.PostLogEventQualityAssessed:       {"post", "post_quality_assessed"},
	models.PostLogEventAllowlistDecision:     {"publish", "auto_publish_decision"},
	models.PostLogEventZernioSubmit:          {"publish", "publish_submitted"},
	models.PostLogEventZernioCancel:          {"publish", "publish_cancelled"},
	models.PostLogEventReconciliationTimeout: {"publish", "reconciliation_timeout"},
}

// BackfillPostLogsToActivity migrates the historical post_logs audit trail
// (main DB) into tenant_activity_events (analytics DB) so the behavioural stream has
// real history, not just events recorded since the live instrumentation shipped.
// It maps the meaningful event types onto the live activity taxonomy
// (see postLogActivityMap) and skips operational noise.
//
// It runs once. A completed run records postLogBackfillDoneKey and later boots
// return at once: from the boot that runs it onwards, every transition is
// recorded live, so re-running at each boot would only re-migrate the post_logs
// written since the previous boot, which the live path already recorded under
// its own ids. The whole migration is one transaction, so a failed run leaves
// nothing and the next boot retries it. Each migrated row's id is the source
// post_log id under postLogBackfillPrefix. The read is cross-tenant (each row
// carries its tenant_id, preserved on insert under a system context). Returns
// the number of rows inserted.
//
// The upper bound `before` excludes rows at/after it. The backfill runs in a
// background goroutine at boot, concurrent with live serving, and the
// live post-transition path writes both a post_log AND its own natively-id'd
// activity event. Bounding the scan to rows that predate this process therefore
// guarantees the backfill never migrates a post_log the live path is recording
// in parallel, which would otherwise persist as a duplicate (the two carry
// different ids, so no unique constraint catches it). Pass the instant captured
// just before the HTTP listener opens.
func BackfillPostLogsToActivity(ctx context.Context, mainDB, analyticsDB *bun.DB, before time.Time) (int, error) {
	if mainDB == nil || analyticsDB == nil {
		return 0, nil
	}
	markerCtx := tenantctx.With(ctx, models.DefaultTenantID)
	var done []string
	if err := mainDB.NewSelect().Model((*models.Setting)(nil)).
		Column("st.key").
		Where("st.key = ?", postLogBackfillDoneKey).
		Limit(1).
		Scan(markerCtx, &done); err != nil {
		return 0, err
	}
	if len(done) > 0 {
		return 0, nil
	}
	// A deployment that ran the migration on earlier boots, before the marker
	// existed, already holds migrated rows: re-running it now would only add
	// the duplicates described above, so it is just marked done.
	var migrated []string
	if err := analyticsDB.NewRaw(
		`SELECT id FROM tenant_activity_events WHERE id LIKE ? LIMIT 1`, postLogBackfillPrefix+`%`,
	).Scan(ctx, &migrated); err != nil {
		return 0, err
	}
	n := 0
	if len(migrated) == 0 {
		var err error
		if n, err = backfillPostLogs(ctx, mainDB, analyticsDB, before); err != nil {
			return n, err
		}
	}
	_, err := mainDB.NewInsert().
		Model(&models.Setting{Key: postLogBackfillDoneKey, Value: before.UTC().Format(time.RFC3339)}).
		On("CONFLICT (tenant_id, key) DO UPDATE").
		Set("value = EXCLUDED.value").
		Exec(markerCtx)
	return n, err
}

// postLogBackfillDoneKey is the default tenant's setting that marks the
// post_logs → activity migration complete; its value is the bound it ran to.
const postLogBackfillDoneKey = "activity.post_logs_backfilled_until"

func backfillPostLogs(ctx context.Context, mainDB, analyticsDB *bun.DB, before time.Time) (int, error) {

	type logRow struct {
		ID             string                  `bun:"id"`
		TenantID       string                  `bun:"tenant_id"`
		PostID         string                  `bun:"post_id"`
		EventTimestamp time.Time               `bun:"event_timestamp"`
		EventType      models.PostLogEventType `bun:"event_type"`
		Actor          string                  `bun:"actor"`
		FromStatus     *string                 `bun:"from_status"`
		ToStatus       *string                 `bun:"to_status"`
		Payload        string                  `bun:"payload"`
		Summary        string                  `bun:"summary"`
	}

	// Only called on a database with nothing migrated yet, so every row before
	// the bound is read. `< before` excludes anything created at/after this
	// process started serving, so a live write can never race into the window.
	var logs []logRow
	if err := mainDB.NewRaw(`
		SELECT id, tenant_id, post_id, event_timestamp, event_type, actor,
		       from_status, to_status, payload, summary
		FROM post_logs
		WHERE event_timestamp < ?
		ORDER BY event_timestamp
	`, before).Scan(ctx, &logs); err != nil {
		return 0, err
	}
	if len(logs) == 0 {
		return 0, nil
	}

	// One transaction for the whole migration: the marker treats any migrated
	// row as a finished migration, so a run that fails partway must leave none.
	sysCtx := tenantctx.WithSystem(ctx)
	tx, err := analyticsDB.BeginTx(sysCtx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	batch := make([]*models.ActivityEvent, 0, 500)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := tx.NewInsert().Model(&batch).Exec(sysCtx); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}

	inserted := 0
	for i := range logs {
		lr := logs[i]
		mapped, ok := postLogActivityMap[lr.EventType]
		if !ok {
			continue // operational noise — not migrated
		}
		id := postLogBackfillPrefix + lr.ID

		ev := &models.ActivityEvent{
			ID:         id,
			Category:   mapped.category,
			Type:       mapped.typ,
			EntityType: "post",
			EntityID:   lr.PostID,
			Status:     statusTransition(lr.FromStatus, lr.ToStatus),
			Source:     "job",
			Tags:       models.StringSlice{"post_logs_backfill"},
			Payload:    logPayload(lr.EventType, lr.Summary, lr.Payload),
			OccurredAt: lr.EventTimestamp,
		}
		ev.TenantID = lr.TenantID // preserved by BeforeAppendModel in the system ctx
		if lr.Actor != "" && lr.Actor != models.ActorSystem {
			ev.UserID = lr.Actor
			ev.Source = "api"
		}

		batch = append(batch, ev)
		inserted++
		if len(batch) >= 500 {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := flush(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

// statusTransition renders the from/to status pair the way the live
// instrumentation records a transition ("from->to"), tolerating a missing side.
func statusTransition(from, to *string) string {
	switch {
	case from != nil && to != nil:
		return *from + "->" + *to
	case to != nil:
		return *to
	case from != nil:
		return *from
	default:
		return ""
	}
}

// logPayload preserves the original post_log event_type + summary, merging in
// the (best-effort parsed) original payload object so nothing is lost.
func logPayload(eventType models.PostLogEventType, summary, raw string) models.JSONMap {
	out := models.JSONMap{"post_log_event_type": string(eventType)}
	if summary != "" {
		out["summary"] = summary
	}
	if raw != "" && raw != "{}" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
			for k, v := range parsed {
				if _, taken := out[k]; !taken {
					out[k] = v
				}
			}
		}
	}
	return out
}
