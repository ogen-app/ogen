package queues

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// This file holds the central River plumbing for the queue package: the
// dependency bundle, the self-registration registry, the periodic-job set,
// and the app-facing enqueue surface.
//
// Workers are NOT listed here. Each job is a processor (in its own file) that
// implements river.Worker directly (Work/Timeout next to its logic) and
// self-registers from an init() via register(). Adding a job therefore means
// adding one file — no central list to edit.

// Deps bundles every dependency the workers need. server.go builds one and
// passes it to RegisterAll; each worker's registrar picks what it needs. The
// cleanup/reconcile repos are the same instances already held by the Zernio
// bundle, so they aren't duplicated here.
type Deps struct {
	Zernio              ZernioDeps
	PostLogRetention    time.Duration
	ReconcileGrace      time.Duration
	AnalyticsSettings   zernio.SettingsStore
	AnalyticsHub        eventhub.Hub
	AnalyticsWindowDays int
	// CON-236: age-based refresh-decay schedule. Zero fields fall back to the
	// processor defaults (fresh<48h→hourly, warm<14d→daily, else weekly).
	AnalyticsDecay AnalyticsDecay
	// CON-102: eager per-tenant Zernio profile provisioning. The bootstrap job
	// reuses the connect-link handler's Bootstrapper (tenant-scoped profile
	// create/adopt + settings write) and Integration (enabled/state gating).
	ProfileBootstrapper *zernio.Bootstrapper
	Integration         *zernio.Integration

	// CON-203: fences the Zernio profile teardown against a concurrent CON-190
	// restore via the tenant row lock. nil disables teardown (fail-closed).
	TenantFence TenantTeardownFence

	// CON-103: the process_pdf worker's dependencies (pdf-service client,
	// embedder, storage, asset repos). A nil Client (no PDF_SERVICE_ADDR) makes
	// the job a no-op.
	PDF PDFDeps

	// CON-280: the process_document worker's dependencies (document-service
	// client, embedder, storage, asset repos). A nil Client (no
	// DOCUMENTS_SERVICE_ADDR) makes the job a no-op.
	Document DocumentDeps

	// CON-282: the process_audio worker's dependencies (audio-service client,
	// embedder, storage, extraction/segment/utterance repos, cost gate). A nil
	// Client (no AUDIO_SERVICE_ADDR) makes the job a no-op. Runs on the dedicated
	// `audio` queue.
	Audio AudioDeps

	// CON-281: the process_image worker's dependencies (image-service client,
	// embedder, storage, extraction/block/asset/file repos, cost gate). A nil
	// Client (no IMAGE_SERVICE_ADDR) makes the job a no-op. Runs on the dedicated
	// `image` queue.
	Image ImageDeps

	// CON-222: the process_url worker's dependencies (Firecrawl scrape client,
	// embedder, storage, asset/image repos, eventhub). A nil Scraper (no
	// firecrawl_api_key) makes the job a no-op.
	URL URLDeps

	// CON-154: the send_email + cleanup_email_logs workers' dependencies
	// (Resend sender, template/suppression/log repos, addressing config). A nil
	// Sender (no RESEND_API_KEY) makes send_email log skipped_disabled.
	Email EmailDeps

	// CON-217: the cleanup_zernio_connect_sessions worker's repo (sweeps expired
	// headless-connect sessions). A nil repo makes the sweep a no-op.
	ConnectSessionRepo repository.ZernioConnectSessionRepository

	// CON-190: reads a tenant's lifecycle status so per-tenant jobs (publish,
	// bootstrap, email) skip suspended/deleted tenants. repository.TenantRepository
	// satisfies it. A nil reader makes the guards no-ops (auth stays the gate).
	Tenants TenantStatusReader

	// CON-219: the detect_expiring_connections sweep's extra deps. Users resolves
	// the owner recipients; AppBaseURL builds the reconnect deep link;
	// ExpiryLeadDays is the heads-up window (days before token expiry). The
	// sweep's client/account repo come from Zernio and its email log repo from
	// Email.Logs.
	Users          repository.UserRepository
	AppBaseURL     string
	ExpiryLeadDays int

	// CON-242: notification center. Notifier lets producers (e.g. the
	// connection-expiry sweep) drop a persistent per-user notification;
	// NotificationRepo + NotificationRetention back the cleanup_notifications
	// sweep. A nil Notifier is a no-op; a nil repo makes cleanup a no-op.
	Notifier              *notify.Service
	NotificationRepo      repository.NotificationRepository
	NotificationRetention time.Duration
}

