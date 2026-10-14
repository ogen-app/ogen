package server

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"

	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/config"
)

// newRiverClient migrates River's schema and builds the job client over the
// bun pool's database/sql handle, so an enqueue can join a caller's
// transaction.
func newRiverClient(ctx context.Context, d *deps) (*river.Client[*sql.Tx], error) {
	workers := river.NewWorkers()
	queues.RegisterAll(workers, queueDeps(d))

	if err := jobs.MigrateRiver(ctx, d.db.DB); err != nil {
		return nil, err
	}
	return river.NewClient[*sql.Tx](riverdatabasesql.New(d.db.DB), &river.Config{
		Logger: slog.Default(),
		// Each job runs in a root tracing span; exhausted-retry failures are
		// reported to Sentry.
		Middleware: jobs.Middleware(),
		// Past the soft stop, River cancels the jobs still running; the drain
		// middleware reschedules them for the replacement replica.
		SoftStopTimeout: softStopTimeout(d.cfg),
		// Dedicated audio, image and ingest queues keep long transcription, vision
		// and document runs from starving short jobs (publishing, email) on the
		// default queue.
		Queues:       queues.QueueConfigs(d.cfg.JobWorkers, d.cfg.AudioJobWorkers, d.cfg.ImageJobWorkers, d.cfg.IngestJobWorkers, d.cfg.AnalysisJobWorkers),
		Workers:      workers,
		PeriodicJobs: periodicJobs(d),
	})
}

// periodicJobs is ogen's periodic set: every sweep river-service does not own.
func periodicJobs(d *deps) []*river.PeriodicJob {
	pc := periodicConfig(d.cfg)
	pc.Relayed = d.enqueuer.Relays
	return pc.PeriodicJobs()
}

// periodicConfig schedules the sweeps. The analytics, follower and
// connection-health sweeps are profile-driven, so they no-op until Zernio is
// configured and start producing once a key is set, with no reboot.
func periodicConfig(cfg *config.Config) queues.PeriodicConfig {
	return queues.PeriodicConfig{
		CleanupEvery:               time.Hour,
		EmailCleanupEvery:          time.Hour,
		ReconcileEvery:             5 * time.Minute,
		AnalyticsEvery:             cfg.ZernioAnalyticsRefreshInterval,
		IncludeAnalytics:           true,
		FollowerEvery:              cfg.ZernioFollowerRefreshInterval,
		IncludeFollowers:           true,
		ConnectSessionCleanupEvery: 15 * time.Minute,
		PluginPairingCleanupEvery:  15 * time.Minute,
		HealthCheckEvery:           cfg.ZernioHealthCheckInterval,
		IncludeConnectionExpiry:    true,
		NotificationCleanupEvery:   cfg.NotificationsCleanupEvery,
		ManualPublishDueEvery:      cfg.ManualPublishDueSweepEvery,
		LoginSecurityCleanupEvery:  24 * time.Hour,
		OrphanSweepEvery:           cfg.ZernioOrphanSweepInterval,
		PendingUploadSweepEvery:    cfg.PendingUploadSweepInterval,
		AnalysisBackfillEvery:      cfg.AnalysisBackfillInterval,
	}
}

