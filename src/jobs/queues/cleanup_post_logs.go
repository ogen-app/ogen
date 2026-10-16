package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// CleanupPostLogsQueue is the recurring sweep that drops Post Log entries
// older than the configured retention window. river-service schedules it;
// ogen runs each pass (MaintenanceService).
const CleanupPostLogsQueue = "cleanup_post_logs"

// CleanupPostLogsTask is the typed payload for the queue. There's
// nothing to carry — the worker just looks at the cutoff window — but
// River still requires a per-kind args type so we satisfy JobArgs with
// a marker struct.
type CleanupPostLogsTask struct{}

// Kind implements JobArgs.
func (CleanupPostLogsTask) Kind() string { return CleanupPostLogsQueue }

// CleanupPostLogsProcessor is the cleanup_post_logs maintenance pass: it
// deletes Post Logs older than `now - retention`. river-service schedules it.
type CleanupPostLogsProcessor struct {
	Repo      repository.PostLogRepository
	Retention time.Duration
}

func newCleanupPostLogs(d Deps) *CleanupPostLogsProcessor {
	return &CleanupPostLogsProcessor{Repo: d.Zernio.PostLogRepo, Retention: d.PostLogRetention}
}

// Process deletes Post Log entries older than the retention window.
func (p *CleanupPostLogsProcessor) Process(ctx context.Context, _ CleanupPostLogsTask) error {
	if p.Retention > 0 && p.Repo != nil {
		cutoff := time.Now().UTC().Add(-p.Retention)
		n, err := p.Repo.DeleteOlderThan(ctx, cutoff)
		if err != nil {
			return err
		}
		if n > 0 {
			jobs.PostLogCleaned.Add(n)
			slog.InfoContext(ctx, "cleanup removed entries", logging.AttrComponent, "jobs.cleanup_post_logs", "count", n, "cutoff", cutoff.Format(time.RFC3339))
		}
	}
	return nil
}
