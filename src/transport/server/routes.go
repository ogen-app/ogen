package server

import (
	"context"
	"expvar"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"

	"github.com/ogen-app/ogen/src/infra/publishers"
	pubzernio "github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/netguard"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/linkpreview"
)

// linkPreviewTimeout bounds one outbound page fetch for a link preview.
const linkPreviewTimeout = 8 * time.Second

// registerRoutes mounts every HTTP handler. The API serves only /api/* (plus
// SSE and /debug/vars); the SPA is deployed separately.
//
// Fiber matches routes in registration order, so handlers sharing a path
// prefix keep their relative order: registerCampaignRoutes precedes
// registerPostRoutes (which also serves /api/campaigns/:campaign_id/posts),
// and within each function the order is deliberate.
func registerRoutes(app *fiber.App, d *deps) {
	registerBillingRoutes(app, d)
	registerNotificationRoutes(app, d)
	registerSystemRoutes(app, d)
	registerAuthRoutes(app, d)
	registerIntegrationRoutes(app, d)
	registerContentBankRoutes(app, d)
	registerCampaignRoutes(app, d)
	registerPostRoutes(app, d)
	registerAnalyticsRoutes(app, d)
}

// registerBillingRoutes serves the public pricing catalog, the in-app
// entitlement view, and usage metering.
func registerBillingRoutes(app *fiber.App, d *deps) {
	e := d.entitlements
	handlers.NewPricingHandler(e.resolver, d.r.tierVersionRepo, e.catalog, d.auth, e.limiter).Register(app)
	handlers.NewUsageHandler(d.usage.events, d.usage.limits, d.usage.defaults, d.auth, d.cfg.UsageAdminToken).Register(app)
}

// registerNotificationRoutes serves the ephemeral event stream, the durable
// per-user inbox, and operator announcements. The hub is shared with the
// internal gRPC server, so an operator tier change reaches open tabs.
func registerNotificationRoutes(app *fiber.App, d *deps) {
	r := d.r
	handlers.NewEventsHandler(d.hub, r.sessionRepo, d.auth, 0).Register(app)
	handlers.NewNotificationsHandler(r.notificationRepo, d.hub, r.sessionRepo, d.auth, 0).Register(app)
	handlers.NewAnnouncementsHandler(r.announcementRepo, r.tenantRepo, d.auth).Register(app)
}

// registerSystemRoutes serves health, settings, the auto-publish allowlist and
// expvar counters. Secrets have no REST surface: they are managed only over
// the internal gRPC server.
func registerSystemRoutes(app *fiber.App, d *deps) {
	r := d.r
	handlers.NewHealthHandler(d.db, d.secretStore).Register(app)
	handlers.NewSettingsHandler(r.settingRepo, d.auth).Register(app)
	handlers.NewAutoPublishAllowlistHandler(r.autoPublishAllowlistRepo, d.auth).Register(app)
	app.Get("/debug/vars", d.auth, adaptor.HTTPHandler(expvar.Handler()))
}

// registerAuthRoutes serves users, sessions, signup, workspaces, password
// reset, invitations and the new-device login alert links.
func registerAuthRoutes(app *fiber.App, d *deps) {
	cfg, r, rec, lim := d.cfg, d.r, d.activity.recorder, d.entitlements.limiter
	// Secure cookies everywhere but debug mode, so localhost over plain HTTP
	// still works.
	secure := !cfg.Debug
	handlers.NewUsersHandler(d.db, r.userRepo, r.accountRepo, r.settingRepo, d.auth, rec, lim).Register(app)

	sessions := handlers.NewSessionsHandler(r.userRepo, r.accountRepo, r.sessionRepo, cfg.SessionCookieName, secure, rec)
	sessions.SetLoginSecurity(d.svc.loginSecurity, cfg.DeviceCookieName)
	sessions.Register(app)
	tenants := handlers.NewTenantsHandler(d.svc.signup, r.tenantRepo, cfg.SessionCookieName, secure, d.auth, rec)
	tenants.SetLoginSecurity(d.svc.loginSecurity, cfg.DeviceCookieName)
	tenants.Register(app)
	handlers.NewLoginAlertsHandler(d.svc.loginSecurity).Register(app)

	handlers.NewWorkspacesHandler(d.db, r.workspaceRepo, r.userRepo, r.accountRepo, r.tenantRepo, r.sessionRepo, d.enqueuer, d.auth, rec).Register(app)
	handlers.NewPasswordResetHandler(d.db, r.userRepo, r.accountRepo, cfg.AppBaseURL, d.enqueuer, rec).Register(app)
	handlers.NewInvitationsHandler(d.db, r.userRepo, r.accountRepo, r.tenantRepo, r.invitationRepo, r.sessionRepo, cfg.AppBaseURL, cfg.SessionCookieName, secure, d.auth, handlers.InvitationsOptions{
		EmailJobs:        d.enqueuer,
		Limiter:          lim,
		Activity:         rec,
		LoginSecurity:    d.svc.loginSecurity,
		DeviceCookieName: cfg.DeviceCookieName,
	}).Register(app)
}

