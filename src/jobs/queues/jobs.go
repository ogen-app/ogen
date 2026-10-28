package queues

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// Background jobs run in river-service. ogen enqueues them through the job
// outbox and serves their steps (JobStepService) and maintenance passes
// (MaintenanceService) over the job RPC listener. This file holds the
// dependency bundle and the app-facing enqueue surface.

// Deps bundles every dependency the jobs' steps and maintenance passes need.
// server.go builds one for the job RPC server. The cleanup/reconcile repos are
// the same instances already held by the Zernio bundle, so they aren't
// duplicated here.
type Deps struct {
	Zernio           ZernioDeps
	PostLogRetention time.Duration
	ReconcileGrace   time.Duration
	// OrphanSweep tunes the Zernio orphan sweep; zero value is a dry run.
	OrphanSweep OrphanSweepConfig
	// PendingUploadSweep tunes the abandoned-upload sweep; zero value is a
	// dry run.
	PendingUploadSweep  PendingUploadSweepConfig
	PendingUploadRepo   repository.PendingUploadRepository
	AnalyticsSettings   zernio.SettingsStore
	AnalyticsHub        eventhub.Hub
	AnalyticsWindowDays int
	// Age-based refresh-decay schedule. Zero fields fall back to the
	// processor defaults (fresh<48h→hourly, warm<14d→daily, else weekly).
	AnalyticsDecay AnalyticsDecay
	// Eager per-tenant Zernio profile provisioning. The bootstrap job
	// reuses the connect-link handler's Bootstrapper (tenant-scoped profile
	// create/adopt + settings write) and Integration (enabled/state gating).
	ProfileBootstrapper *zernio.Bootstrapper
	Integration         *zernio.Integration

	// TeardownLease guards a teardown river-service runs, step by step,
	// against a concurrent restore.
	TeardownLease TeardownLease

	// Billing runs the billing sweeps and scheduled plan changes; BillingLive
	// gates the unpaid-trial teardown. Nil when billing is off: the jobs are
	// no-ops and that teardown is skipped.
	Billing     BillingJobs
	BillingLive BillingLiveChecker
	// BillingSteps runs the billing jobs for river-service; nil leaves them
	// to ogen's own workers.
	BillingSteps BillingSteps
	// BillingPayments runs the payments sync; nil leaves it unregistered.
	BillingPayments BillingPayments

	// The process_pdf worker's dependencies (pdf-service client,
	// embedder, storage, asset repos). A nil Client (no PDF_SERVICE_ADDR) makes
	// the job a no-op.
	PDF PDFDeps

	// The process_document worker's dependencies (document-service
	// client, embedder, storage, asset repos). A nil Client (no
	// DOCUMENTS_SERVICE_ADDR) makes the job a no-op.
	Document DocumentDeps

	// The process_audio worker's dependencies (audio-service client,
	// embedder, storage, extraction/segment/utterance repos, cost gate). A nil
	// Client (no AUDIO_SERVICE_ADDR) makes the job a no-op. Runs on the dedicated
	// `audio` queue.
	Audio AudioDeps

	// The analyze_asset / reanalyze_campaign workers' dependencies. A nil
	// Runner makes both jobs no-ops. Runs on the dedicated `analysis` queue.
	Analysis AnalysisDeps
	// BrandDistill runs brand distillations and expires their proposals.
	BrandDistill BrandDistillDeps

	// The process_image worker's dependencies (image-service client,
	// embedder, storage, extraction/block/asset/file repos, cost gate). A nil
	// Client (no IMAGE_SERVICE_ADDR) makes the job a no-op. Runs on the dedicated
	// `image` queue.
	Image ImageDeps

	// The process_url worker's dependencies (Firecrawl scrape client,
	// embedder, storage, asset/image repos, eventhub). A nil Scraper (no
	// firecrawl_api_key) makes the job a no-op.
	URL URLDeps

	// The send_email + cleanup_email_logs workers' dependencies
	// (Resend sender, template/suppression/log repos, addressing config). A nil
	// Sender (no RESEND_API_KEY) makes send_email log skipped_disabled.
	Email EmailDeps

	// The cleanup_zernio_connect_sessions worker's repo (sweeps expired
	// headless-connect sessions). A nil repo makes the sweep a no-op.
	ConnectSessionRepo repository.ZernioConnectSessionRepository

	// The cleanup_plugin_pairings worker's repo. A nil repo makes the sweep a
	// no-op.
	PluginPairingRepo repository.PluginPairingRepository

	// The cleanup_login_security sweep's repos. Nil repos skip their half.
	KnownDeviceRepo     repository.KnownDeviceRepository
	SessionRepo         repository.SessionRepository
	LoginAlertTokenRepo repository.LoginAlertTokenRepository
	OAuthRepo           repository.OAuthRepository

	// Reads a tenant's lifecycle status so per-tenant jobs (publish,
	// bootstrap, email) skip suspended/deleted tenants. repository.TenantRepository
	// satisfies it. A nil reader makes the guards no-ops (auth stays the gate).
	Tenants TenantStatusReader

	// The detect_expiring_connections sweep's extra deps. Users resolves
	// the owner recipients; AppBaseURL builds the reconnect deep link;
	// ExpiryLeadDays is the heads-up window (days before token expiry) used
	// when Zernio reports no health status. The
	// sweep's client/account repo come from Zernio and its email log repo from
	// Email.Logs.
	Users          repository.UserRepository
	AppBaseURL     string
	ExpiryLeadDays int

	// Notification center. Notifier lets producers (e.g. the
	// connection-expiry sweep) drop a persistent per-user notification;
	// NotificationRepo + NotificationRetention back the cleanup_notifications
	// sweep. A nil Notifier is a no-op; a nil repo makes cleanup a no-op.
	Notifier              *notify.Service
	NotificationRepo      repository.NotificationRepository
	NotificationRetention time.Duration

	// The weekly report jobs. A nil Store or Collector makes them no-ops.
	WeeklyReport WeeklyReportDeps

	// Jobs is the app's enqueuer, for steps and sweeps that queue follow-up
	// jobs (a re-queued analysis, a withdrawal).
	Jobs *Enqueuer
}