// registrars is appended to by each worker file's init(). A job is registered
// simply by existing in this package.
var registrars []func(*river.Workers, Deps)

// register records a worker registrar. Called from each job file's init().
func register(fn func(*river.Workers, Deps)) {
	registrars = append(registrars, fn)
}

// RegisterAll adds every self-registered worker to the River registry.
func RegisterAll(workers *river.Workers, deps Deps) {
	for _, fn := range registrars {
		fn(workers, deps)
	}
}

// periodicUniqueOpts is the uniqueness applied to the periodic markers
// (cleanup/reconcile/analytics): a new tick is skipped only while a previous
// one is still ACTIVE. Completed/cancelled/discarded are deliberately excluded
// — including them (River's default ByState) would block the next tick until
// the job cleaner removes the retained completed row, breaking the cadence.
func periodicUniqueOpts() river.UniqueOpts {
	return river.UniqueOpts{
		ByState: []rivertype.JobState{
			rivertype.JobStateAvailable,
			rivertype.JobStatePending,
			rivertype.JobStateRunning,
			rivertype.JobStateScheduled,
			rivertype.JobStateRetryable,
		},
	}
}

// PeriodicConfig carries the recurring-job cadences. The three recurring
// sweeps (cleanup, reconcile, analytics) are River periodic jobs rather
// than self-rescheduling tasks; River's scheduler owns the cadence and the
// chain can't die on a single failed tick.
type PeriodicConfig struct {
	CleanupEvery      time.Duration
	EmailCleanupEvery time.Duration
	ReconcileEvery    time.Duration
	AnalyticsEvery    time.Duration
	IncludeAnalytics  bool // only when the Zernio integration is configured
	FollowerEvery     time.Duration
	IncludeFollowers  bool // CON-153: only when the Zernio integration is configured
	// CON-217: expired headless-connect-session sweep. Gated on a configured
	// interval so a zero value (e.g. in tests) can't create an invalid job.
	ConnectSessionCleanupEvery time.Duration
	// CON-219: connection-health / expiry-notification sweep.
	HealthCheckEvery        time.Duration
	IncludeConnectionExpiry bool // only when the Zernio integration is configured
	// CON-242: notification retention/expiry sweep. Gated on a positive interval
	// so a zero value (e.g. in tests) can't create an invalid periodic job.
	NotificationCleanupEvery time.Duration
	// CON-285: manual-publish-due sweep. Positive-interval gated like the others.
	ManualPublishDueEvery time.Duration
}

