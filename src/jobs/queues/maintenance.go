package queues

import (
	"context"
	"time"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// MaintenanceTasks returns the sweeps river-service schedules and ogen runs
// whole, keyed by kind (jobrpc.v1 MaintenanceService). They only touch ogen's
// own tables and storage. Each runs across tenants, under its own timeout.
func MaintenanceTasks(d Deps) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		CleanupPostLogsQueue: sweep(30*time.Second, func(ctx context.Context) error {
			return newCleanupPostLogs(d).Process(ctx, CleanupPostLogsTask{})
		}),
		CleanupNotificationsQueue: sweep(30*time.Second, func(ctx context.Context) error {
			return newCleanupNotifications(d).Process(ctx, CleanupNotificationsTask{})
		}),
		CleanupLoginSecurityQueue: sweep(time.Minute, func(ctx context.Context) error {
			return newCleanupLoginSecurity(d).Process(ctx, CleanupLoginSecurityTask{})
		}),
		CleanupPluginPairingsQueue: sweep(30*time.Second, func(ctx context.Context) error {
			return newCleanupPluginPairings(d).Process(ctx, CleanupPluginPairingsTask{})
		}),
		SweepPendingUploadsQueue: sweep(5*time.Minute, func(ctx context.Context) error {
			return newSweepPendingUploads(d).Process(ctx, SweepPendingUploadsTask{})
		}),
		CleanupEmailLogsQueue: sweep(30*time.Second, func(ctx context.Context) error {
			return newCleanupEmailLogs(d).Process(ctx, CleanupEmailLogsTask{})
		}),
		CleanupZernioConnectSessionsQueue: sweep(30*time.Second, func(ctx context.Context) error {
			return newCleanupZernioConnectSessions(d).Process(ctx, CleanupZernioConnectSessionsTask{})
		}),
		ReconcileScheduledPostsQueue: sweep(30*time.Second, func(ctx context.Context) error {
			return newReconcileScheduledPosts(d).Process(ctx, ReconcileScheduledPostsTask{})
		}),
		DetectManualPublishDueQueue: sweep(time.Minute, func(ctx context.Context) error {
			return newDetectManualPublishDue(d).Process(ctx, DetectManualPublishDueTask{})
		}),
		BackfillAssetAnalysesKind: sweep(5*time.Minute, func(ctx context.Context) error {
			return newBackfillAssetAnalyses(d).Process(ctx)
		}),
	}
}

// sweep runs one pass across tenants within timeout.
func sweep(timeout time.Duration, run func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(tenantctx.WithSystem(ctx), timeout)
		defer cancel()
		return run(ctx)
	}
}