// JobArgs is a job's arguments: its kind, and fields mirrored field for field
// by the jobs.v1 message river-service receives (see setArgs). Queue,
// attempts and uniqueness are river-service's, per kind.
type JobArgs interface {
	Kind() string
}

// EnqueueOpts schedules a job. The zero value runs it as soon as river-service
// picks it up.
type EnqueueOpts struct {
	ScheduledAt time.Time
}

// Enqueuer is the app-facing enqueue surface: callers (the schedule service,
// the posts handler) depend on a tiny method set rather than on the outbox.
// Each enqueue writes a job_outbox row, in the caller's transaction when it
// has one, and the relay delivers it to river-service.
type Enqueuer struct {
	outbox *outbox.Writer
}

// NewEnqueuer returns an enqueuer writing to w.
func NewEnqueuer(w *outbox.Writer) *Enqueuer {
	return &Enqueuer{outbox: w}
}

// insertTx writes args to the outbox inside tx, so the job exists iff tx
// commits. A nil enqueuer is a no-op.
func (e *Enqueuer) insertTx(ctx context.Context, tx *sql.Tx, args JobArgs, opts *EnqueueOpts) error {
	if e == nil || e.outbox == nil {
		return nil
	}
	req, err := enqueueRequest(args, opts)
	if err != nil {
		return err
	}
	return e.outbox.WriteTx(ctx, tx, args.Kind(), req)
}

// insert is insertTx for callers with no transaction. A nil enqueuer is a
// no-op.
func (e *Enqueuer) insert(ctx context.Context, args JobArgs, opts *EnqueueOpts) error {
	if e == nil || e.outbox == nil {
		return nil
	}
	req, err := enqueueRequest(args, opts)
	if err != nil {
		return err
	}
	return e.outbox.Write(ctx, args.Kind(), req)
}

// enqueue is insert for jobs whose loss matters (a follow-up of another job,
// a cancel the user asked for): with no enqueuer wired it fails rather than
// doing nothing.
func (e *Enqueuer) enqueue(ctx context.Context, args JobArgs, opts *EnqueueOpts) error {
	if e == nil || e.outbox == nil {
		return fmt.Errorf("jobs: no enqueuer for %s", args.Kind())
	}
	return e.insert(ctx, args, opts)
}

// EnqueueSubmitTx enqueues a submit task inside the given transaction, so
// the enqueue commits atomically with the post status change.
func (e *Enqueuer) EnqueueSubmitTx(ctx context.Context, tx *sql.Tx, postID string) error {
	return e.insertTx(ctx, tx, SubmitPostTask{PostID: postID}, nil)
}

// EnqueueWithdrawTx enqueues a Zernio withdrawal inside the given transaction,
// so it commits atomically with the delete or unschedule that orphaned the
// Zernio post: a committed change always gets its withdrawal.
func (e *Enqueuer) EnqueueWithdrawTx(ctx context.Context, tx *sql.Tx, task WithdrawZernioPostTask) error {
	return e.insertTx(ctx, tx, task, nil)
}