// PeriodicJobs builds the River periodic-job set. Every job runs once on
// start (RunOnStart) and then every interval. Without RunOnStart the first
// tick fires only one full interval after the client starts, and each process
// restart re-bases that timer from zero — so a long-interval sweep (analytics
// at 30m, cleanup at 1h) can be starved indefinitely in a frequently-restarting
// environment (dev live-reload, crash-loops, rapid deploys) and never fire its
// first tick. RunOnStart makes each boot deterministically produce one tick;
// the active-state UniqueOpts dedupe overlapping inserts across a fast restart,
// and every sweep is idempotent (upserts) or a no-op when there's nothing to
// do, so the extra boot tick is harmless in a stable process too.
func (cfg PeriodicConfig) PeriodicJobs() []*river.PeriodicJob {
	runOnStart := &river.PeriodicJobOpts{RunOnStart: true}
	jobs := []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(cfg.CleanupEvery), func() (river.JobArgs, *river.InsertOpts) {
			return CleanupPostLogsTask{}, nil
		}, runOnStart),
		river.NewPeriodicJob(river.PeriodicInterval(cfg.ReconcileEvery), func() (river.JobArgs, *river.InsertOpts) {
			return ReconcileScheduledPostsTask{}, nil
		}, runOnStart),
	}
	// CON-154: email_logs retention sweep, gated on a configured interval so a
	// zero value (e.g. in tests) can't create an invalid periodic job.
	if cfg.EmailCleanupEvery > 0 {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.EmailCleanupEvery), func() (river.JobArgs, *river.InsertOpts) {
			return CleanupEmailLogsTask{}, nil
		}, runOnStart))
	}
	if cfg.IncludeAnalytics {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.AnalyticsEvery), func() (river.JobArgs, *river.InsertOpts) {
			return RefreshZernioAnalyticsTask{}, nil
		}, runOnStart))
	}
	if cfg.IncludeFollowers {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.FollowerEvery), func() (river.JobArgs, *river.InsertOpts) {
			return RefreshZernioFollowersTask{}, nil
		}, runOnStart))
	}
	if cfg.ConnectSessionCleanupEvery > 0 {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.ConnectSessionCleanupEvery), func() (river.JobArgs, *river.InsertOpts) {
			return CleanupZernioConnectSessionsTask{}, nil
		}, runOnStart))
	}
	// CON-219: connection-health / expiry sweep. Also requires a positive
	// interval — River doesn't validate it and PeriodicInterval(0).Next(t)==t, so
	// a zero HealthCheckEvery would spin the periodic enqueuer (mirrors the
	// EmailCleanup/ConnectSessionCleanup guards above).
	if cfg.IncludeConnectionExpiry && cfg.HealthCheckEvery > 0 {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.HealthCheckEvery), func() (river.JobArgs, *river.InsertOpts) {
			return DetectExpiringConnectionsTask{}, nil
		}, runOnStart))
	}
	// CON-242: notification retention/expiry sweep. Positive-interval guard like
	// the EmailCleanup/ConnectSessionCleanup sweeps above.
	if cfg.NotificationCleanupEvery > 0 {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.NotificationCleanupEvery), func() (river.JobArgs, *river.InsertOpts) {
			return CleanupNotificationsTask{}, nil
		}, runOnStart))
	}
	// CON-285: manual-publish-due sweep. Positive-interval guard like above.
	if cfg.ManualPublishDueEvery > 0 {
		jobs = append(jobs, river.NewPeriodicJob(river.PeriodicInterval(cfg.ManualPublishDueEvery), func() (river.JobArgs, *river.InsertOpts) {
			return DetectManualPublishDueTask{}, nil
		}, runOnStart))
	}
	return jobs
}

// Enqueuer is the app-facing enqueue surface. It wraps the River client so
// callers (the schedule service, the posts handler) depend on a tiny method
// set rather than River directly. The client is generic over *sql.Tx
// because River runs on bun's shared database/sql pool — so InsertTx joins
// the exact bun transaction the schedule path opens.
type Enqueuer struct {
	Client *river.Client[*sql.Tx]
}

// requestIDMetadata returns River job metadata carrying the originating request
// id (CON-107) when one is present on ctx, so a job's logs correlate back to the
// HTTP request that enqueued it. Returns nil for system/periodic enqueues that
// have no request id (River treats nil as empty metadata).
func requestIDMetadata(ctx context.Context) []byte {
	rid, ok := logging.RequestIDFrom(ctx)
	if !ok {
		return nil
	}
	b, err := json.Marshal(map[string]string{logging.AttrRequestID: rid})
	if err != nil {
		return nil
	}
	return b
}

// insertOptsWithRequestID merges the originating request id into opts as job
// metadata (creating opts if nil). When ctx carries no request id, opts is
// returned unchanged.
func insertOptsWithRequestID(ctx context.Context, opts *river.InsertOpts) *river.InsertOpts {
	meta := requestIDMetadata(ctx)
	if meta == nil {
		return opts
	}
	if opts == nil {
		opts = &river.InsertOpts{}
	}
	opts.Metadata = meta
	return opts
}

// WithJobRequestID re-attaches the originating request id (from the job row's
// metadata) to a worker context so the job's log lines carry the same
// request_id as the enqueuing request. Tenant attachment stays the worker's
// responsibility (tenantctx.With). Takes the *rivertype.JobRow (not the
// promoted job.Metadata field) so it is nil-safe: unit tests that construct a
// river.Job without a JobRow, and jobs enqueued without metadata, both return
// ctx unchanged rather than panicking.
func WithJobRequestID(ctx context.Context, jr *rivertype.JobRow) context.Context {
	if jr == nil || len(jr.Metadata) == 0 {
		return ctx
	}
	var m struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(jr.Metadata, &m); err == nil && m.RequestID != "" {
		ctx = logging.WithRequestID(ctx, m.RequestID)
	}
	return ctx
}

