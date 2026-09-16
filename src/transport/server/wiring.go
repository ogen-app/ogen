package server

import (
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// repos is the API server's full data-access surface, built once by
// wireRepositories. Collecting the ~30 repositories here makes the persistence
// layer explicit in one place and lets it be constructed (and tested)
// independently of the rest of New. Analytics-backed repos (postAnalyticsRepo,
// followerStatsRepo) are nil when the analytics pool is disabled; every reader
// already treats those as fail-open.
type repos struct {
	userRepo                 repository.UserRepository
	accountRepo              repository.AccountRepository
	workspaceRepo            repository.WorkspaceRepository
	tenantRepo               repository.TenantRepository
	sessionRepo              repository.SessionRepository
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
	campaignTypeRepo         repository.CampaignTypeRepository
	campaignRepo             repository.CampaignRepository
	postRepo                 repository.PostRepository
	brandRepo                repository.BrandRepository
	postVersionRepo          repository.PostVersionRepository
	postMessageRepo          repository.PostAssistantMessageRepository
	campaignMessageRepo      repository.CampaignAssistantMessageRepository
	postAttachmentRepo       repository.PostAttachmentRepository
	postNoteRepo             repository.PostNoteRepository
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
	notificationRepo         repository.NotificationRepository
	invitationRepo           repository.InvitationRepository
	tierVersionRepo          repository.TenantTierVersionRepository
	tierAssignmentRepo       repository.TenantTierAssignmentRepository
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
		campaignTypeRepo:         campaignTypeRepo,
		campaignRepo:             repository.NewCampaignRepository(db, tagRepo, platformRepo, campaignTypeRepo),
		postRepo:                 repository.NewPostRepository(db),
		brandRepo:                repository.NewBrandRepository(db), // CON-228 store; CON-245 ref validation + flow resolution
		postVersionRepo:          repository.NewPostVersionRepository(db),
		postMessageRepo:          repository.NewPostAssistantMessageRepository(db),
		campaignMessageRepo:      repository.NewCampaignAssistantMessageRepository(db),
		postAttachmentRepo:       repository.NewPostAttachmentRepository(db),
		postNoteRepo:             repository.NewPostNoteRepository(db),
		postLogRepo:              repository.NewPostLogRepository(db),
		postEvaluationRepo:       repository.NewPostEvaluationRepository(db),
		socialAccountRepo:        repository.NewSocialAccountRepository(db),
		zernioConnectSessionRepo: repository.NewZernioConnectSessionRepository(db),
		autoPublishAllowlistRepo: repository.NewAutoPublishAllowlistRepository(db),
		emailTemplateRepo:        repository.NewEmailTemplateRepository(db),
		emailSuppressionRepo:     repository.NewEmailSuppressionRepository(db),
		emailLogRepo:             repository.NewEmailLogRepository(db),
		emailEventRepo:           repository.NewEmailEventRepository(db),
		notificationRepo:         repository.NewNotificationRepository(db),
		invitationRepo:           repository.NewInvitationRepository(db),
		tierVersionRepo:          repository.NewTenantTierVersionRepository(db),
		tierAssignmentRepo:       repository.NewTenantTierAssignmentRepository(db),
	}

	// CON-125/CON-153: analytics snapshots + follower stats live on the isolated
	// analytics pool. Left nil when it is disabled — reads fail-open to 503 and
	// the refresh jobs no-op.
	if analyticsDB != nil {
		r.postAnalyticsRepo = repository.NewPostAnalyticsRepository(analyticsDB)
		r.followerStatsRepo = repository.NewFollowerStatsRepository(analyticsDB)
	}
	return r
}

// newFiberApp builds the API's fiber.App with its base middleware stack:
// panic-recovery, per-request correlation id (CON-107), access logging, and —
// when a cross-origin UI is configured — credentialed CORS (CON-98).
func newFiberApp(cfg *config.Config) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: defaultErrorHandler,
		// WriteTimeout 0 disables the per-response write deadline so that SSE
		// streams (e.g. /generate-draft) are not forcibly closed mid-flight.
		WriteTimeout: 0,
		// Allow batched markdown uploads (up to 10 MB per file).
		BodyLimit: 100 << 20,
	})

	app.Use(recover.New())
	// Per-request correlation id (CON-107): honours an inbound X-Request-ID,
	// otherwise generates one, echoes it on the response, and stores it under
	// logging.RequestIDKey so the slog ContextHandler attaches it to every line
	// made with c.Context().
	app.Use(requestid.New(requestid.Config{ContextKey: logging.RequestIDKey}))
	app.Use(accessLog())

	// CORS for the decoupled UI (CON-98). When the SPA is served from a
	// different origin, the configured UI origin(s) must be allowed to call
	// the API with credentials so the c3_session cookie is accepted. Empty
	// CORSAllowedOrigins (same-origin dev, or a UI that reverse-proxies /api)
	// leaves CORS off entirely.
	if cfg.CORSAllowedOrigins != "" {
		app.Use(cors.New(cors.Config{
			AllowOrigins:     cfg.CORSAllowedOrigins,
			AllowCredentials: true,
			AllowMethods:     "GET,POST,PUT,PATCH,DELETE,OPTIONS",
			AllowHeaders:     "Content-Type",
		}))
	}
	return app
}