// registerIntegrationRoutes serves the Zernio integration, the Figma plugin
// (its own token-authenticated API plus the web app's approval and
// connection routes) and the public email endpoints (unsubscribe, Resend
// webhook). All are registered unconditionally and report themselves disabled
// until their secret is set.
func registerIntegrationRoutes(app *fiber.App, d *deps) {
	z, r := d.zernio, d.r
	handlers.NewZernioHandler(z.Integration, z.Bootstrapper, z.Settings, r.platformRepo, r.socialAccountRepo, r.postRepo, z.Worker, z.RateLimiter, d.auth, r.zernioConnectSessionRepo, d.cipher, d.cfg.AppBaseURL).Register(app)
	handlers.NewFigmaPluginHandler(handlers.FigmaPluginDeps{
		Pairing:       d.svc.plugins,
		AppBaseURL:    d.cfg.AppBaseURL,
		Tokens:        r.pluginTokenRepo,
		Users:         r.userRepo,
		Posts:         r.postRepo,
		Platforms:     r.platformRepo,
		Assets:        d.newAssetsHandler(),
		Attachments:   d.newPostAttachmentsHandler(),
		Activity:      d.activity.recorder,
		MaxVideoBytes: d.cfg.PluginMaxVideoBytes,

		PostAttachments: r.postAttachmentRepo,
		MediaPreviews:   r.mediaPreviewRepo,
		Storage:         d.store,
		Previews:        d.clients.previewRenderer(),
	}).Register(app)
	handlers.NewFigmaConnectionsHandler(d.svc.plugins, r.userRepo, d.auth, d.activity.recorder).Register(app)
	d.email.Handler.Register(app)
	d.email.Webhook.Register(app)
}

// registerContentBankRoutes serves assets (the three asset handlers share
// /api/content-bank/assets), brand materials and ideas. A disabled ingestion
// kind gets a true-nil enqueuer, so its upload fails fast (or, for PDF, the
// asset stays pending) instead of stranding work.
func registerContentBankRoutes(app *fiber.App, d *deps) {
	r, in, lim := d.r, d.ingest, d.entitlements.limiter
	var (
		imgJobs   handlers.ImageIngestEnqueuer
		audioJobs handlers.AudioIngestEnqueuer
	)
	if in.imageOn {
		imgJobs = d.enqueuer
	}
	if in.audioOn {
		audioJobs = d.enqueuer
	}

	d.newAssetsHandler().Register(app)
	handlers.NewAudioAssetsHandler(r.pieceRepo, r.assetFileRepo, r.audioExtractionRepo, r.audioSegmentRepo, r.utteranceRepo, d.store, d.db, audioJobs, d.auth, lim).Register(app)
	handlers.NewAssetsImageHandler(r.pieceRepo, r.assetFileRepo, r.imageExtractionRepo, r.imageBlockRepo, d.store, d.db, imgJobs, d.clients.imagePreparer(), d.usage.recorder, d.cfg.AltTextGenMaxChars, d.auth).Register(app)
	handlers.NewBrandHandler(r.brandRepo, d.store, d.auth, d.activity.recorder).Register(app)
	handlers.NewIdeasHandler(ideas.New(r.ideaRepo, r.userRepo), d.auth, d.activity.recorder).Register(app)
	handlers.NewSeriesHandler(d.svc.series, d.auth, d.activity.recorder).Register(app)
}