// EnqueueSubmitTx enqueues a submit task inside the given transaction, so
// the enqueue commits atomically with the post status change (CON-78 §9).
func (e *Enqueuer) EnqueueSubmitTx(ctx context.Context, tx *sql.Tx, postID string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, SubmitPostTask{PostID: postID}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueBootstrapProfileTx enqueues an eager Zernio profile-provisioning task
// inside the given transaction, so it commits atomically with the tenant insert
// (CON-102 §6 FR2): a rolled-back signup creates no job, a committed one
// durably queues exactly one. A nil enqueuer (Zernio queue unwired) is a no-op —
// the lazy on-connect bootstrap remains the fallback.
func (e *Enqueuer) EnqueueBootstrapProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, BootstrapZernioProfileTask{TenantID: tenantID}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueTeardownProfileTx enqueues a Zernio profile-teardown task inside the
// given transaction, so it commits atomically with the workspace soft-delete
// (CON-203, follow-up to CON-147 PR4): a rolled-back delete queues no job, a
// committed one durably queues exactly one. The enqueue is a local DB insert —
// the Zernio deletes happen later in the worker — so the delete request never
// blocks on Zernio reachability. A nil enqueuer (Zernio queue unwired) is a
// no-op; the profile is simply left orphaned, as it was before CON-203.
func (e *Enqueuer) EnqueueTeardownProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, TeardownZernioProfileTask{TenantID: tenantID}, insertOptsWithRequestID(ctx, nil))
	return err
}

// dripSchedule is the marketing onboarding drip cadence (CON-154 FR5): one
// in-code table, so rescheduling the whole sequence is a single edit. Offsets
// are relative to signup.
var dripSchedule = []struct {
	Key    string
	Offset time.Duration
}{
	{templates.KeyDripDay2, 2 * 24 * time.Hour},
	{templates.KeyDripDay5, 5 * 24 * time.Hour},
	{templates.KeyDripDay7, 7 * 24 * time.Hour},
}

