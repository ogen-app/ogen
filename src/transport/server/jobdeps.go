package server

import (
	"time"

	"github.com/ogen-app/ogen/src/jobs/queues"
)

// queueDeps is the dependency bundle the job RPCs serve the jobs' steps and
// maintenance passes with.
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
		// Holds off a tenant restore while its Zernio profile is torn down.
		TeardownLease:         r.tenantFence,
		PDF:                   d.ingest.pdf,
		Document:              d.ingest.document,
		Audio:                 d.ingest.audio,
		Image:                 d.ingest.image,
		URL:                   d.ingest.url,
		Analysis:              queues.AnalysisDeps{Runner: d.analysis, Runs: d.analysis, Tenants: r.tenantRepo, BackfillBatch: cfg.AnalysisBackfillBatch},
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
		Jobs:                  d.enqueuer,
	}
}

// zernioQueueDeps is the Zernio publish/sync jobs' dependency set.
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
