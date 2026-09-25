package server

import (
	"context"
	"database/sql"
	"errors"
	"expvar"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/domain/modelconfig"
	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/firecrawl"
	"github.com/ogen-app/ogen/src/infra/publishers"
	pubzernio "github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/kernel/usage"
	audioclient "github.com/ogen-app/ogen/src/transport/grpc/client/audio"
	"github.com/ogen-app/ogen/src/transport/grpc/client/documents"
	imageclient "github.com/ogen-app/ogen/src/transport/grpc/client/image"
	"github.com/ogen-app/ogen/src/transport/grpc/client/pdf"
	"github.com/ogen-app/ogen/src/transport/grpc/client/video"
	"github.com/ogen-app/ogen/src/transport/handlers"
	activityreport "github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/summaries"
	"github.com/ogen-app/ogen/src/usecase/ideas"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// TODO: refactor this function
func New(ctx context.Context, db, analyticsDB *bun.DB, cfg *config.Config, secretStore secrets.Store, hub eventhub.Hub) (*fiber.App, error) {
	// Opt-in pprof for perf diagnostics (CON-112). Container-internal only.
	if cfg.EnablePprof {
		startPprof("localhost:6060")
	}

	app := newFiberApp(cfg)

	// API routes: the full data-access layer is built once by wireRepositories.
	r := wireRepositories(db, analyticsDB)
	// CON-292: load the operator-controlled platform catalog into the Zernio
	// resolver (it replaced the deleted hardcoded registry) and keep it fresh.
	// Non-fatal if the initial load fails — a background tick retries.
	pubzernio.InitCatalog(ctx, r.platformRepo)
	// CON-292: load the operator-controlled global upload/thread ceilings into
	// the cached config the attachment handlers and thread validator read.
	platforms.InitGlobalLimits(ctx, r.platformGlobalLimitsRepo)
	// CON-308: load the per-flow/per-tier model configuration into the cached
	// resolver the genkit flows read (Phase 4). Seeds any missing global-default
	// row from the legacy config fields so day-one behaviour is byte-identical,
	// then keeps the snapshot fresh. tierOf prefers a ctx-stamped tier and falls
	// back to the caller's tenant tier (a rare, LLM-call-time lookup).
	modelconfig.Init(ctx, r.flowModelConfigRepo,
		modelconfig.Defaults{
			Generation: cfg.ModelID,
			Quality:    cfg.QualityModelID,
			Planning:   cfg.PlanningModelID,
			Embed:      cfg.EmbedModel,
		},
		vendors.VendorOf,
		func(ctx context.Context) (string, bool) {
			if t, ok := tenantctx.TierFrom(ctx); ok {
				return t, true
			}
			tid, ok := tenantctx.From(ctx)
			if !ok {
				return "", false
			}
			t, err := r.tenantRepo.GetByID(ctx, tid)
			if err != nil || t == nil || t.TierID == "" {
				return "", false
			}
			return t.TierID, true
		},
	)
	// CON-308 boot diagnostic: enumerate the model catalog this build ships, so an
	// operator can confirm at startup which models ListModels / the resolver expose.
	// A short catalog in the logs points at a stale binary, not a config problem.
	{
		var modelIDs []string
		for _, d := range vendors.ByFamily(vendors.FamilyModel) {
			for id := range d.Prices.Models {
				modelIDs = append(modelIDs, d.Name+"/"+id)
			}
		}
		sort.Strings(modelIDs)
		slog.InfoContext(ctx, "model catalog loaded", logging.AttrComponent, "modelconfig",
			"count", len(modelIDs), "models", modelIDs)
	}
	// CON-113: one overview service, shared by the REST endpoint and the
	// Campaign Assistant's getCampaignOverview tool. Not gated by the Anthropic
	// key — it's a plain tenant-scoped DB read.
	campaignOverviewSvc := overview.New(r.campaignRepo, r.postRepo, r.platformRepo)
	// CON-152: batched Campaigns-list summaries — one tenant-scoped read that
	// replaces the per-card GET /:id/posts N+1 (CON-127).
	campaignSummariesSvc := summaries.New(r.postRepo)
	// CON-285: Activity daily report — server-side per-local-day counts over live
	// post/campaign/post_logs data (tenant-scoped repos).
	activityReportSvc := activityreport.New(r.postRepo, r.postLogRepo, r.campaignRepo)

	auth := handlers.RequireAuth(r.sessionRepo, r.userRepo, cfg.SessionCookieName)

	// CON-243: versioned tier entitlements. Load the engineering-owned feature
	// catalog (boot fails if the embedded JSON is malformed), build the
	// point-in-time resolver, and serve the public pricing catalog + the in-app
	// entitlement view.
	entitlementCatalog, err := entitlements.LoadCatalog()
	if err != nil {
		return nil, err
	}
	entitlementResolver := entitlements.NewResolver(r.tierVersionRepo, r.tierAssignmentRepo, r.tenantRepo, entitlementCatalog)
	pricingHandler := handlers.NewPricingHandler(entitlementResolver, r.tierVersionRepo, entitlementCatalog, auth)
	// CON-295: entitlement quota limiter over the resolver, wired to the
	// control-plane counters for each capped feature. warn-first via config; the
	// counters ignore the explicit tenant arg because the request ctx already
	// carries it (tenant-scoped reads).
	entitlementLimiter := entitlements.NewLimiter(entitlementResolver, entitlementCatalog, entitlements.ParseMode(cfg.EntitlementEnforcementMode)).
		Register("team_seats", entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) { return r.userRepo.CountInTenant(ctx) })).
		Register("active_campaigns", entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) { return r.campaignRepo.CountActive(ctx) })).
		Register("content_bank_assets", entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) { return r.pieceRepo.Count(ctx) })).
		// CON-295: web_page_imports is a stricter sub-cap on the total bank —
		// it counts only URL-type assets (CON-222). Without its own counter the
		// pricing page sold an allowance nothing measured.
		Register("web_page_imports", entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) {
			return r.pieceRepo.CountByType(ctx, models.AssetTypeURL)
		})).
		// media_storage_bytes is "all uploaded media" (catalog): post attachments
		// plus content-bank originals (CON-312 — before, only attachments counted,
		// so a multi-GB audio upload was invisible to the cap).
		Register("media_storage_bytes", entitlements.CounterFunc(func(ctx context.Context, _ string) (int64, error) {
			att, err := r.postAttachmentRepo.SumSizeBytesInTenant(ctx)
			if err != nil {
				return 0, err
			}
			bank, err := r.assetFileRepo.SumSizeBytesInTenant(ctx)
			if err != nil {
				return 0, err
			}
			return att + bank, nil
		}))
	// CON-295: the same counters back the "N of M" usage on GET /api/me/entitlements.
	pricingHandler.SetLimiter(entitlementLimiter)
	pricingHandler.Register(app)

	// CON-86: apply any operator price-map override (USAGE_MODEL_PRICES) before
	// metering starts; a malformed payload or unknown vendor fails boot.
	if err := usage.ApplyModelPrices(cfg.UsageModelPrices); err != nil {
		return nil, err
	}
	// usage metering + per-tenant cost enforcement. recorder/checker are nil
	// when analytics is disabled — both are nil-safe in the flows.
	usageWiring := initUsage(cfg, db, analyticsDB)
	handlers.NewUsageHandler(usageWiring.events, usageWiring.limits, usageWiring.defaults, auth, cfg.UsageAdminToken).Register(app)

	// CON-125: centralised user-activity collection. Shares the analytics pool;
	// the recorder is nil (a no-op) when analytics is disabled. Call-sites emit
	// via activityWiring.recorder.Record(...). Drained on shutdown below, after
	// the job producers stop.
	activityWiring := initActivity(cfg, analyticsDB)

	// In-process event hub: backend code publishes; the SSE endpoint fans events
	// out to authenticated clients. Created by the caller (cmd/server) and shared
	// with the internal gRPC server, so an operator tier change over gRPC can
	// invalidate a tenant's open tabs on the same bus (CON-295).
	handlers.NewEventsHandler(hub, r.sessionRepo, auth, 0).Register(app)

	// CON-242: notification center. A persistent per-user inbox (REST + durable
	// SSE), fed by the notify service that producers call. Distinct from the
	// ephemeral events bus above (which loses everything on disconnect) and the
	// email channel below. The notifier is threaded into the job Deps so
	// background producers (e.g. the connection-expiry sweep) can emit; the repo
	// also backs the cleanup_notifications retention sweep.
	notifier := notify.New(r.notificationRepo, hub)
	handlers.NewNotificationsHandler(r.notificationRepo, hub, r.sessionRepo, auth, 0).Register(app)

	// CON-230: operator-authored informational announcements (banners). The
	// tenant-facing delivery + per-user click/dismiss tracking; announcements are
	// authored by Harbor over the internal gRPC surface (AnnouncementAdminService).
	// Delivery resolves the caller's active workspace to its tier + groups to
	// evaluate targeting, so it needs the tenant classification read.
	handlers.NewAnnouncementsHandler(r.announcementRepo, r.tenantRepo, auth).Register(app)

	// CON-295 §12: warn workspace owners via the durable inbox as a tenant nears a
	// numeric cap. The Limiter fires crossing-only LimitEvents; this adapter turns
	// them into notifications. Best-effort — it never affects the create path.
	entitlementLimiter.WithNotifier(&limitNotifier{notify: notifier, users: r.userRepo}, cfg.EntitlementWarnThresholdPct)

	handlers.NewHealthHandler(db, secretStore).Register(app)
	usersHandler := handlers.NewUsersHandler(db, r.userRepo, r.accountRepo, r.settingRepo, auth)
	usersHandler.SetActivityRecorder(activityWiring.recorder)
	usersHandler.SetLimiter(entitlementLimiter)
	usersHandler.Register(app)
	// CON-97 signup + CON-102 eager Zernio profile provisioning are registered
	// below, after the River enqueuer is built (signup enqueues a bootstrap job
	// in its transaction).
	// Session cookies are marked Secure in production. Debug mode is the
	// development escape hatch so localhost over plain HTTP still works.
	sessionsHandler := handlers.NewSessionsHandler(r.userRepo, r.accountRepo, r.sessionRepo, cfg.SessionCookieName, !cfg.Debug)
	sessionsHandler.SetActivityRecorder(activityWiring.recorder)
	sessionsHandler.Register(app)
	handlers.NewSettingsHandler(r.settingRepo, auth).Register(app)
	// Secrets are managed exclusively over the internal gRPC surface
	// (src/grpc/server), reached by Harbor — there is deliberately no REST CRUD
	// for them. secretStore is still used below to resolve keys at call time and
	// to power the health endpoint's resolvability report.
	handlers.NewAutoPublishAllowlistHandler(r.autoPublishAllowlistRepo, auth).Register(app)

	// Zernio integration. Ping, profile bootstrap, and the sync worker
	// all run in background goroutines so Ogen boot never blocks on
	// Zernio reachability. The shutdown hook waits up to 2s for the
	// worker to exit cleanly.
	zernioRT := initZernio(ctx, cfg, secretStore, r.settingRepo, r.socialAccountRepo, hub, usageWiring.recorder)
	// CON-217: the headless connect callback seals Zernio's short-lived
	// connect_token/tempToken at rest. Rebuild the envelope cipher from the same
	// KEK the secret store uses (idempotent — LoadOrCreateKEK reads the existing
	// file), avoiding a server.New signature change.
	connectCipher, _, err := secrets.InitCipher(cfg.KEKPath)
	if err != nil {
		return nil, err
	}
	// Registered unconditionally so /api/integrations/zernio/* (incl. the
	// unauthenticated /health) always exists. When no key is set the endpoints
	// report/return integration_disabled; setting zernio_api_key via the
	// secrets API enables it with no reboot (see initZernio's subscription).
	handlers.NewZernioHandler(
		zernioRT.Integration,
		zernioRT.Bootstrapper,
		zernioRT.Settings,
		r.platformRepo,
		r.socialAccountRepo,
		r.postRepo,
		zernioRT.Worker,
		zernioRT.RateLimiter,
		auth,
		r.zernioConnectSessionRepo,
		connectCipher,
		cfg.AppBaseURL,
	).Register(app)
	app.Hooks().OnShutdown(func() error {
		zernioRT.shutdown()
		return nil
	})

	// Build the publisher list the platforms handler will surface in its
	// enriched response. The Zernio publisher is always present (the runtime is
	// always wired); it self-reports connection state and resolves the API key
	// per call, so it works once zernio_api_key is set with no reboot.
	var pubs []publishers.Publisher
	if zernioRT.Integration != nil && zernioRT.Settings != nil {
		pubs = append(pubs, pubzernio.NewPublisher(
			zernioRT.Integration,
			r.socialAccountRepo,
			zernioRT.Settings,
		))
	}

	store, err := storage.New(cfg)
	if err != nil {
		return nil, err
	}

	// CON-103: gRPC client for the PDF parsing microservice over the Railway
	// private network. nil when PDF_SERVICE_ADDR is unset; closed on shutdown.
	pdfClient, err := pdf.New(pdf.Config{
		Addr:         cfg.PDFServiceAddr,
		Timeout:      cfg.PDFServiceTimeout,
		MaxRecvBytes: cfg.PDFServiceMaxRecvBytes,
	})
	if err != nil {
		return nil, err
	}
	if pdfClient != nil {
		app.Hooks().OnShutdown(func() error { return pdfClient.Close() })
	}

	// CON-148: gRPC client for the video probing microservice over the Railway
	// private network. nil when VIDEO_SERVICE_ADDR is unset; closed on shutdown.
	videoClient, err := video.New(video.Config{
		Addr:         cfg.VideoServiceAddr,
		Timeout:      cfg.VideoServiceTimeout,
		MaxRecvBytes: cfg.VideoServiceMaxRecvBytes,
	})
	if err != nil {
		return nil, err
	}
	if videoClient != nil {
		app.Hooks().OnShutdown(func() error { return videoClient.Close() })
	}

	// CON-280: gRPC client for the document parsing microservice over the Railway
	// private network. nil when DOCUMENTS_SERVICE_ADDR is unset; closed on shutdown.
	documentsClient, err := documents.New(documents.Config{
		Addr:         cfg.DocumentsServiceAddr,
		Timeout:      cfg.DocumentsServiceTimeout,
		MaxRecvBytes: cfg.DocumentsServiceMaxRecvBytes,
	})
	if err != nil {
		return nil, err
	}
	if documentsClient != nil {
		app.Hooks().OnShutdown(func() error { return documentsClient.Close() })
	}

	// CON-282: gRPC client for the audio transcription microservice over the
	// Railway private network. nil when AUDIO_SERVICE_ADDR is unset. Its Close
	// hook is registered LATER — after riverClient.Stop — so draining audio jobs
	// don't have their in-flight TranscribeSegment RPCs killed by an early
	// connection close (unlike pdf/video which are request-time only).
	audioClient, err := audioclient.New(audioclient.Config{
		Addr:         cfg.AudioServiceAddr,
		Timeout:      cfg.AudioServiceTimeout,
		MaxRecvBytes: cfg.AudioServiceMaxRecvBytes,
	})
	if err != nil {
		return nil, err
	}

	// CON-281: gRPC client for the image microservice over the Railway private
	// network. nil when IMAGE_SERVICE_ADDR is unset. Like audio, its Close hook is
	// registered LATER — after riverClient.Stop — so a draining process_image job's
	// in-flight Extract RPC isn't killed by an early connection close.
	imageClient, err := imageclient.New(imageclient.Config{
		Addr:         cfg.ImageServiceAddr,
		Timeout:      cfg.ImageServiceTimeout,
		MaxRecvBytes: cfg.ImageServiceMaxRecvBytes,
	})
	if err != nil {
		return nil, err
	}

	// Embedding (Gemini) is initialised here — before the River registry —
	// because the process_pdf worker (CON-103) needs the embedder in its deps.
	// The returned embedder is a stable reloadable wrapper (always non-nil): when
	// gemini_api_key is unset it reports unavailable, so PDF ingestion + semantic
	// search are dormant until a key is added via the secrets API (CON-104), with
	// no restart; markdown/JSON saves still succeed meanwhile.
	slog.Info("genkit initialising", logging.AttrComponent, "genkit", "genkit_env", os.Getenv("GENKIT_ENV"))
	embedCallbacks, embedder, err := initEmbedding(ctx, cfg, r.chunksRepo, r.pieceRepo, r.assetFileRepo, store, secretStore, usageWiring.recorder)
	if err != nil {
		return nil, err
	}

	// PDF ingestion is live when the parser and storage are present; embedder
	// availability is checked per-run by the worker (a key set later re-enables
	// it without a restart), not gated at boot. The Client field is left nil
	// otherwise so the worker no-ops.
	pdfIngestEnabled := pdfClient != nil && store != nil
	pdfDeps := queues.PDFDeps{
		Embedder:   embedder,
		Storage:    store,
		Assets:     r.pieceRepo,
		Chunks:     r.chunksRepo,
		Files:      r.assetFileRepo,
		Recorder:   usageWiring.recorder,
		EmbedModel: cfg.EmbedModel,
		Notifier:   notifier, // CON-242: asset-ingest-done producer
	}
	if pdfIngestEnabled {
		pdfDeps.Client = pdfClient
	}

	// CON-280: document ingestion mirrors PDF — live when the parser and storage
	// are present; embedder availability is checked per-run by the worker. Client
	// left nil otherwise so the worker no-ops.
	documentIngestEnabled := documentsClient != nil && store != nil
	documentDeps := queues.DocumentDeps{
		Embedder:   embedder,
		Storage:    store,
		Assets:     r.pieceRepo,
		Chunks:     r.chunksRepo,
		Files:      r.assetFileRepo,
		Recorder:   usageWiring.recorder,
		EmbedModel: cfg.EmbedModel,
		Notifier:   notifier, // CON-242: asset-ingest-done producer
	}
	if documentIngestEnabled {
		documentDeps.Client = documentsClient
	}

	// CON-282: audio ingestion mirrors document/PDF — live when the audio-service
	// client and storage are present; embedder availability is checked per-run by
	// the worker. Client left nil otherwise so the worker no-ops. Runs on the
	// dedicated `audio` River queue; the cost gate reuses the usage Checker.
	audioIngestEnabled := audioClient != nil && store != nil
	audioDeps := queues.AudioDeps{
		Embedder:         embedder,
		Storage:          store,
		Assets:           r.pieceRepo,
		Content:          r.pieceRepo,
		Chunks:           r.chunksRepo,
		Extractions:      r.audioExtractionRepo,
		Segments:         r.audioSegmentRepo,
		Utterances:       r.utteranceRepo,
		Recorder:         usageWiring.recorder,
		Checker:          usageWiring.checker,
		EmbedModel:       cfg.EmbedModel,
		TranscribeModel:  cfg.TranscribeModel,
		SegmentMaxMs:     cfg.AudioSegmentMaxMs,
		SegmentOverlapMs: cfg.AudioSegmentOverlapMs,
		MaxDurationMs:    cfg.AudioMaxDurationMs,
		JobTimeout:       cfg.AudioJobTimeout,
		Notifier:         notifier, // CON-242: asset-ingest-done producer
	}
	if audioIngestEnabled {
		audioDeps.Client = audioClient
	}

	// CON-281: image ingestion mirrors audio — live when the image-service client
	// and storage are present; embedder availability is checked per-run by the
	// worker. Client left nil otherwise so the worker no-ops. Runs on the dedicated
	// `image` River queue; the cost gate reuses the usage Checker.
	imageIngestEnabled := imageClient != nil && store != nil
	imageDeps := queues.ImageDeps{
		Embedder:            embedder,
		Storage:             store,
		Assets:              r.pieceRepo,
		Chunks:              r.chunksRepo,
		Files:               r.assetFileRepo,
		Extractions:         r.imageExtractionRepo,
		Blocks:              r.imageBlockRepo,
		Recorder:            usageWiring.recorder,
		Checker:             usageWiring.checker,
		EmbedModel:          cfg.EmbedModel,
		ClassifyModel:       cfg.VisionClassifyModel,
		ExtractModel:        cfg.VisionExtractModel,
		EscalateModel:       cfg.VisionEscalateModel,
		ConfidenceThreshold: cfg.VisionConfidenceThreshold,
		AltTextMaxChars:     cfg.AltTextGenMaxChars,
		JobTimeout:          cfg.ImageJobTimeout,
		Notifier:            notifier, // CON-242: asset-ingest-done producer
	}
	if imageIngestEnabled {
		imageDeps.Client = imageClient
	}

	// CON-222: URL assets. The Firecrawl scrape client resolves firecrawl_api_key
	// per request (hot-reload without restart, like Resend), so a key added via
	// the secrets API enables the process_url worker + the /url endpoint with no
	// reboot; an unset key leaves both dormant (the endpoint returns 409). Storage
	// is optional — without it images stay as external links (best-effort mirror).
	firecrawlClient := firecrawl.New(
		func(ctx context.Context) (string, error) { return secretStore.Get(ctx, secrets.NameFirecrawlAPIKey) },
		cfg.FirecrawlBaseURL, cfg.FirecrawlHTTPTimeout,
	)
	urlDeps := queues.URLDeps{
		Scraper:    firecrawlClient,
		Embedder:   embedder,
		Storage:    store,
		Assets:     r.pieceRepo,
		Chunks:     r.chunksRepo,
		Images:     r.assetImageRepo,
		Hub:        hub,
		Recorder:   usageWiring.recorder,
		EmbedModel: cfg.EmbedModel,
		Notifier:   notifier, // CON-242: asset-ingest-done producer
	}

	// CON-87 WS3: River background-job queue. Runs on the same
	// database/sql pool as bun (db.DB), so a submit enqueue can join the
	// schedule transaction (CON-78 §9). The worker pool starts below and
	// is drained on shutdown via the Fiber hook.
	//
	// ProfileID resolves lazily so this wiring runs before the
	// bootstrapper has finished, and so a profile id added later via the
	// secrets API is picked up on the next dispatch.
	// profileIDResolver reads the current tenant's Zernio profile id from the
	// tenant-scoped settings on each call (shared by the jobs, the analytics
	// insight endpoints, and verify-external — CON-153).
	profileIDResolver := func(ctx context.Context) (string, error) {
		id, _, err := zernioRT.Settings.Get(ctx, pubzernio.SettingProfileID)
		return id, err
	}
	zernioDeps := queues.ZernioDeps{
		PostRepo:           r.postRepo,
		PostLogRepo:        r.postLogRepo,
		PostAttachmentRepo: r.postAttachmentRepo,
		SocialAccountRepo:  r.socialAccountRepo,
		Storage:            store,
		SettingRepo:        r.settingRepo,
		AnalyticsRepo:      r.postAnalyticsRepo,
		FollowerRepo:       r.followerStatsRepo,
		PlatformRepo:       r.platformRepo,
		Client:             zernioRT.Integration.Client,
		Recorder:           usageWiring.recorder,
		ActivityRecorder:   activityWiring.recorder,
		ProfileID:          profileIDResolver,
	}

	// The six workers self-register from their init()s; RegisterAll wires them
	// to the River registry with one dependency bundle. The analytics periodic
	// job is always scheduled; it is profile-driven, so when Zernio is disabled
	// (no key / no profiles) each tick is a harmless no-op, and it starts
	// producing once a key is set via the secrets API — no reboot.
	// CON-154: transactional + marketing email. Seeds default templates and
	// builds the Resend sender (per-call key resolution, so a key set/rotated via
	// the secrets API takes effect with no reboot; an unset key = skipped_disabled).
	emailRT, err := initEmail(ctx, cfg, secretStore, r.emailTemplateRepo, r.emailSuppressionRepo, r.emailLogRepo, r.emailEventRepo, r.emailBodyRepo, r.userRepo, activityWiring.recorder)
	if err != nil {
		return nil, err
	}

	cleanupEvery := time.Hour
	reconcileEvery := 5 * time.Minute
	workers := river.NewWorkers()
	queues.RegisterAll(workers, queues.Deps{
		Zernio:              zernioDeps,
		PostLogRetention:    time.Duration(cfg.PostLogRetentionDays) * 24 * time.Hour,
		ReconcileGrace:      cfg.ReconcileGrace,
		AnalyticsSettings:   zernioRT.Settings,
		AnalyticsHub:        hub,
		AnalyticsWindowDays: cfg.ZernioAnalyticsWindowDays,
		// CON-236: age-based refresh-decay schedule (new posts checked often,
		// settled posts rarely) so the trend history and current-state writes
		// stay proportional to how fast a post's numbers still move.
		AnalyticsDecay: queues.AnalyticsDecay{
			FreshWindow: cfg.ZernioAnalyticsFreshWindow,
			WarmWindow:  cfg.ZernioAnalyticsWarmWindow,
			FreshEvery:  cfg.ZernioAnalyticsFreshEvery,
			WarmEvery:   cfg.ZernioAnalyticsWarmEvery,
			ColdEvery:   cfg.ZernioAnalyticsColdEvery,
		},
		// CON-102: eager per-tenant profile provisioning at signup.
		ProfileBootstrapper: zernioRT.Bootstrapper,
		Integration:         zernioRT.Integration,
		// CON-203: fence the profile teardown against a concurrent CON-190 restore.
		TenantFence: r.tenantFence,
		// CON-103: PDF ingestion worker deps.
		PDF: pdfDeps,
		// CON-280: document ingestion worker deps.
		Document: documentDeps,
		// CON-282: audio ingestion worker deps (runs on the dedicated audio queue).
		Audio: audioDeps,
		// CON-281: image ingestion worker deps (runs on the dedicated image queue).
		Image: imageDeps,
		// CON-222: URL scrape ingestion worker deps.
		URL: urlDeps,
		// CON-154: email send + cleanup worker deps.
		Email: emailRT.Deps,
		// CON-217: expired headless-connect-session sweep.
		ConnectSessionRepo: r.zernioConnectSessionRepo,
		// CON-190: gate per-tenant jobs (publish/bootstrap/email) on tenant status.
		Tenants: r.tenantRepo,
		// CON-219: connection-expiry sweep deps (owner recipients + reconnect link
		// base + lead window). Its client/account repo ride Zernio, its email log
		// repo rides Email.Logs.
		Users:          r.userRepo,
		AppBaseURL:     cfg.AppBaseURL,
		ExpiryLeadDays: cfg.ConnectionExpiryLeadDays,
		// CON-242: notification center — producer service + cleanup sweep deps.
		Notifier:              notifier,
		NotificationRepo:      r.notificationRepo,
		NotificationRetention: time.Duration(cfg.NotificationsRetentionDays) * 24 * time.Hour,
		// CON-229: outbound new-tenant webhook to Harbor (signed). Empty URL ⇒ no-op.
		HarborNotify: queues.HarborNotifyDeps{URL: cfg.HarborWebhookURL, Secret: cfg.HarborWebhookSecret},
	})

	if err := jobs.MigrateRiver(ctx, db.DB); err != nil {
		return nil, err
	}
	riverClient, err := river.NewClient[*sql.Tx](riverdatabasesql.New(db.DB), &river.Config{
		// Route River's internal logging through the shared structured logger
		// (CON-107) so job-queue lines join the same stream and format.
		Logger: slog.Default(),
		// CON-303: wrap every job in a root tracing span (so its DB/gRPC work is a
		// coherent trace) and report exhausted-retry failures to Sentry.
		Middleware: jobs.Middleware(),
		// CON-282/CON-281: dedicated `audio` and `image` queues isolate long
		// transcription/vision runs from short jobs on the default queue (their
		// pools are sized separately). Jobs are routed by each task's InsertOpts.
		Queues:  queues.QueueConfigs(cfg.JobWorkers, cfg.AudioJobWorkers, cfg.ImageJobWorkers),
		Workers: workers,
		PeriodicJobs: queues.PeriodicConfig{
			CleanupEvery:      cleanupEvery,
			EmailCleanupEvery: cleanupEvery,
			ReconcileEvery:    reconcileEvery,
			AnalyticsEvery:    cfg.ZernioAnalyticsRefreshInterval,
			IncludeAnalytics:  true,
			// CON-153: daily follower-stats snapshot sweep. Like analytics, it is
			// profile-driven, so it no-ops when Zernio is unconfigured.
			FollowerEvery:    cfg.ZernioFollowerRefreshInterval,
			IncludeFollowers: true,
			// CON-217: reclaim expired headless-connect sessions. Correctness
			// doesn't depend on it (readers treat past-expiry as gone); this just
			// keeps the table tidy.
			ConnectSessionCleanupEvery: 15 * time.Minute,
			// CON-219: connection-health / expiry-notification sweep. Like analytics
			// and followers it is profile-driven, so it no-ops when Zernio is
			// unconfigured.
			HealthCheckEvery:        cfg.ZernioHealthCheckInterval,
			IncludeConnectionExpiry: true,
			// CON-242: notification retention/expiry sweep.
			NotificationCleanupEvery: cfg.NotificationsCleanupEvery,
			// CON-285: manual-publish-due sweep.
			ManualPublishDueEvery: cfg.ManualPublishDueSweepEvery,
		}.PeriodicJobs(),
	})
	if err != nil {
		return nil, err
	}
	enqueuer := &queues.Enqueuer{Client: riverClient}

	// CON-97: public self-service signup (POST /api/tenants) + tenant CRU. The
	// transactional signup use case (CON-102 profile bootstrap + CON-154 welcome/
	// drip mail enqueued in its tx) lives in the signup service; both enqueues go
	// through the River enqueuer, so this waits until the River client exists.
	signupSvc := signup.New(db, r.accountRepo, r.tenantRepo, enqueuer)
	signupSvc.SetEmailEnqueuer(enqueuer)
	// CON-229: notify operators on a new registration. Only wired when the Harbor
	// webhook URL is configured, so an unconfigured deploy queues no webhook jobs.
	if cfg.HarborWebhookURL != "" {
		signupSvc.SetHarborEnqueuer(enqueuer)
	}
	tenantsHandler := handlers.NewTenantsHandler(signupSvc, r.tenantRepo, cfg.SessionCookieName, !cfg.Debug, auth)
	tenantsHandler.SetActivityRecorder(activityWiring.recorder)
	tenantsHandler.Register(app)

	// CON-147: authenticated workspace surface (list / create / switch). Create
	// provisions a per-workspace Zernio profile through the same River enqueuer as
	// signup, so it registers here alongside the tenants handler.
	workspacesHandler := handlers.NewWorkspacesHandler(db, r.workspaceRepo, r.userRepo, r.accountRepo, r.tenantRepo, r.sessionRepo, enqueuer, auth)
	workspacesHandler.SetActivityRecorder(activityWiring.recorder)
	workspacesHandler.Register(app)

	// CON-161: public password-reset request + confirm (both unauthenticated —
	// the emailed token is the capability). The request endpoint enqueues the
	// reset email in its token-minting tx, so registration waits until the River
	// enqueuer exists.
	passwordResetHandler := handlers.NewPasswordResetHandler(db, r.userRepo, r.accountRepo, cfg.AppBaseURL)
	passwordResetHandler.SetActivityRecorder(activityWiring.recorder)
	passwordResetHandler.SetEmailEnqueuer(enqueuer)
	passwordResetHandler.Register(app)

	// CON-26: workspace invitations (owner-gated create/list/revoke + public
	// preview/accept). Creating an invite enqueues its email in the minting tx —
	// like password reset — so registration waits until the River enqueuer exists.
	invitationsHandler := handlers.NewInvitationsHandler(db, r.userRepo, r.accountRepo, r.tenantRepo, r.invitationRepo, r.sessionRepo, cfg.AppBaseURL, cfg.SessionCookieName, !cfg.Debug, auth)
	invitationsHandler.SetActivityRecorder(activityWiring.recorder)
	invitationsHandler.SetEmailEnqueuer(enqueuer)
	invitationsHandler.SetLimiter(entitlementLimiter) // CON-295: seat cap on the accept path
	invitationsHandler.Register(app)

	// CON-154: public unsubscribe (token-gated) + Resend delivery webhook
	// (signature-gated). Registered unconditionally; both degrade safely when
	// the relevant secret is unset.
	emailRT.Handler.Register(app)
	emailRT.Webhook.Register(app)

	// Expose expvar counters for ops health dashboards (CON-69 §13).
	// Gated by the same auth as the rest of the app so internal
	// counters aren't anonymous. (A River monitoring UI is a follow-up;
	// the old /admin/backlite mount is removed with backlite.)
	app.Get("/debug/vars", auth, adaptor.HTTPHandler(expvar.Handler()))

	if err := riverClient.Start(ctx); err != nil {
		return nil, err
	}
	app.Hooks().OnShutdown(func() error {
		sctx, cancel := context.WithTimeout(context.Background(), cfg.JobShutdownTimeout)
		defer cancel()
		_ = riverClient.Stop(sctx)
		return nil
	})
	// CON-282: close the audio-service gRPC connection AFTER River has drained
	// (hook registered here, post-Stop, so it runs after it in Fiber's ordered
	// shutdown). Closing earlier would abort a still-running process_audio job's
	// in-flight TranscribeSegment RPC. Runs before the recorder drain below.
	if audioClient != nil {
		app.Hooks().OnShutdown(func() error { return audioClient.Close() })
	}
	// CON-281: same ordering for the image-service connection — close it after
	// River drains so a running process_image job's Extract RPC isn't aborted.
	if imageClient != nil {
		app.Hooks().OnShutdown(func() error { return imageClient.Close() })
	}

	// Drain the usage recorder LAST. Fiber runs OnShutdown hooks in registration
	// order, so this must come after the river/zernio producer hooks above:
	// otherwise recorder.loop() can drain and exit while a worker is still
	// calling Record(), silently dropping queued usage events. Nil-safe when
	// analytics is disabled.
	if usageWiring.recorder != nil {
		app.Hooks().OnShutdown(func() error {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return usageWiring.recorder.Close(sctx)
		})
	}
	// Drain the activity recorder LAST, for the same reason as the usage
	// recorder above (it too is fed by request handlers and background workers).
	// Nil-safe when analytics is disabled.
	if activityWiring.recorder != nil {
		app.Hooks().OnShutdown(func() error {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return activityWiring.recorder.Close(sctx)
		})
	}

	// CON-103: PDF ingestion goes through the process_pdf River job — the handler
	// stores original.pdf and enqueues in its transaction. The enqueuer is wired
	// only when ingestion is live; otherwise PDF uploads create a pending asset
	// and skip processing. Markdown/JSON embedding still uses OnMarkdownSave.
	var pdfJobs handlers.PDFIngestEnqueuer
	if pdfIngestEnabled {
		pdfJobs = enqueuer
	}
	// CON-222: URL ingestion enqueues through the same River client; the /url
	// endpoint gates on firecrawlClient.HasKey (409 when no key configured).
	var urlJobs handlers.URLIngestEnqueuer = enqueuer
	// CON-280: document ingestion enqueues through the same River client, gated on
	// document-service being configured. Left nil otherwise so a doc upload fails
	// fast ("document ingestion is not configured") instead of stranding a pending
	// asset.
	var docJobs handlers.DocumentIngestEnqueuer
	if documentIngestEnabled {
		docJobs = enqueuer
	}
	// CON-281: image ingestion enqueues through the same River client, gated on
	// image-service being configured. Left nil otherwise so an image upload fails
	// fast ("image processing is not configured") — imageprobe was deleted, so
	// there is no local fallback (D6).
	var imgJobs handlers.ImageIngestEnqueuer
	if imageIngestEnabled {
		imgJobs = enqueuer
	}
	// imagePreparer is a TRUE nil interface when the service is unwired (assigning a
	// typed-nil *Client would make the interface non-nil and defeat the handlers'
	// `== nil` guards), so image attachments/alt-text fail fast with a clear reason.
	var imagePreparer handlers.ImagePreparer
	if imageClient != nil {
		imagePreparer = imageClient
	}
	assetsHandler := handlers.NewAssetsHandler(r.pieceRepo, r.assetFileRepo, r.assetImageRepo, store, db, pdfJobs, urlJobs, firecrawlClient, docJobs, imgJobs, auth, embedCallbacks.OnMarkdownSave)
	assetsHandler.SetLimiter(entitlementLimiter)
	assetsHandler.SetChunkLister(r.chunksRepo)
	if imageIngestEnabled {
		assetsHandler.SetImageReembedder(enqueuer)
	}
	assetsHandler.Register(app)

	// CON-282: audio asset lifecycle (presigned upload + extraction status/
	// transcript/retry). audioJobs is wired only when audio ingestion is live;
	// otherwise the write endpoints return 409 ("audio ingestion not configured").
	var audioJobs handlers.AudioIngestEnqueuer
	if audioIngestEnabled {
		audioJobs = enqueuer
	}
	audioAssetsHandler := handlers.NewAudioAssetsHandler(r.pieceRepo, r.assetFileRepo, r.audioExtractionRepo, r.audioSegmentRepo, r.utteranceRepo, store, db, audioJobs, auth)
	audioAssetsHandler.SetLimiter(entitlementLimiter)
	audioAssetsHandler.Register(app)

	// CON-281: content-bank image extraction surface (status/blocks + extract/
	// reextract/regenerate-alt-text). imgJobs is wired only when image ingestion is
	// live; imageClient (nil-safe) backs alt-text regeneration.
	handlers.NewAssetsImageHandler(r.pieceRepo, r.assetFileRepo, r.imageExtractionRepo, r.imageBlockRepo, store, db, imgJobs, imagePreparer, usageWiring.recorder, cfg.VisionClassifyModel, cfg.AltTextGenMaxChars, auth).Register(app)

	// Anthropic-backed flows live in a hot-reloadable runtime. boot
	// is allowed to start without an Anthropic key (callbacks return
	// 503 via the handler's IsAnthropicAvailable check); rotating
	// anthropic_api_key via the gRPC secrets service triggers a rebuild
	// on the next call.
	// CON-59: one clone service, shared by the REST endpoint and the
	// assistant's clonePost tool. Deep-copies attachments in object
	// storage so clone and source have independent blob lifecycles.
	cloneSvc := clone.New(db, r.postRepo, r.postVersionRepo, r.postAttachmentRepo, r.platformRepo, r.postLogRepo, store, hub)
	// CON-68: one restore service, shared by the REST endpoint and the
	// assistant's restoreVersion tool. Non-destructive append-only roll-back.
	restoreSvc := restore.New(db, r.postRepo, r.postVersionRepo, r.postLogRepo, hub)
	// CON-78: one schedule service, shared by POST /:id/schedule, the
	// assistant's schedulePost tool, and the PUT scheduling branch. Owns
	// allowlist routing + transactional persist + Zernio submit enqueue.
	scheduleSvc := schedule.New(db, r.postRepo, r.platformRepo, r.postAttachmentRepo, r.autoPublishAllowlistRepo, r.postLogRepo, enqueuer, hub)
	// CON-150: reject ambiguous / invalid same-platform account selections at
	// schedule time (auto-publish posts only). Reuses the Zernio profile-id
	// resolver so it degrades to the submit-worker backstop before bootstrap.
	scheduleSvc.SetAccountGate(r.socialAccountRepo, func(ctx context.Context) (string, error) {
		id, _, err := zernioRT.Settings.Get(ctx, pubzernio.SettingProfileID)
		return id, err
	})
	// CON-251: snapshot the content submitted to Zernio at schedule time so a
	// published post keeps a durable record of "what actually went out".
	scheduleSvc.SetVersionSnapshot(r.postVersionRepo)
	// CON-188: one note service, shared by the REST CRUD and the assistant's
	// createNote tool, so validation + origin stamping never drift.
	noteSvc := notes.New(r.postNoteRepo)

	gkRuntime, err := newGenkitRuntime(ctx, genkitDeps{
		cfg:      cfg,
		hub:      hub,
		embedder: embedder,
		contentPlanRepos: content_plan.ContentPlanRepos{
			Campaigns: r.campaignRepo,
			Assets:    r.pieceRepo,
			Chunks:    r.chunksRepo,
			Platforms: r.platformRepo,
			Posts:     r.postRepo,
			Notes:     r.postNoteRepo,
			Brands:    r.brandRepo, // CON-245
		},
		postAssistRepos: post_assistant.PostAssistantRepos{
			Posts:       r.postRepo,
			Assets:      r.pieceRepo,
			Chunks:      r.chunksRepo,
			Campaigns:   r.campaignRepo,
			Versions:    r.postVersionRepo,
			Messages:    r.postMessageRepo,
			Platforms:   r.platformRepo,
			Settings:    r.settingRepo,
			Allowlist:   r.autoPublishAllowlistRepo,
			Attachments: r.postAttachmentRepo,
			Notes:       r.postNoteRepo,
			Brands:      r.brandRepo, // CON-245
		},
		postQualityRepos: post_quality.PostQualityRepos{
			Posts:       r.postRepo,
			Campaigns:   r.campaignRepo,
			Assets:      r.pieceRepo,
			Chunks:      r.chunksRepo,
			Platforms:   r.platformRepo,
			Evaluations: r.postEvaluationRepo,
			PostLogs:    r.postLogRepo,
			Versions:    r.postVersionRepo,
		},
		enrichBriefRepos: enrich_brief.EnrichBriefRepos{
			Campaigns:     r.campaignRepo,
			CampaignTypes: r.campaignTypeRepo,
		},
		campaignAssistRepos: campaign_assistant.CampaignAssistantRepos{
			Messages:  r.campaignMessageRepo,
			Campaigns: r.campaignRepo,
			Posts:     r.postRepo,
			Assets:    r.pieceRepo,
			Chunks:    r.chunksRepo,
			Brands:    r.brandRepo, // CON-245
		},
		draftPostRepos: draft_post.DraftPostRepos{
			Campaigns: r.campaignRepo,
			Platforms: r.platformRepo,
			Posts:     r.postRepo,
			Notes:     r.postNoteRepo,
			Brands:    r.brandRepo, // CON-245
		},
		campaignOverviewSvc: campaignOverviewSvc,
		cloneSvc:            cloneSvc,
		restoreSvc:          restoreSvc,
		scheduleSvc:         scheduleSvc,
		noteSvc:             noteSvc,
		recorder:            usageWiring.recorder,
		checker:             usageWiring.checker,
		notifier:            notifier, // CON-242: campaign content-plan-ready producer
	}, secretStore)
	if err != nil {
		return nil, err
	}
	slog.Info("genkit flows registered", logging.AttrComponent, "genkit")

	handlers.NewCampaignTypesHandler(r.campaignTypeRepo, auth).Register(app)
	// CON-113/CON-152: the campaign read projections (GET /:id/overview + GET
	// /summaries) are a focused handler (CON-291 split out of CampaignsHandler),
	// registered BEFORE it so the static /summaries route wins over /:id.
	handlers.NewCampaignReadHandler(campaignOverviewSvc, campaignSummariesSvc, auth).Register(app)
	// CON-285: Activity daily-report endpoints (GET /api/activity/report/:date +
	// /reports). The live feed itself rides the CON-242 notification stream.
	handlers.NewActivityHandler(activityReportSvc, auth).Register(app)
	campaignsHandler := handlers.NewCampaignsHandler(r.campaignRepo, r.campaignTypeRepo, auth, gkRuntime.GenerateDraft, gkRuntime.IsAnthropicAvailable, gkRuntime.EnrichBrief, r.campaignMessageRepo, gkRuntime.RunCampaignAssistant)
	campaignsHandler.SetLimiter(entitlementLimiter)
	// CON-114/CON-116: targeted generation + consistency reviews are a focused
	// handler (CON-291 split out of CampaignsHandler), sharing the Anthropic-key
	// readiness gate and the same flow callbacks the assistant uses.
	handlers.NewCampaignGenerationHandler(r.campaignRepo, gkRuntime.GeneratePosts, cfg.GeneratePostsMax, gkRuntime.CheckBrief, gkRuntime.CheckPosts, gkRuntime.IsAnthropicAvailable, activityWiring.recorder, auth).Register(app)
	campaignsHandler.SetActivityRecorder(activityWiring.recorder)
	// CON-245: validate campaign brand_voice_id/brand_audience_id against the tenant.
	campaignsHandler.SetBrandRepo(r.brandRepo)
	campaignsHandler.Register(app)
	// CON-166: a campaign's phase date plan (GET/PUT/DELETE /:id/phases).
	handlers.NewCampaignPhasesHandler(r.campaignRepo, activityWiring.recorder, auth).Register(app)
	// CON-228: Brand materials — tenant-scoped voices/audiences/guardrails/look/
	// templates behind /api/brand. The ui repo built its /brand screens against a
	// stub whose shapes this endpoint answers verbatim (CON-227).
	brandHandler := handlers.NewBrandHandler(r.brandRepo, store, auth)
	brandHandler.SetActivityRecorder(activityWiring.recorder)
	brandHandler.Register(app)
	handlers.NewPlatformsHandler(r.platformRepo, pubs, r.autoPublishAllowlistRepo, auth).Register(app)
	handlers.NewTagsHandler(r.tagRepo, auth).Register(app)
	postsHandler := handlers.NewPostsHandler(r.postRepo, r.postVersionRepo, r.platformRepo, r.postAttachmentRepo, auth)
	// CON-128: the AI assistant surface (POST /:id/assistant SSE + GET /:id/messages)
	// is a focused handler (CON-291 split out of PostsHandler).
	handlers.NewPostAssistantHandler(gkRuntime.RunPostAssistant, gkRuntime.IsAnthropicAvailable, r.postMessageRepo, activityWiring.recorder, auth).Register(app)
	// CON-245: validate a post's brand_voice_id/brand_audience_id against the tenant.
	postsHandler.SetBrandRepo(r.brandRepo)
	// CON-166: a post's campaign_type_phase_id must be a phase of its campaign's type.
	postsHandler.SetCampaignRepo(r.campaignRepo)
	// CON-69 §11: every transition (success/blocked) and validation
	// outcome lands in the Post Log.
	postsHandler.SetPostLogRepo(r.postLogRepo)
	// CON-69 §5: ReadyForPublish→Scheduled consults the auto-publish
	// allowlist and (for allowlisted platforms) enqueues a submit task
	// transactionally with the status change + log write.
	postsHandler.SetSchedulingDeps(r.autoPublishAllowlistRepo, enqueuer, db)
	// CON-59/CON-68: clone + restore actions (POST /:id/clone, /:id/restore) are a
	// focused actions handler (CON-291 split out of PostsHandler), sharing the same
	// services the assistant tools use.
	handlers.NewPostActionsHandler(r.postRepo, cloneSvc, restoreSvc, activityWiring.recorder, auth).Register(app)
	// CON-78: same schedule service the assistant uses, behind the REST
	// endpoint POST /api/posts/:id/schedule and the PUT scheduling branch.
	postsHandler.SetScheduleService(scheduleSvc)
	// CON-85/CON-92/CON-93: post quality assessment (POST /:id/assess + GET
	// /:id/assessment) and the per-post analytics snapshot read (GET
	// /:id/analytics) are a focused insights handler (CON-291 split out of
	// PostsHandler), registered on the same /api/posts group.
	handlers.NewPostInsightsHandler(r.postRepo, gkRuntime.AssessPostQuality, r.postEvaluationRepo, r.postAnalyticsRepo, gkRuntime.IsAnthropicAvailable, activityWiring.recorder, auth).Register(app)
	// CON-153: POST /api/posts/:id/verify-external — confirm a manually
	// published post via Zernio's sync-external, back-fill publisher_post_id +
	// a first analytics snapshot, and emit post.analytics.updated.
	// CON-153 external-post verification is its own focused handler (CON-291 split
	// out of PostsHandler), registered on the same /api/posts group.
	handlers.NewPostVerificationHandler(r.postRepo, zernioRT.Integration.Client, r.socialAccountRepo, profileIDResolver, r.postAnalyticsRepo, r.postVersionRepo, hub, auth).Register(app)
	postsHandler.SetActivityRecorder(activityWiring.recorder)
	// Cascade post-attachment S3 cleanup on post delete (CON-73 §2.7).
	// FK CASCADE handles the DB rows; this hook handles the bucket.
	postsHandler.SetOnBeforeDelete(func(ctx context.Context, postID string) error {
		if store == nil {
			return nil
		}
		keys, err := r.postAttachmentRepo.ListS3KeysByPostID(ctx, postID)
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
		return nil
	})
	postsHandler.Register(app)
	handlers.NewPostLogsHandler(r.postLogRepo, r.postRepo, auth).Register(app)
	// CON-93 FR5 + CON-153: analytics surface under its own /api/analytics group
	// (avoids the /api/posts/:id route collision). The post overview + follower
	// series are served from the DB; the insight aggregates live-proxy to Zernio.
	handlers.NewAnalyticsHandler(r.postAnalyticsRepo, r.followerStatsRepo, r.postRepo, zernioRT.Integration.Client, profileIDResolver, auth).Register(app)

	handlers.NewImagesHandler(store, auth).Register(app)
	// CON-103: PDF attachment page-count + thumbnail now come from pdf-service.
	// nil pdfClient (PDF_SERVICE_ADDR unset) degrades gracefully (no page count
	// / thumbnail), so it's wired only when present.
	var attachmentRenderer handlers.PDFRenderer
	if pdfClient != nil {
		attachmentRenderer = pdfClient
	}
	// CON-148: video attachment probing (duration/codec/resolution + poster)
	// comes from video-service. nil videoClient (VIDEO_SERVICE_ADDR unset)
	// degrades gracefully (uploads accepted unprobed), so it's wired only when
	// present.
	var attachmentProber handlers.VideoProber
	if videoClient != nil {
		attachmentProber = videoClient
	}
	// CON-281: image attachments route through image-service (EXIF-strip + metadata
	// + async alt text). imagePreparer is nil when the service is unwired, so image
	// attachment uploads fail fast (503) — imageprobe was deleted (D6). Alt-text
	// generation is metered on the gemini vendor and targets AltTextGenMaxChars.
	postAttachmentsHandler := handlers.NewPostAttachmentsHandler(r.postAttachmentRepo, r.postRepo, store, attachmentRenderer, attachmentProber, imagePreparer, usageWiring.recorder, cfg.VisionClassifyModel, cfg.AltTextGenMaxChars, auth)
	postAttachmentsHandler.SetLimiter(entitlementLimiter)
	postAttachmentsHandler.Register(app)

	// CON-188: per-post notes CRUD, nested under a post.
	postNotesHandler := handlers.NewPostNotesHandler(noteSvc, r.postRepo, auth)
	postNotesHandler.SetActivityRecorder(activityWiring.recorder)
	postNotesHandler.Register(app)

	// CON-315: the workspace Ideas backlog (capture + triage verdicts).
	ideasHandler := handlers.NewIdeasHandler(ideas.New(r.ideaRepo, r.userRepo), auth)
	ideasHandler.SetActivityRecorder(activityWiring.recorder)
	ideasHandler.Register(app)

	// The React SPA is deployed separately (CON-98) — the API serves only
	// /api/* (plus SSE). Non-API routes fall through to a 404.
	return app, nil
}