// registerCampaignRoutes serves campaign types and campaigns. The read
// projections register before CampaignsHandler so the static /summaries
// route wins over /:id.
func registerCampaignRoutes(app *fiber.App, d *deps) {
	r, gk, rec := d.r, d.genkit, d.activity.recorder
	handlers.NewCampaignTypesHandler(r.campaignTypeRepo, d.auth).Register(app)
	handlers.NewCampaignReadHandler(d.svc.campaignOverview, d.svc.campaignSummaries, d.auth).Register(app)
	handlers.NewCampaignGenerationHandler(r.campaignRepo, gk.GeneratePosts, d.cfg.GeneratePostsMax, gk.CheckBrief, gk.CheckPosts, gk.IsAnthropicAvailable, rec, d.auth).Register(app)
	handlers.NewCampaignsHandler(r.campaignRepo, r.campaignTypeRepo, d.auth, gk.GenerateDraft, gk.IsAnthropicAvailable, gk.EnrichBrief, r.campaignMessageRepo, gk.RunCampaignAssistant, handlers.CampaignsOptions{
		Limiter:  d.entitlements.limiter,
		Activity: rec,
		Brands:   r.brandRepo,
		Withdraw: d.svc.withdraw,
	}).Register(app)
	handlers.NewCampaignPhasesHandler(r.campaignRepo, rec, d.auth).Register(app)
}

// registerPostRoutes serves the platform and tag catalogs and everything
// under /api/posts. The focused post handlers register before PostsHandler,
// and attachments and notes after it.
func registerPostRoutes(app *fiber.App, d *deps) {
	r, gk, rec := d.r, d.genkit, d.activity.recorder
	handlers.NewPlatformsHandler(r.platformRepo, d.publishers(), r.autoPublishAllowlistRepo, d.auth).Register(app)
	handlers.NewTagsHandler(r.tagRepo, d.auth).Register(app)

	handlers.NewPostAssistantHandler(gk.RunPostAssistant, gk.IsAnthropicAvailable, r.postMessageRepo, rec, d.auth).Register(app)
	handlers.NewPostActionsHandler(r.postRepo, d.svc.clone, d.svc.restore, rec, d.auth).Register(app)
	handlers.NewPostInsightsHandler(r.postRepo, gk.AssessPostQuality, r.postEvaluationRepo, r.postAnalyticsRepo, gk.IsAnthropicAvailable, rec, d.auth).Register(app)
	handlers.NewPostVerificationHandler(r.postRepo, d.zernio.Integration.Client, r.socialAccountRepo, d.zernioProfileID, r.postAnalyticsRepo, r.postVersionRepo, d.hub, d.auth).Register(app)
	handlers.NewPostsHandler(r.postRepo, r.postVersionRepo, r.platformRepo, r.postAttachmentRepo, d.auth, handlers.PostsOptions{
		Brands:         r.brandRepo,
		Campaigns:      r.campaignRepo,
		PostLogs:       r.postLogRepo,
		Allowlist:      r.autoPublishAllowlistRepo,
		Jobs:           d.enqueuer,
		DB:             d.db,
		Schedule:       d.svc.schedule,
		Activity:       rec,
		OnBeforeDelete: deleteAttachmentBlobs(d.store, r.postAttachmentRepo),
		Storage:        d.store,
		Series:         d.svc.series,
		Withdraw:       d.svc.withdraw,
	}).Register(app)
	handlers.NewPostLogsHandler(r.postLogRepo, r.postRepo, d.auth).Register(app)
	handlers.NewLinkPreviewHandler(linkpreview.New(netguard.SafeClient(linkPreviewTimeout), netguard.ResolveAllowed), d.auth).Register(app)

	handlers.NewImagesHandler(d.store, d.auth).Register(app)
	d.newPostAttachmentsHandler().Register(app)
	handlers.NewPostNotesHandler(d.svc.notes, r.postRepo, d.auth, rec).Register(app)
}

