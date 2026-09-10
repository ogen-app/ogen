package server

import (
	"log/slog"
	"sync"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// activityMetrics is registered once per process: expvar.NewInt panics on a
// duplicate name, so repeated server.New calls (integration tests) must share
// one Metrics instance rather than re-register the ogen_activity_* counters.
var sharedActivityMetrics = sync.OnceValue(activity.NewMetrics)

// activityDeps bundles the wired CON-125 activity-collection pieces. Both are
// nil when analytics is disabled — the recorder is nil-safe, so call-sites stay
// unconditional.
type activityDeps struct {
	recorder *activity.Recorder
	events   repository.ActivityRepository // analytics pool; nil when disabled
}

// initActivity wires the activity-collection layer. When analyticsDB is nil
// (ANALYTICS_DSN empty, or the analytics DB was unreachable at boot) the
// recorder/events are left nil: call-sites then record nothing — the
// graceful-disable path. The caller drains the recorder on shutdown via
// recorder.Close, registered AFTER the river/zernio producers stop so loop()
// never exits while a worker is still calling Record() (see server.New).
func initActivity(cfg *config.Config, analyticsDB *bun.DB) activityDeps {
	if analyticsDB == nil {
		slog.Warn("activity collection disabled (ANALYTICS_DSN empty)",
			logging.AttrComponent, "activity")
		return activityDeps{}
	}

	events := repository.NewActivityRepository(analyticsDB)
	recorder := activity.NewRecorder(events, sharedActivityMetrics(), activity.Config{})

	slog.Info("activity collection enabled",
		logging.AttrComponent, "activity")
	return activityDeps{recorder: recorder, events: events}
}