func defaultErrorHandler(c *fiber.Ctx, err error) error {
	// CON-295: render entitlement denials as structured bodies.
	if qe, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
		return c.Status(fiber.StatusPaymentRequired).JSON(fiber.Map{
			"error": "entitlement_exceeded", "feature": qe.Key, "limit": qe.Limit, "current": qe.Current,
		})
	}
	if fe, ok := errors.AsType[*entitlements.FeatureNotAvailableError](err); ok {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "feature_not_available", "feature": fe.Key})
	}
	code := fiber.StatusInternalServerError
	if e, ok := errors.AsType[*fiber.Error](err); ok {
		code = e.Code
	}
	// Server errors were previously swallowed — only a JSON body reached the
	// client, nothing was logged (CON-107). Log 5xx at ERROR with request
	// context; 4xx is a client problem, not a server fault, so it is not logged
	// as an error here (it still appears in the access log).
	if code >= 500 {
		slog.ErrorContext(c.Context(), "request failed",
			logging.AttrComponent, "http",
			"method", c.Method(),
			"path", c.Path(),
			"status", code,
			logging.AttrError, err)
		// CON-303: report the server fault to Sentry, linked to the request trace.
		// A no-op when telemetry is disabled; skips panics already captured at
		// recovery time.
		reportServerError(c, err, code)
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}