// EnqueueBootstrapProfileTx enqueues an eager Zernio profile-provisioning task
// inside the given transaction, so it commits atomically with the tenant insert:
// a rolled-back signup creates no job, a committed one
// durably queues exactly one. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueBootstrapProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error {
	return e.insertTx(ctx, tx, BootstrapZernioProfileTask{TenantID: tenantID}, nil)
}

// EnqueueTeardownProfileTx enqueues a Zernio profile-teardown task inside the
// given transaction, so it commits atomically with the workspace soft-delete
// (CON-203, follow-up to CON-147 PR4): a rolled-back delete queues no job, a
// committed one durably queues exactly one. The enqueue is a local DB insert —
// the Zernio deletes happen later in the worker — so the delete request never
// blocks on Zernio reachability. A nil enqueuer (Zernio queue unwired) is a
// no-op; the profile is simply left orphaned, as it was before CON-203.
func (e *Enqueuer) EnqueueTeardownProfileTx(ctx context.Context, tx *sql.Tx, tenantID string) error {
	return e.insertTx(ctx, tx, TeardownZernioProfileTask{TenantID: tenantID}, nil)
}

// EnqueueTrialTeardownTx enqueues the unpaid-trial Zernio teardown inside the
// transaction that records the expiry, so it exists iff the expiry does.
func (e *Enqueuer) EnqueueTrialTeardownTx(ctx context.Context, tx *sql.Tx, tenantID string) error {
	return e.insertTx(ctx, tx, TeardownZernioProfileTask{TenantID: tenantID, Reason: TeardownReasonTrialUnpaid}, nil)
}

// dripSchedule is the marketing onboarding drip cadence: one
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
// signup transaction, so the job exists iff the tenant/user do.
// The enqueue is a local DB insert — the Resend call happens later in the
// worker — so signup never blocks on Resend reachability. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueWelcomeEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	return e.insertTx(ctx, tx, SendEmailTask{
		UserID:         userID,
		TenantID:       tenantID,
		TemplateKey:    templates.KeyWelcome,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "welcome:" + userID,
	}, nil)
}

// EnqueueAdminTenantRegisteredEmail enqueues one admin_tenant_registered
// operator-notification send to a single admin recipient. Unlike the
// *Tx enqueuers this runs OUTSIDE any transaction — it is driven by Harbor's
// gRPC callback after a tenant has already committed — so it is a plain (non-tx)
// Insert. The tenant's details ride the job args as vars because the operator
// recipient has no user/tenant to resolve at send time. Idempotency is keyed by
// (tenant, recipient) so a retried webhook can't double-send. A nil enqueuer
// (email queue unwired) is a no-op.
func (e *Enqueuer) EnqueueAdminTenantRegisteredEmail(ctx context.Context, tenantID, recipient string, vars map[string]string) error {
	return e.insert(ctx, SendEmailTask{
		TenantID:       tenantID,
		ToEmail:        recipient,
		TemplateKey:    templates.KeyAdminTenantRegistered,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "admin_tenant_registered:" + tenantID + ":" + recipient,
		Vars:           vars,
	}, nil)
}

// EnqueueNotifyHarborTenantRegisteredTx enqueues the CON-229 "notify Harbor a
// new tenant registered" webhook job inside the signup transaction, so the
// operator notification fires iff the tenant commits. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueNotifyHarborTenantRegisteredTx(ctx context.Context, tx *sql.Tx, tenantID string) error {
	return e.insertTx(ctx, tx, NotifyHarborTenantRegisteredTask{TenantID: tenantID}, nil)
}

// EnqueuePasswordResetTx enqueues the transactional password-reset email inside
// the token-minting transaction, so the mail exists iff the token row does.
// resetURL is the one-time link; it rides the job args as a template
// var because the worker cannot rebuild it — only the token's hash is stored.
// The enqueue is a local DB insert (the Resend call happens later in the
// worker), so the request never blocks on Resend reachability. tokenID keys the
// idempotency so each distinct request queues its own mail. A nil enqueuer
// (email queue unwired) is a no-op.
func (e *Enqueuer) EnqueuePasswordResetTx(ctx context.Context, tx *sql.Tx, userID, tenantID, tokenID, resetURL string) error {
	return e.insertTx(ctx, tx, SendEmailTask{
		UserID:         userID,
		TenantID:       tenantID,
		TemplateKey:    templates.KeyPasswordReset,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "password_reset:" + tokenID,
		Vars:           map[string]string{"reset_url": resetURL},
	}, nil)
}