// EnqueueWelcomeEmailTx enqueues the transactional welcome email inside the
// signup transaction, so the job exists iff the tenant/user do (CON-154 FR4).
// The enqueue is a local DB insert — the Resend call happens later in the
// worker — so signup never blocks on Resend reachability. A nil enqueuer (email
// queue unwired) is a no-op.
func (e *Enqueuer) EnqueueWelcomeEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, SendEmailTask{
		UserID:         userID,
		TenantID:       tenantID,
		TemplateKey:    templates.KeyWelcome,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "welcome:" + userID,
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueuePasswordResetTx enqueues the transactional password-reset email inside
// the token-minting transaction, so the mail exists iff the token row does
// (CON-161). resetURL is the one-time link; it rides the job args as a template
// var because the worker cannot rebuild it — only the token's hash is stored.
// The enqueue is a local DB insert (the Resend call happens later in the
// worker), so the request never blocks on Resend reachability. tokenID keys the
// idempotency so each distinct request queues its own mail. A nil enqueuer
// (email queue unwired) is a no-op.
func (e *Enqueuer) EnqueuePasswordResetTx(ctx context.Context, tx *sql.Tx, userID, tenantID, tokenID, resetURL string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, SendEmailTask{
		UserID:         userID,
		TenantID:       tenantID,
		TemplateKey:    templates.KeyPasswordReset,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "password_reset:" + tokenID,
		Vars:           map[string]string{"reset_url": resetURL},
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueInvitationEmailTx enqueues the transactional workspace-invitation email
// inside the invite-minting transaction, so the mail exists iff the invitation
// row does (CON-26). The invitee has no users row yet, so the recipient address
// rides the task directly (ToEmail) rather than being re-resolved from users;
// inviteURL / inviterName / workspaceName / role ride the args as template vars
// because the worker cannot rebuild them (only the token's hash is stored). The
// enqueue is a local DB insert (the Resend call happens later in the worker), so
// the request never blocks on Resend reachability. invitationID keys the
// idempotency so each distinct invite queues its own mail. A nil enqueuer (email
// queue unwired) is a no-op.
func (e *Enqueuer) EnqueueInvitationEmailTx(ctx context.Context, tx *sql.Tx, tenantID, invitationID, toEmail, inviteURL, inviterName, workspaceName, role string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, SendEmailTask{
		TenantID:       tenantID,
		ToEmail:        toEmail,
		TemplateKey:    templates.KeyInvitation,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "invitation:" + invitationID,
		Vars: map[string]string{
			"invite_url":     inviteURL,
			"inviter_name":   inviterName,
			"workspace_name": workspaceName,
			"role":           role,
		},
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueDripTx enqueues the marketing onboarding drip (day 2/5/7) as delayed
// jobs inside the signup transaction (CON-154 FR5). Each step fires at its
// ScheduledAt; unsubscribes are honoured at send time, so no scheduled job ever
// needs cancelling. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueDripTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	now := time.Now().UTC()
	for _, step := range dripSchedule {
		opts := insertOptsWithRequestID(ctx, &river.InsertOpts{ScheduledAt: now.Add(step.Offset)})
		if _, err := e.Client.InsertTx(ctx, tx, SendEmailTask{
			UserID:         userID,
			TenantID:       tenantID,
			TemplateKey:    step.Key,
			EmailKind:      models.EmailKindMarketing,
			IdempotencyKey: step.Key + ":" + userID,
		}, opts); err != nil {
			return err
		}
	}
	return nil
}

// EnqueueProcessPDFTx enqueues a PDF-ingestion task inside the given
// transaction, so it commits atomically with the asset insert (CON-103): a
// committed upload always has a job, a rolled-back one never does. The worker
// re-reads original.pdf from storage, so the bytes are not in the args. Takes
// primitives so the handler can depend on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessPDFTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, ProcessPDFTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueProcessDocumentTx enqueues a document-ingestion task inside the given
// transaction, so it commits atomically with the asset insert (CON-280): a
// committed upload always has a job, a rolled-back one never does. The worker
// re-reads the original from storage (storageKey is the tenant-relative object
// path), so the bytes are not in the args. Takes primitives so the handler can
// depend on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessDocumentTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, ProcessDocumentTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
		StorageKey:   storageKey,
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueProcessAudioTx enqueues an audio-ingestion task inside the given
// transaction, so it commits atomically with the asset insert/reset (CON-282):
// a committed upload always has a job, a rolled-back one never does. The worker
// hands audio-service presigned URLs (storageKey is the tenant-relative object
// path), so the bytes are not in the args. runKey makes the run idempotent; an
// empty pinnedModel uses the configured TRANSCRIBE_MODEL. The task's InsertOpts
// routes it onto the dedicated `audio` queue. Takes primitives so the handler
// depends on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessAudioTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey, runKey, pinnedModel string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, ProcessAudioTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
		StorageKey:   storageKey,
		RunKey:       runKey,
		PinnedModel:  pinnedModel,
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueProcessImageTx enqueues an image-ingestion task inside the given
// transaction, so it commits atomically with the asset insert/reset (CON-281): a
// committed upload always has a job, a rolled-back one never does. The worker
// hands image-service presigned URLs (storageKey is the tenant-relative object
// path), so the bytes are not in the args. runKey makes the run idempotent; an
// empty pinnedModel uses the configured VISION_EXTRACT_MODEL. The task's
// InsertOpts routes it onto the dedicated `image` queue. Takes primitives so the
// handler depends on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessImageTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey, runKey, pinnedModel string) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, ProcessImageTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
		StorageKey:   storageKey,
		RunKey:       runKey,
		PinnedModel:  pinnedModel,
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueProcessURLTx enqueues a URL-scrape task inside the given transaction,
// so it commits atomically with the asset insert/reset (CON-222): a committed
// submit always has a job, a rolled-back one never does. Refresh flips
// Firecrawl's cache off for a re-submit of an existing URL. Takes primitives so
// the handler depends on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessURLTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, sourceURL string, refresh bool) error {
	if e == nil || e.Client == nil {
		return nil
	}
	_, err := e.Client.InsertTx(ctx, tx, ProcessURLTask{
		AssetID:   assetID,
		TenantID:  tenantID,
		SourceURL: sourceURL,
		Refresh:   refresh,
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

// EnqueueCancel enqueues a cancellation task (non-transactional).
func (e *Enqueuer) EnqueueCancel(ctx context.Context, postID string, target CancelTarget, actor string) error {
	if e == nil || e.Client == nil {
		return fmt.Errorf("jobs: enqueuer not configured")
	}
	_, err := e.Client.Insert(ctx, CancelZernioJobTask{PostID: postID, Target: target, Actor: actor}, insertOptsWithRequestID(ctx, nil))
	return err
}
