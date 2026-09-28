// Package queues holds the typed River queues that drive Ogen's
// auto-publish pipeline.
package queues

import (
	"context"

	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/usage"
)

// ProfileIDResolver returns the Zernio profile id this single-tenant
// Ogen install operates under. Resolved lazily so the queue can boot
// before the bootstrapper has completed; an error indicates "no
// integration bootstrap yet" and the queue handler treats it as
// terminal.
type ProfileIDResolver func(ctx context.Context) (string, error)

// ZernioDeps bundles the dependency set every Zernio-related queue
// shares. Held by each processor so the processor's Process method
// can stay narrow and easy to test in isolation. Constructed once at
// boot in server.go.
type ZernioDeps struct {
	PostRepo           repository.PostRepository
	PostLogRepo        repository.PostLogRepository
	PostAttachmentRepo repository.PostAttachmentRepository
	SocialAccountRepo  repository.SocialAccountRepository
	// Storage reads attachment bytes so the submit worker can upload them to
	// Zernio's media endpoint. nil ⇒ posts are submitted text-only.
	Storage storage.Storage
	// SettingRepo backs the workspace timezone lookup used to stamp the
	// Zernio submit's Timezone field. nil falls back to UTC.
	SettingRepo repository.SettingRepository
	// AnalyticsRepo appends the analytics snapshots the refresh queue writes
	// (CON-93 §6 FR2; append-only per CON-125). Only the refresh queue uses it;
	// nil on the submit/poll/cancel processors that share this bundle.
	AnalyticsRepo repository.PostAnalyticsRepository
	// FollowerRepo upserts the daily follower snapshots the follower-refresh
	// queue writes. Only that queue uses it; nil disables the sweep.
	FollowerRepo repository.FollowerStatsRepository
	// PlatformRepo resolves platform_id → name so the refresh queue can
	// denormalise the platform name onto each snapshot (CON-125 Track B). Only
	// the refresh queue uses it.
	PlatformRepo repository.PlatformRepository
	Client       *zernio.Client
	ProfileID    ProfileIDResolver
	// Recorder meters publish/schedule usage events. nil = no-op.
	Recorder *usage.Recorder
	// ActivityRecorder emits CON-125 publish-category activity events
	// (publish_submitted/succeeded/failed/cancelled, analytics_refreshed).
	// nil = no-op.
	ActivityRecorder *activity.Recorder
}
