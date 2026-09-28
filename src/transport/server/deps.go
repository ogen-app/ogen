package server

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/content_plan"
	"github.com/ogen-app/ogen/src/genkit/flows/draft_post"
	"github.com/ogen-app/ogen/src/genkit/flows/enrich_brief"
	"github.com/ogen-app/ogen/src/genkit/flows/post_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/post_quality"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	pubzernio "github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/usage"
	"github.com/ogen-app/ogen/src/transport/handlers"
	activityreport "github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/overview"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/summaries"
	"github.com/ogen-app/ogen/src/usecase/notes"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/clone"
	"github.com/ogen-app/ogen/src/usecase/post_actions/restore"
	"github.com/ogen-app/ogen/src/usecase/post_actions/schedule"
	"github.com/ogen-app/ogen/src/usecase/tenant_actions/signup"
)

// deps is the API server's wired object graph. server.New fills it in phases,
// each reading what earlier phases set, and the register*Routes functions
// build the HTTP handlers from it.
type deps struct {
	cfg         *config.Config
	db          *bun.DB
	analyticsDB *bun.DB
	secretStore secrets.Store
	cipher      *envelope.Cipher
	hub         eventhub.Hub
	r           *repos
	auth        fiber.Handler
	shutdown    *shutdownPlan

	entitlements entitlementDeps
	usage        usageDeps
	activity     activityDeps
	notifier     *notify.Service
	zernio       zernioRuntime
	store        storage.Storage
	clients      grpcClients
	ingest       ingestion
	email        emailRuntime
	enqueuer     *queues.Enqueuer
	svc          services
	genkit       *genkitRuntime
}

// services are the use cases shared between REST handlers and the assistant
// tools, so both paths run the same validation and persistence.
type services struct {
	campaignOverview  *overview.Service
	campaignSummaries *summaries.Service
	activityReport    *activityreport.Service
	clone             *clone.Service
	restore           *restore.Service
	schedule          *schedule.Service
	notes             *notes.Service
	signup            *signup.Service
}

func newDeps(db, analyticsDB *bun.DB, cfg *config.Config, secretStore secrets.Store, cipher *envelope.Cipher, hub eventhub.Hub, plan *shutdownPlan) *deps {
	r := wireRepositories(db, analyticsDB)
	return &deps{
		cfg:         cfg,
		db:          db,
		analyticsDB: analyticsDB,
		secretStore: secretStore,
		cipher:      cipher,
		hub:         hub,
		r:           r,
		auth:        handlers.RequireAuth(r.sessionRepo, r.userRepo, cfg.SessionCookieName),
		shutdown:    plan,
	}
}

// phases returns the wiring steps in dependency order.
func (d *deps) phases() []func(context.Context) error {
	return []func(context.Context) error{
		d.initEntitlements,
		d.initMetering,
		d.startZernio,
		d.initStorage,
		d.initGRPCClients,
		d.initIngestion,
		d.initRiver,
		d.initServices,
		d.initGenkit,
	}
}

func (d *deps) initEntitlements(context.Context) error {
	var err error
	d.entitlements, err = newEntitlements(d.cfg, d.r)
	return err
}

// initMetering wires usage metering, activity collection and the
// notification center. The recorders are nil when analytics is disabled.
func (d *deps) initMetering(context.Context) error {
	// A malformed USAGE_MODEL_PRICES or an unknown vendor fails boot before
	// anything is metered.
	if err := usage.ApplyModelPrices(d.cfg.UsageModelPrices); err != nil {
		return err
	}
	d.usage = initUsage(d.cfg, d.db, d.analyticsDB)
	d.activity = initActivity(d.cfg, d.analyticsDB)
	if d.usage.recorder != nil {
		d.shutdown.add(stageRecorders, closeWithin(5*time.Second, d.usage.recorder.Close))
	}
	if d.activity.recorder != nil {
		d.shutdown.add(stageRecorders, closeWithin(5*time.Second, d.activity.recorder.Close))
	}

	d.notifier = notify.New(d.r.notificationRepo, d.hub)
	// Near-limit crossings become inbox notifications for workspace owners.
	d.entitlements.limiter.WithNotifier(&limitNotifier{notify: d.notifier, users: d.r.userRepo}, d.cfg.EntitlementWarnThresholdPct)
	return nil
}

// startZernio starts the Zernio runtime. Ping, profile bootstrap and the sync
// worker run in the background, so boot never blocks on Zernio.
func (d *deps) startZernio(ctx context.Context) error {
	d.zernio = initZernio(ctx, d.cfg, d.secretStore, d.r.settingRepo, d.r.socialAccountRepo, d.hub, d.usage.recorder)
	d.shutdown.add(stageIntegrations, func() error {
		d.zernio.shutdown()
		return nil
	})
	return nil
}

// zernioProfileID reads the current tenant's Zernio profile id on each call,
// so it works before the bootstrapper finishes and picks up a profile added
// later.
func (d *deps) zernioProfileID(ctx context.Context) (string, error) {
	id, _, err := d.zernio.Settings.Get(ctx, pubzernio.SettingProfileID)
	return id, err
}

func (d *deps) initStorage(context.Context) error {
	var err error
	d.store, err = storage.New(d.cfg)
	return err
}

