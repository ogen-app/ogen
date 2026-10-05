package server

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// repos is the API server's full data-access surface, built once by
// wireRepositories. Collecting the ~30 repositories here makes the persistence
// layer explicit in one place and lets it be constructed (and tested)
// independently of the rest of the wiring. Analytics-backed repos (postAnalyticsRepo,
// followerStatsRepo) are nil when the analytics pool is disabled; every reader
// already treats those as fail-open.
type repos struct {
	userRepo                 repository.UserRepository
	accountRepo              repository.AccountRepository
	workspaceRepo            repository.WorkspaceRepository
	tenantRepo               repository.TenantRepository
	sessionRepo              repository.SessionRepository
	knownDeviceRepo          repository.KnownDeviceRepository
	loginAlertTokenRepo      repository.LoginAlertTokenRepository
	settingRepo              repository.SettingRepository
	tagRepo                  repository.TagRepository
	chunksRepo               repository.AssetChunksRepository
	assetFileRepo            repository.AssetFileRepository
	assetImageRepo           repository.AssetImageRepository
	pieceRepo                repository.AssetRepository
	audioExtractionRepo      repository.AudioExtractionRepository
	audioSegmentRepo         repository.AudioSegmentRepository
	utteranceRepo            repository.UtteranceRepository
	imageExtractionRepo      repository.ImageExtractionRepository
	imageBlockRepo           repository.ImageBlockRepository
	platformRepo             repository.PlatformRepository
	platformGlobalLimitsRepo repository.PlatformGlobalLimitsRepository
	flowModelConfigRepo      repository.FlowModelConfigRepository
	campaignTypeRepo         repository.CampaignTypeRepository
	campaignRepo             repository.CampaignRepository
	postRepo                 repository.PostRepository
	brandRepo                repository.BrandRepository
	postVersionRepo          repository.PostVersionRepository
	postMessageRepo          repository.PostAssistantMessageRepository
	campaignMessageRepo      repository.CampaignAssistantMessageRepository
	postAttachmentRepo       repository.PostAttachmentRepository
	postNoteRepo             repository.PostNoteRepository
	ideaRepo                 repository.IdeaRepository
	seriesRepo               repository.SeriesRepository
	postLogRepo              repository.PostLogRepository
	postEvaluationRepo       repository.PostEvaluationRepository
	postAnalyticsRepo        repository.PostAnalyticsRepository
	followerStatsRepo        repository.FollowerStatsRepository
	socialAccountRepo        repository.SocialAccountRepository
	zernioConnectSessionRepo repository.ZernioConnectSessionRepository
	autoPublishAllowlistRepo repository.AutoPublishAllowlistRepository
	emailTemplateRepo        repository.EmailTemplateRepository
	emailSuppressionRepo     repository.EmailSuppressionRepository
	emailLogRepo             repository.EmailLogRepository
	emailEventRepo           repository.EmailEventRepository
	emailBodyRepo            repository.EmailBodyRepository
	notificationRepo         repository.NotificationRepository
	announcementRepo         repository.AnnouncementRepository
	invitationRepo           repository.InvitationRepository
	tierVersionRepo          repository.TenantTierVersionRepository
	tierAssignmentRepo       repository.TenantTierAssignmentRepository
	// tenantFence serialises the CON-203 Zernio profile teardown against a
	// concurrent CON-190 restore via the tenant row lock.
	tenantFence *repository.TenantTeardownFence
}

