package queues

import (
	"context"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// MaintenanceTasks returns the housekeeping sweeps river-service schedules and
// ogen runs, keyed by kind (jobrpc.v1 MaintenanceService). Each runs exactly as
// its River worker does, built from the same deps, under the worker's own
// timeout.
func MaintenanceTasks(d Deps) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		CleanupPostLogsQueue:              sweepRunner(newCleanupPostLogs(d), CleanupPostLogsTask{}),
		CleanupNotificationsQueue:         sweepRunner(newCleanupNotifications(d), CleanupNotificationsTask{}),
		CleanupLoginSecurityQueue:         sweepRunner(newCleanupLoginSecurity(d), CleanupLoginSecurityTask{}),
		CleanupPluginPairingsQueue:        sweepRunner(newCleanupPluginPairings(d), CleanupPluginPairingsTask{}),
		SweepPendingUploadsQueue:          sweepRunner(newSweepPendingUploads(d), SweepPendingUploadsTask{}),
		CleanupEmailLogsQueue:             sweepRunner(newCleanupEmailLogs(d), CleanupEmailLogsTask{}),
		CleanupZernioConnectSessionsQueue: sweepRunner(newCleanupZernioConnectSessions(d), CleanupZernioConnectSessionsTask{}),
		ReconcileScheduledPostsQueue:      sweepRunner(newReconcileScheduledPosts(d), ReconcileScheduledPostsTask{}),
		DetectManualPublishDueQueue:       sweepRunner(newDetectManualPublishDue(d), DetectManualPublishDueTask{}),
	}
}

func sweepRunner[T river.JobArgs](w river.Worker[T], args T) func(context.Context) error {
	return func(ctx context.Context) error {
		job := &river.Job[T]{JobRow: &rivertype.JobRow{Kind: args.Kind()}, Args: args}
		if d := w.Timeout(job); d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return w.Work(ctx, job)
	}
}