// queueDeps is the dependency bundle every River worker is registered with.
func queueDeps(d *deps) queues.Deps {
	cfg, r := d.cfg, d.r
	return queues.Deps{
		Zernio:           zernioQueueDeps(d),
		PostLogRetention: time.Duration(cfg.PostLogRetentionDays) * 24 * time.Hour,
		ReconcileGrace:   cfg.ReconcileGrace,
		OrphanSweep: queues.OrphanSweepConfig{
			Live:   cfg.ZernioOrphanSweepLive,
			MinAge: cfg.ZernioOrphanSweepMinAge,
		},
		PendingUploadSweep: queues.PendingUploadSweepConfig{
			Live:  cfg.PendingUploadSweepLive,
			Grace: cfg.PendingUploadSweepGrace,
		},
		PendingUploadRepo:   r.pendingUploadRepo,
		AnalyticsSettings:   d.zernio.Settings,
		AnalyticsHub:        d.hub,
		AnalyticsWindowDays: cfg.ZernioAnalyticsWindowDays,
		// New posts are refreshed often and settled posts rarely, so snapshot
		// writes stay proportional to how fast a post's numbers still move.
		AnalyticsDecay: queues.AnalyticsDecay{
			FreshWindow: cfg.ZernioAnalyticsFreshWindow,
			WarmWindow:  cfg.ZernioAnalyticsWarmWindow,
			FreshEvery:  cfg.ZernioAnalyticsFreshEvery,
			WarmEvery:   cfg.ZernioAnalyticsWarmEvery,
			ColdEvery:   cfg.ZernioAnalyticsColdEvery,
		},
		ProfileBootstrapper: d.zernio.Bootstrapper,
		Integration:         d.zernio.Integration,
		// Fences the profile teardown against a concurrent tenant restore.
		TenantFence:           r.tenantFence,
		TeardownLease:         r.tenantFence,
		PDF:                   d.ingest.pdf,
		Document:              d.ingest.document,
		Audio:                 d.ingest.audio,
		Image:                 d.ingest.image,
		URL:                   d.ingest.url,
		Analysis:              queues.AnalysisDeps{Runner: d.analysis, Tenants: r.tenantRepo, BackfillBatch: cfg.AnalysisBackfillBatch},
		Email:                 d.email.Deps,
		ConnectSessionRepo:    r.zernioConnectSessionRepo,
		PluginPairingRepo:     r.pluginPairingRepo,
		KnownDeviceRepo:       r.knownDeviceRepo,
		SessionRepo:           r.sessionRepo,
		LoginAlertTokenRepo:   r.loginAlertTokenRepo,
		Tenants:               r.tenantRepo,
		Users:                 r.userRepo,
		AppBaseURL:            cfg.AppBaseURL,
		ExpiryLeadDays:        cfg.ConnectionExpiryLeadDays,
		Notifier:              d.notifier,
		NotificationRepo:      r.notificationRepo,
		NotificationRetention: time.Duration(cfg.NotificationsRetentionDays) * 24 * time.Hour,
		// An empty URL disables the signed new-tenant webhook.
		Jobs:         d.enqueuer,
		HarborNotify: queues.HarborNotifyDeps{URL: cfg.HarborWebhookURL, Secret: cfg.HarborWebhookSecret},
	}
}

// zernioQueueDeps is the Zernio publish/sync workers' dependency set.
func zernioQueueDeps(d *deps) queues.ZernioDeps {
	r := d.r
	return queues.ZernioDeps{
		PostRepo:           r.postRepo,
		PostLogRepo:        r.postLogRepo,
		PostAttachmentRepo: r.postAttachmentRepo,
		SocialAccountRepo:  r.socialAccountRepo,
		Storage:            d.store,
		SettingRepo:        r.settingRepo,
		AnalyticsRepo:      r.postAnalyticsRepo,
		FollowerRepo:       r.followerStatsRepo,
		PlatformRepo:       r.platformRepo,
		Client:             d.zernio.Integration.Client,
		Recorder:           d.usage.recorder,
		ActivityRecorder:   d.activity.recorder,
		ProfileID:          d.zernioProfileID,
		Jobs:               d.enqueuer,
	}
}

// minJobCancelMargin is the least time the shutdown leaves between River
// cancelling running jobs and giving up on the stop, so cancelled jobs can
// record their reschedule before the process exits.
const minJobCancelMargin = 15 * time.Second

// softStopTimeout is how long River lets running jobs finish on shutdown
// before cancelling them. It is clamped below JobShutdownTimeout so the
// cancellation always happens while the stop is still being awaited.
func softStopTimeout(cfg *config.Config) time.Duration {
	ceiling := cfg.JobShutdownTimeout - minJobCancelMargin
	if ceiling <= 0 {
		return cfg.JobShutdownTimeout / 2
	}
	if cfg.JobSoftStopTimeout <= 0 || cfg.JobSoftStopTimeout > ceiling {
		return ceiling
	}
	return cfg.JobSoftStopTimeout
}