// wireRepositories constructs every repository the API server needs. db is the
// primary pool; analyticsDB is the isolated analytics pool (may be nil, which
// leaves the analytics-backed repos nil / fail-open). Construction order honours
// the few inter-repo dependencies (pieceRepo and campaignRepo compose other
// repos).
func wireRepositories(db, analyticsDB *bun.DB) *repos {
	tagRepo := repository.NewTagRepository(db)
	assetFileRepo := repository.NewAssetFileRepository(db)
	platformRepo := repository.NewPlatformRepository(db)
	campaignTypeRepo := repository.NewCampaignTypeRepository(db)

	r := &repos{
		userRepo:                 repository.NewUserRepository(db),
		accountRepo:              repository.NewAccountRepository(db),
		workspaceRepo:            repository.NewWorkspaceRepository(db),
		tenantRepo:               repository.NewTenantRepository(db),
		tenantFence:              repository.NewTenantTeardownFence(db),
		sessionRepo:              repository.NewSessionRepository(db),
		settingRepo:              repository.NewSettingRepository(db),
		tagRepo:                  tagRepo,
		chunksRepo:               repository.NewAssetChunksRepository(db),
		assetFileRepo:            assetFileRepo,
		assetImageRepo:           repository.NewAssetImageRepository(db),
		pieceRepo:                repository.NewAssetRepository(db, tagRepo, assetFileRepo),
		audioExtractionRepo:      repository.NewAudioExtractionRepository(db),
		audioSegmentRepo:         repository.NewAudioSegmentRepository(db),
		utteranceRepo:            repository.NewUtteranceRepository(db),
		imageExtractionRepo:      repository.NewImageExtractionRepository(db),
		imageBlockRepo:           repository.NewImageBlockRepository(db),
		platformRepo:             platformRepo,
		platformGlobalLimitsRepo: repository.NewPlatformGlobalLimitsRepository(db),
		flowModelConfigRepo:      repository.NewFlowModelConfigRepository(db),
		campaignTypeRepo:         campaignTypeRepo,
		campaignRepo:             repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo),
		postRepo:                 repository.NewPostRepository(db),
		brandRepo:                repository.NewBrandRepository(db), // CON-228 store; CON-245 ref validation + flow resolution
		postVersionRepo:          repository.NewPostVersionRepository(db),
		postMessageRepo:          repository.NewPostAssistantMessageRepository(db),
		campaignMessageRepo:      repository.NewCampaignAssistantMessageRepository(db),
		postAttachmentRepo:       repository.NewPostAttachmentRepository(db),
		postNoteRepo:             repository.NewPostNoteRepository(db),
		ideaRepo:                 repository.NewIdeaRepository(db),
		seriesRepo:               repository.NewSeriesRepository(db),
		postLogRepo:              repository.NewPostLogRepository(db),
		postEvaluationRepo:       repository.NewPostEvaluationRepository(db),
		socialAccountRepo:        repository.NewSocialAccountRepository(db),
		zernioConnectSessionRepo: repository.NewZernioConnectSessionRepository(db),
		autoPublishAllowlistRepo: repository.NewAutoPublishAllowlistRepository(db),
		emailTemplateRepo:        repository.NewEmailTemplateRepository(db),
		emailSuppressionRepo:     repository.NewEmailSuppressionRepository(db),
		emailLogRepo:             repository.NewEmailLogRepository(db),
		emailEventRepo:           repository.NewEmailEventRepository(db),
		emailBodyRepo:            repository.NewEmailBodyRepository(db),
		notificationRepo:         repository.NewNotificationRepository(db),
		announcementRepo:         repository.NewAnnouncementRepository(db),
		invitationRepo:           repository.NewInvitationRepository(db),
		knownDeviceRepo:          repository.NewKnownDeviceRepository(db),
		loginAlertTokenRepo:      repository.NewLoginAlertTokenRepository(db),
		tierVersionRepo:          repository.NewTenantTierVersionRepository(db),
		tierAssignmentRepo:       repository.NewTenantTierAssignmentRepository(db),
	}

	// Analytics snapshots + follower stats live on the isolated
	// analytics pool. Left nil when it is disabled — reads fail-open to 503 and
	// the refresh jobs no-op.
	if analyticsDB != nil {
		r.postAnalyticsRepo = repository.NewPostAnalyticsRepository(analyticsDB)
		r.followerStatsRepo = repository.NewFollowerStatsRepository(analyticsDB)
	}
	return r
}

// newFiberApp builds the API's fiber.App with its base middleware stack:
// panic-recovery, per-request correlation id, access logging, and —
// when a cross-origin UI is configured — credentialed CORS.
func newFiberApp(cfg *config.Config) *fiber.App {
	fcfg := fiber.Config{
		ErrorHandler: defaultErrorHandler,
		// WriteTimeout 0 disables the per-response write deadline so that SSE
		// streams (e.g. /generate-draft) are not forcibly closed mid-flight.
		WriteTimeout: 0,
		// Allow batched markdown uploads (up to 10 MB per file).
		BodyLimit: 100 << 20,
	}
	applyProxyConfig(&fcfg, cfg)
	app := fiber.New(fcfg)

	// Tracing, panic recovery and error capture, outermost. See
	// useObservability.
	useObservability(app)

	// Per-request correlation id: honours an inbound X-Request-ID,
	// otherwise generates one, echoes it on the response, and stores it under
	// logging.RequestIDKey so the slog ContextHandler attaches it to every line
	// made with c.Context().
	app.Use(requestid.New(requestid.Config{ContextKey: logging.RequestIDKey}))
	app.Use(accessLog())

	// CORS for the decoupled UI. When the SPA is served from a
	// different origin, the configured UI origin(s) must be allowed to call
	// the API with credentials so the c3_session cookie is accepted. Empty
	// CORSAllowedOrigins (same-origin dev, or a UI that reverse-proxies /api)
	// leaves CORS off entirely.
	if cfg.CORSAllowedOrigins != "" {
		app.Use(cors.New(cors.Config{
			// Plugin routes answer their own preflights below; left to this
			// policy, a plugin's preflight would be refused before reaching them.
			Next:             isPluginRoute,
			AllowOrigins:     cfg.CORSAllowedOrigins,
			AllowCredentials: true,
			AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
			// sentry-trace/baggage (Sentry browser SDK) + traceparent/tracestate
			// (W3C) let the UI's trace continue into the API server span.
			AllowHeaders: "Content-Type,sentry-trace,baggage,traceparent,tracestate",
		}))
	}
	// Design-tool plugins call from an iframe whose origin is "null", which
	// only a wildcard matches. A wildcard is safe here because these routes
	// never read cookies: they authenticate by bearer plugin token alone.
	// Mounted whether or not the UI policy above is on.
	app.Use(handlers.PluginRoutePrefix, cors.New(cors.Config{
		AllowOrigins:  "*",
		AllowMethods:  "GET,POST,DELETE,OPTIONS",
		AllowHeaders:  "Authorization,Content-Type",
		ExposeHeaders: "Retry-After",
		MaxAge:        600,
	}))
	return app
}

// isPluginRoute reports whether a request targets the plugin API, which runs
// its own CORS policy.
func isPluginRoute(c *fiber.Ctx) bool {
	return strings.HasPrefix(c.Path(), handlers.PluginRoutePrefix+"/")
}