// NewDeviceLoginVars are the sign-in details a new-device alert shows. They
// ride the job args because the worker can't rebuild them: the secure link's
// token exists only as a hash.
type NewDeviceLoginVars struct {
	LoginTime   string
	DeviceLabel string
	IPAddress   string
	Location    string
	SecureURL   string
	MaskedEmail string
	// LoginMethod names the provider the sign-in went through ("Google");
	// empty for a password sign-in.
	LoginMethod string
}

// EnqueueNewDeviceLoginEmailTx enqueues the transactional new-device alert
// inside the transaction that stores its alert token, so the mail exists iff
// the token does. The recipient is re-resolved from userID at send time.
// tokenID keys the idempotency. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueNewDeviceLoginEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID, tokenID string, v NewDeviceLoginVars) error {
	return e.insertTx(ctx, tx, SendEmailTask{
		UserID:         userID,
		TenantID:       tenantID,
		TemplateKey:    templates.KeyNewDeviceLogin,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "new_device_login:" + tokenID,
		Vars: map[string]string{
			"login_time":   v.LoginTime,
			"device_label": v.DeviceLabel,
			"ip_address":   v.IPAddress,
			"location":     v.Location,
			"secure_url":   v.SecureURL,
			"masked_email": v.MaskedEmail,
			"login_method": v.LoginMethod,
		},
	}, nil)
}

// EnqueueGoogleLinkedEmailTx enqueues the transactional "Google sign-in is now
// connected" notice inside the transaction that links the identity, so the mail
// exists iff the link does. identityID keys the idempotency. A nil enqueuer is a
// no-op.
func (e *Enqueuer) EnqueueGoogleLinkedEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID, identityID, googleEmail string, passwordCleared bool) error {
	return e.insertTx(ctx, tx, SendEmailTask{
		UserID:         userID,
		TenantID:       tenantID,
		TemplateKey:    templates.KeyGoogleLinked,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "google_linked:" + identityID,
		Vars: map[string]string{
			"google_email":     googleEmail,
			"password_cleared": strconv.FormatBool(passwordCleared),
		},
	}, nil)
}

// EnqueueBillingEmailTx enqueues a transactional billing notice to one owner
// inside the transaction that records the billing change, so the mail exists
// iff the change does. idempotencyKey distinguishes one notice per event.
// A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueBillingEmailTx(ctx context.Context, tx *sql.Tx, userID, tenantID, templateKey, idempotencyKey string) error {
	return e.insertTx(ctx, tx, SendEmailTask{
		UserID:         userID,
		TenantID:       tenantID,
		TemplateKey:    templateKey,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: idempotencyKey,
	}, nil)
}

// EnqueueInvitationEmailTx enqueues the transactional workspace-invitation email
// inside the invite-minting transaction, so the mail exists iff the invitation
// row does. The invitee has no users row yet, so the recipient address
// rides the task directly (ToEmail) rather than being re-resolved from users;
// inviteURL / inviterName / workspaceName / role ride the args as template vars
// because the worker cannot rebuild them (only the token's hash is stored). The
// enqueue is a local DB insert (the Resend call happens later in the worker), so
// the request never blocks on Resend reachability. invitationID keys the
// idempotency so each distinct invite queues its own mail. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueInvitationEmailTx(ctx context.Context, tx *sql.Tx, tenantID, invitationID, toEmail, inviteURL, inviterName, workspaceName, role string) error {
	return e.insertTx(ctx, tx, SendEmailTask{
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
	}, nil)
}

// EnqueueTenantInviteEmailTx enqueues a tester invite email inside the
// transaction that mints or rotates the invite's token, so the mail exists iff
// the token does. The recipient has no account yet and the mail has no tenant.
// The idempotency key names the send (tenant_invite:<id>:<send>), so a resend
// is a new email and the operator view finds the latest one by prefix.
func (e *Enqueuer) EnqueueTenantInviteEmailTx(ctx context.Context, tx *sql.Tx, inviteID string, send int, toEmail, inviteURL, expiresAt, message string) error {
	return e.insertTx(ctx, tx, SendEmailTask{
		ToEmail:        toEmail,
		TemplateKey:    templates.KeyTenantInvite,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: "tenant_invite:" + inviteID + ":" + strconv.Itoa(send),
		Vars: map[string]string{
			"invite_url": inviteURL,
			"expires_at": expiresAt,
			"message":    message,
		},
	}, nil)
}