func (d *deps) initGRPCClients(context.Context) error {
	var err error
	d.clients, err = newGRPCClients(d)
	return err
}

// initIngestion builds the embedder and the per-kind ingestion worker deps.
// The embedder is a reloadable wrapper that reports unavailable until a
// gemini_api_key is set, so it never blocks boot.
func (d *deps) initIngestion(ctx context.Context) error {
	slog.Info("genkit initialising", logging.AttrComponent, "genkit", "genkit_env", os.Getenv("GENKIT_ENV"))
	callbacks, embedder, err := initEmbedding(ctx, d.cfg, d.r.chunksRepo, d.r.pieceRepo, d.r.assetFileRepo, d.store, d.secretStore, d.usage.recorder)
	if err != nil {
		return err
	}
	d.ingest = newIngestion(d, callbacks, embedder)
	return nil
}

// initRiver wires email (its workers ride River), builds the job client and
// starts the worker pool.
func (d *deps) initRiver(ctx context.Context) error {
	r := d.r
	var err error
	d.email, err = initEmail(ctx, d.cfg, d.secretStore, r.emailTemplateRepo, r.emailSuppressionRepo, r.emailLogRepo, r.emailEventRepo, r.emailBodyRepo, r.userRepo, d.activity.recorder)
	if err != nil {
		return err
	}
	client, err := newRiverClient(ctx, d)
	if err != nil {
		return err
	}
	d.enqueuer = &queues.Enqueuer{Client: client}
	if err := client.Start(ctx); err != nil {
		return err
	}
	d.shutdown.add(stageJobs, closeWithin(d.cfg.JobShutdownTimeout, func(ctx context.Context) error {
		_ = client.Stop(ctx)
		return nil
	}))
	return nil
}

func (d *deps) initServices(context.Context) error {
	r := d.r
	d.svc = services{
		campaignOverview:  overview.New(r.campaignRepo, r.postRepo, r.platformRepo),
		campaignSummaries: summaries.New(r.postRepo),
		activityReport:    activityreport.New(r.postRepo, r.postLogRepo, r.campaignRepo),
		// Deep-copies attachments in object storage so a clone and its source
		// have independent blob lifecycles.
		clone:   clone.New(d.db, r.postRepo, r.postVersionRepo, r.postAttachmentRepo, r.platformRepo, r.postLogRepo, d.store, d.hub),
		restore: restore.New(d.db, r.postRepo, r.postVersionRepo, r.postLogRepo, d.hub),
		notes:   notes.New(r.postNoteRepo),
	}

	d.svc.schedule = schedule.New(d.db, r.postRepo, r.platformRepo, r.postAttachmentRepo, r.autoPublishAllowlistRepo, r.postLogRepo, d.enqueuer, d.hub)
	// Rejects ambiguous same-platform account selections at schedule time;
	// before the profile exists it defers to the submit worker's check.
	d.svc.schedule.SetAccountGate(r.socialAccountRepo, d.zernioProfileID)
	// Keeps a durable record of the content submitted to Zernio.
	d.svc.schedule.SetVersionSnapshot(r.postVersionRepo)

	d.svc.signup = signup.New(d.db, r.accountRepo, r.tenantRepo, d.enqueuer)
	d.svc.signup.SetEmailEnqueuer(d.enqueuer)
	// Without a Harbor webhook URL no registration webhook jobs are queued.
	if d.cfg.HarborWebhookURL != "" {
		d.svc.signup.SetHarborEnqueuer(d.enqueuer)
	}
	return nil
}

// initGenkit builds the hot-reloadable Anthropic flow runtime. Boot proceeds
// without an Anthropic key: the flow handlers answer 503 until one is set via
// the secrets API, which rebuilds the runtime on the next call.
func (d *deps) initGenkit(ctx context.Context) error {
	var err error
	d.genkit, err = newGenkitRuntime(ctx, d.genkitDeps(), d.secretStore)
	if err != nil {
		return err
	}
	slog.Info("genkit flows registered", logging.AttrComponent, "genkit")
	return nil
}

func (d *deps) genkitDeps() genkitDeps {
	r := d.r
	return genkitDeps{
		cfg:      d.cfg,
		hub:      d.hub,
		embedder: d.ingest.embedder,
		contentPlanRepos: content_plan.ContentPlanRepos{
			Campaigns: r.campaignRepo,
			Assets:    r.pieceRepo,
			Chunks:    r.chunksRepo,
			Platforms: r.platformRepo,
			Posts:     r.postRepo,
			Notes:     r.postNoteRepo,
			Brands:    r.brandRepo,
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
			Brands:      r.brandRepo,
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
			Brands:    r.brandRepo,
		},
		draftPostRepos: draft_post.DraftPostRepos{
			Campaigns: r.campaignRepo,
			Platforms: r.platformRepo,
			Posts:     r.postRepo,
			Notes:     r.postNoteRepo,
			Brands:    r.brandRepo,
		},
		campaignOverviewSvc: d.svc.campaignOverview,
		cloneSvc:            d.svc.clone,
		restoreSvc:          d.svc.restore,
		scheduleSvc:         d.svc.schedule,
		noteSvc:             d.svc.notes,
		recorder:            d.usage.recorder,
		checker:             d.usage.checker,
		notifier:            d.notifier,
	}
}
