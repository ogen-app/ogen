package queues

import (
	"context"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// CleanupPluginPairingsQueue is the recurring sweep that drops expired plugin
// pairings. Correctness never depends on it — readers treat expires_at in the
// past as gone — it only reclaims rows, including any sealed token an
// approved pairing was never collected with.
const CleanupPluginPairingsQueue = "cleanup_plugin_pairings"

// CleanupPluginPairingsTask is the marker payload.
type CleanupPluginPairingsTask struct{}

// Kind implements JobArgs.
func (CleanupPluginPairingsTask) Kind() string { return CleanupPluginPairingsQueue }

// CleanupPluginPairingsProcessor runs the sweep.
type CleanupPluginPairingsProcessor struct {
	Repo repository.PluginPairingRepository
}

func newCleanupPluginPairings(d Deps) *CleanupPluginPairingsProcessor {
	return &CleanupPluginPairingsProcessor{Repo: d.PluginPairingRepo}
}

// Process deletes pairings whose expiry has passed. A nil repo is a no-op.
func (p *CleanupPluginPairingsProcessor) Process(ctx context.Context, _ CleanupPluginPairingsTask) error {
	if p.Repo == nil {
		return nil
	}
	n, err := p.Repo.DeleteExpired(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	if n > 0 {
		jobs.PluginPairingsSwept.Add(n)
		slog.InfoContext(ctx, "swept expired plugin pairings",
			logging.AttrComponent, "jobs.cleanup_plugin_pairings", "count", n)
	}
	return nil
}