// accessLog emits exactly one structured line per request, replacing Fiber's
// default text logger (CON-107). It runs after the handler so the request,
// tenant, and user ids set by downstream middleware are attached by the slog
// ContextHandler via c.Context(). Like Fiber's own logger middleware it invokes
// the app ErrorHandler when the chain returns an error, so the logged status
// reflects the final response (e.g. 5xx) rather than the pre-handler default.
func accessLog() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		chainErr := c.Next()
		if chainErr != nil {
			if herr := c.App().ErrorHandler(c, chainErr); herr != nil {
				_ = c.SendStatus(fiber.StatusInternalServerError)
			}
		}

		status := c.Response().StatusCode()
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}
		// Streaming responses (SetBodyStreamWriter — e.g. the SSE event stream)
		// have no materialised body. Calling c.Response().Body() on one drains
		// the stream to EOF to buffer it; for a long-lived/infinite stream that
		// never returns, blocking the response from ever being flushed (the SSE
		// client then hangs forever in "connecting"). Skip the byte count there.
		respBytes := 0
		if !c.Response().IsBodyStream() {
			respBytes = len(c.Response().Body())
		}
		slog.Default().LogAttrs(c.Context(), level, "request",
			slog.String(logging.AttrComponent, "http"),
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			slog.Int("bytes", respBytes),
			slog.String("ip", c.IP()),
		)
		return nil
	}
}
