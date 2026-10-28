package server

import (
	"time"

	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/weeklyreport"
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
		TeardownLease: r.tenantFence,
		// The unpaid-trial teardown re-checks billing before each delete; the
		// billing jobs run over the same usecase.
		BillingLive:           d.billing,
		BillingSteps:          d.billing,
		BillingPayments:       d.billing,
		Billing:               d.billing,
		PDF:                   d.ingest.pdf,
		Document:              d.ingest.document,
		Audio:                 d.ingest.audio,
		Image:                 d.ingest.image,
		URL:                   d.ingest.url,
		Analysis:              queues.AnalysisDeps{Runner: d.analysis, Runs: d.analysis, Distiller: d.analysis, DistillRuns: d.analysis, Tenants: r.tenantRepo, BackfillBatch: cfg.AnalysisBackfillBatch},
		BrandDistill:          queues.BrandDistillDeps{Runner: d.brandDistill, Runs: d.brandDistill, Tenants: r.tenantRepo},
		Email:                 d.email.Deps,
		WeeklyReport:          weeklyReportDeps(d),
		ConnectSessionRepo:    r.zernioConnectSessionRepo,
		PluginPairingRepo:     r.pluginPairingRepo,
		KnownDeviceRepo:       r.knownDeviceRepo,
		SessionRepo:           r.sessionRepo,
		LoginAlertTokenRepo:   r.loginAlertTokenRepo,
		OAuthRepo:             r.oauthRepo,
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

// weeklyReportDeps wires the weekly report jobs. The analytics, activity and
// limit readers are optional: a missing one only drops its section. With
// WEEKLY_REPORT_ENABLED off both jobs are no-ops, since river-service keeps
// the dispatch schedule whatever the flag.
func weeklyReportDeps(d *deps) queues.WeeklyReportDeps {
	cfg, r := d.cfg, d.r
	if !cfg.WeeklyReportEnabled {
		return queues.WeeklyReportDeps{}
	}
	collector := &weeklyreport.Collector{
		Store:      r.weeklyReportRepo,
		Analytics:  r.postAnalyticsRepo,
		Followers:  r.followerStatsRepo,
		Activity:   d.activity.events,
		Goals:      overview.New(r.campaignRepo, r.postRepo, r.platformRepo),
		Limits:     d.entitlements.limiter,
		LimitPct:   cfg.WeeklyReportLimitPct,
		ExpiryLead: time.Duration(cfg.ConnectionExpiryLeadDays) * 24 * time.Hour,
	}
	return queues.WeeklyReportDeps{
		Store:      r.weeklyReportRepo,
		Settings:   r.settingRepo,
		Collector:  collector,
		EmailLogs:  r.emailLogRepo,
		AppBaseURL: cfg.AppBaseURL,
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