// EnqueueDripTx enqueues the marketing onboarding drip (day 2/5/7) as delayed
// jobs inside the signup transaction. Each step fires at its
// ScheduledAt; unsubscribes are honoured at send time, so no scheduled job ever
// needs cancelling. A nil enqueuer is a no-op.
func (e *Enqueuer) EnqueueDripTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error {
	now := time.Now().UTC()
	for _, step := range dripSchedule {
		if err := e.insertTx(ctx, tx, SendEmailTask{
			UserID:         userID,
			TenantID:       tenantID,
			TemplateKey:    step.Key,
			EmailKind:      models.EmailKindMarketing,
			IdempotencyKey: step.Key + ":" + userID,
		}, &EnqueueOpts{ScheduledAt: now.Add(step.Offset)}); err != nil {
			return err
		}
	}
	return nil
}

// EnqueueProcessPDFTx enqueues a PDF-ingestion task inside the given
// transaction, so it commits atomically with the asset insert: a
// committed upload always has a job, a rolled-back one never does. The worker
// re-reads original.pdf from storage, so the bytes are not in the args. Takes
// primitives so the handler can depend on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessPDFTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType string) error {
	return e.insertTx(ctx, tx, ProcessPDFTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
	}, nil)
}

// EnqueueProcessDocumentTx enqueues a document-ingestion task inside the given
// transaction, so it commits atomically with the asset insert: a
// committed upload always has a job, a rolled-back one never does. The worker
// re-reads the original from storage (storageKey is the tenant-relative object
// path), so the bytes are not in the args. Takes primitives so the handler can
// depend on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessDocumentTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey string) error {
	return e.insertTx(ctx, tx, ProcessDocumentTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
		StorageKey:   storageKey,
	}, nil)
}

// EnqueueProcessAudioTx enqueues an audio-ingestion task inside the given
// transaction, so it commits atomically with the asset insert/reset:
// a committed upload always has a job, a rolled-back one never does. The worker
// hands audio-service presigned URLs (storageKey is the tenant-relative object
// path), so the bytes are not in the args. runKey makes the run idempotent; an
// empty pinnedModel uses the transcribe/main model slot. The task's InsertOpts
// routes it onto the dedicated `audio` queue. Takes primitives so the handler
// depends on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessAudioTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey, runKey, pinnedModel string) error {
	return e.insertTx(ctx, tx, ProcessAudioTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
		StorageKey:   storageKey,
		RunKey:       runKey,
		PinnedModel:  pinnedModel,
	}, nil)
}

// EnqueueProcessImageTx enqueues an image-ingestion task inside the given
// transaction, so it commits atomically with the asset insert/reset: a
// committed upload always has a job, a rolled-back one never does. The worker
// hands image-service presigned URLs (storageKey is the tenant-relative object
// path), so the bytes are not in the args. runKey makes the run idempotent; an
// empty pinnedModel uses the vision/extract model slot. The task's
// InsertOpts routes it onto the dedicated `image` queue. Takes primitives so the
// handler depends on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessImageTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, originalName, mimeType, storageKey, runKey, pinnedModel string) error {
	return e.insertTx(ctx, tx, ProcessImageTask{
		AssetID:      assetID,
		TenantID:     tenantID,
		OriginalName: originalName,
		MimeType:     mimeType,
		StorageKey:   storageKey,
		RunKey:       runKey,
		PinnedModel:  pinnedModel,
	}, nil)
}

// EnqueueReembedImage enqueues a re-embed of an image asset's edited description
// plus its stored region blocks. Not transactional: the description is
// already saved, and the worker re-reads it, so a lost enqueue only leaves the
// previous chunks in place until the next edit or re-extract.
func (e *Enqueuer) EnqueueReembedImage(ctx context.Context, assetID, tenantID string) error {
	return e.insert(ctx, ReembedImageTask{AssetID: assetID, TenantID: tenantID}, nil)
}

// EnqueueProcessURLTx enqueues a URL-scrape task inside the given transaction,
// so it commits atomically with the asset insert/reset: a committed
// submit always has a job, a rolled-back one never does. Refresh flips
// Firecrawl's cache off for a re-submit of an existing URL. Takes primitives so
// the handler depends on a narrow interface, not this package.
func (e *Enqueuer) EnqueueProcessURLTx(ctx context.Context, tx *sql.Tx, assetID, tenantID, sourceURL string, refresh bool) error {
	return e.insertTx(ctx, tx, ProcessURLTask{
		AssetID:   assetID,
		TenantID:  tenantID,
		SourceURL: sourceURL,
		Refresh:   refresh,
	}, nil)
}

// EnqueueCancel enqueues a cancellation task (non-transactional).
func (e *Enqueuer) EnqueueCancel(ctx context.Context, postID string, target CancelTarget, actor string) error {
	return e.enqueue(ctx, CancelZernioJobTask{PostID: postID, Target: target, Actor: actor}, nil)
}