// registerAnalyticsRoutes serves post analytics under their own /api/analytics
// group (the post overview and follower series from the DB, insight
// aggregates live-proxied to Zernio) and the activity daily reports.
func registerAnalyticsRoutes(app *fiber.App, d *deps) {
	r := d.r
	handlers.NewAnalyticsHandler(r.postAnalyticsRepo, r.followerStatsRepo, r.postRepo, r.platformRepo, r.socialAccountRepo, r.campaignRepo, d.zernio.Integration.Client, d.zernioProfileID, d.auth).Register(app)
	handlers.NewActivityHandler(d.svc.activityReport, d.auth).Register(app)
}

// newAssetsHandler builds the content-bank asset handler. A disabled ingestion
// kind gets a true-nil enqueuer, so its upload fails fast (or, for PDF, the
// asset stays pending) instead of stranding work. The handler holds no
// per-instance state, so the plugin API builds its own for image ingest.
func (d *deps) newAssetsHandler() *handlers.AssetsHandler {
	r, in := d.r, d.ingest
	var (
		pdfJobs handlers.PDFIngestEnqueuer
		docJobs handlers.DocumentIngestEnqueuer
		imgJobs handlers.ImageIngestEnqueuer
		reembed handlers.ImageReembedEnqueuer
	)
	if in.pdfOn {
		pdfJobs = d.enqueuer
	}
	if in.documentOn {
		docJobs = d.enqueuer
	}
	if in.imageOn {
		imgJobs, reembed = d.enqueuer, d.enqueuer
	}
	return handlers.NewAssetsHandler(r.pieceRepo, r.assetFileRepo, r.assetImageRepo, d.store, d.db, pdfJobs, d.enqueuer, in.firecrawl, docJobs, imgJobs, d.auth, in.embedCallbacks.OnMarkdownSave, handlers.AssetsOptions{
		Limiter:      d.entitlements.limiter,
		Chunks:       r.chunksRepo,
		ImageReembed: reembed,
	})
}

// newPostAttachmentsHandler builds the post attachments handler with
// attach-from-bank, attachment events and pending-upload tracking enabled. Stateless, so the plugin API builds its own.
func (d *deps) newPostAttachmentsHandler() *handlers.PostAttachmentsHandler {
	r := d.r
	return handlers.NewPostAttachmentsHandler(r.postAttachmentRepo, r.postRepo, d.store, d.clients.pdfRenderer(), d.clients.videoProber(), d.clients.imagePreparer(), d.usage.recorder, d.cfg.AltTextGenMaxChars, d.auth, d.entitlements.limiter).WithContentBank(r.pieceRepo).WithEventHub(d.hub).WithPendingUploads(r.pendingUploadRepo)
}

// publishers lists the publishers the platforms handler reports on. The
// Zernio publisher resolves its API key per call, so it works once a key is
// set with no reboot.
func (d *deps) publishers() []publishers.Publisher {
	var pubs []publishers.Publisher
	if d.zernio.Integration != nil && d.zernio.Settings != nil {
		pubs = append(pubs, pubzernio.NewPublisher(d.zernio.Integration, d.r.socialAccountRepo, d.zernio.Settings))
	}
	return pubs
}

// deleteAttachmentBlobs removes a post's attachment objects from storage
// before the post row is deleted; FK cascade removes the attachment rows. The
// post's post-attachments/<id>/ folder goes too, which catches presigned
// video uploads that were never finalized into a row.
func deleteAttachmentBlobs(store storage.Storage, attachments repository.PostAttachmentRepository) func(ctx context.Context, postID string) error {
	return func(ctx context.Context, postID string) error {
		if store == nil {
			return nil
		}
		keys, err := attachments.ListS3KeysByPostID(ctx, postID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if k == "" {
				continue
			}
			if err := store.Delete(ctx, k); err != nil {
				return err
			}
		}
		return store.DeletePrefix(ctx, storage.TenantKey(ctx, "post-attachments/"+postID+"/"))
	}
}
