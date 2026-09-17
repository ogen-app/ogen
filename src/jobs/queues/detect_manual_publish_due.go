package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// manualPublishDueLister enumerates overdue manual-publish posts; the post
// repository satisfies it. ownerLister resolves a tenant's owner recipients; the
// user repository satisfies it. Narrow interfaces keep the sweep unit-testable.
type manualPublishDueLister interface {
	ListManualPublishDue(ctx context.Context, now time.Time, limit int) ([]models.Post, error)
}

type ownerLister interface {
	ListOwnersByTenant(ctx context.Context, tenantID string) ([]models.User, error)
}

// DetectManualPublishDueQueue is the recurring sweep that notifies a workspace
// when a post left for manual publishing reaches its scheduled time and nobody
// has published it yet (CON-285 FR9). Today nothing tells the user a manual post
// has come due; this closes that gap. It mirrors the connection-health sweep: a
// marker payload, self-registering, gated on a positive interval, and a harmless
// no-op when there is nothing due.
const DetectManualPublishDueQueue = "detect_manual_publish_due"

const manualPublishComp = "jobs.detect_manual_publish_due"

// DetectManualPublishDueTask is a marker payload — this queue carries no
// per-tick data.
type DetectManualPublishDueTask struct{}

// Kind implements river.JobArgs.
func (DetectManualPublishDueTask) Kind() string { return DetectManualPublishDueQueue }

// InsertOpts mirrors the other periodic sweeps: one attempt (each tick records
// its own outcome and the emit is idempotent per recipient), active-state
// uniqueness so overlapping ticks don't stack.
func (DetectManualPublishDueTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// DetectManualPublishDueProcessor wires one sweep tick. Posts resolves the
// overdue manual-publish posts across tenants, Users resolves each tenant's
// owner recipients, and Notifier drops the in-app notification. A nil Notifier
// (notification center unwired) makes the sweep a harmless read-only no-op.
type DetectManualPublishDueProcessor struct {
	river.WorkerDefaults[DetectManualPublishDueTask]
	Posts    manualPublishDueLister
	Users    ownerLister
	Notifier *notify.Service
}

func (p *DetectManualPublishDueProcessor) Work(ctx context.Context, job *river.Job[DetectManualPublishDueTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Background job spans tenants; the enumeration runs cross-tenant and each
	// emit re-scopes to the post's tenant.
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

func (p *DetectManualPublishDueProcessor) Timeout(*river.Job[DetectManualPublishDueTask]) time.Duration {
	return 60 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &DetectManualPublishDueProcessor{
			Posts:    d.Zernio.PostRepo,
			Users:    d.Users,
			Notifier: d.Notifier,
		})
	})
}

// Process runs one sweep tick. Failures are recorded in the log line but never
// returned to River — a single attempt records its own outcome and reschedules
// on the next interval.
func (p *DetectManualPublishDueProcessor) Process(ctx context.Context, _ DetectManualPublishDueTask) error {
	tickStart := time.Now()
	notified, err := p.sweep(ctx, tickStart.UTC())
	jobs.ManualPublishDueSwept.Add(int64(notified))
	if err != nil {
		slog.ErrorContext(ctx, "manual-publish-due sweep failed", logging.AttrComponent, manualPublishComp,
			"notified", notified, "tick_ms", time.Since(tickStart).Milliseconds(), logging.AttrError, err)
	} else {
		slog.InfoContext(ctx, "manual-publish-due sweep ok", logging.AttrComponent, manualPublishComp,
			"notified", notified, "tick_ms", time.Since(tickStart).Milliseconds())
	}
	return nil
}

// sweep enumerates every overdue manual-publish post (cross-tenant), groups them
// by tenant, and notifies each tenant's owners once per post. The dedupe_key
// (manual_publish:<post_id>) collapses repeats for the same post per recipient
// while still unread, so a post that stays overdue across many ticks notifies
// once; a post published or edited out of the manual-publish state stops matching
// and never fires again. Returns the number of post notifications emitted.
func (p *DetectManualPublishDueProcessor) sweep(ctx context.Context, now time.Time) (int, error) {
	if p.Posts == nil || p.Users == nil || p.Notifier == nil {
		return 0, nil // notification center unwired: nothing to do
	}
	due, err := p.Posts.ListManualPublishDue(ctx, now, 0)
	if err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}

	// Group by tenant so owners are resolved once per tenant, not once per post.
	byTenant := map[string][]models.Post{}
	order := make([]string, 0)
	for _, post := range due {
		if _, ok := byTenant[post.TenantID]; !ok {
			order = append(order, post.TenantID)
		}
		byTenant[post.TenantID] = append(byTenant[post.TenantID], post)
	}

	notified := 0
	var firstErr error
	for _, tenantID := range order {
		tctx := tenantctx.With(ctx, tenantID)
		owners, ownersErr := p.Users.ListOwnersByTenant(tctx, tenantID)
		if ownersErr != nil {
			if firstErr == nil {
				firstErr = ownersErr
			}
			slog.ErrorContext(tctx, "manual-publish-due: resolve owners failed", logging.AttrComponent, manualPublishComp,
				"tenant_id", tenantID, logging.AttrError, ownersErr)
			continue
		}
		ownerIDs := make([]string, 0, len(owners))
		for _, o := range owners {
			if o.ID != "" {
				ownerIDs = append(ownerIDs, o.ID)
			}
		}
		if len(ownerIDs) == 0 {
			continue
		}
		for _, post := range byTenant[tenantID] {
			// EmitToUsers returns the first per-recipient insert error; a durable
			// row may be missing when it does, so record it and don't count this
			// post as notified. Process still returns nil to River (best-effort),
			// but the error surfaces in the "sweep failed" log + metric.
			if err := p.Notifier.EmitToUsers(tctx, ownerIDs, notify.Spec{
				Level:      models.NotificationLevelWarning,
				Type:       "post.manual_publish_due",
				Title:      "Post ready to publish",
				Body:       "A post left for manual publishing is now due.",
				EntityType: "post",
				EntityID:   post.ID,
				ActionURL:  "/posts/" + post.ID,
				Data:       map[string]any{"platform_id": post.PlatformID},
				DedupeKey:  "manual_publish:" + post.ID,
			}); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			notified++
		}
	}
	return notified, firstErr
}
