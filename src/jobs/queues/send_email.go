package queues

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// SendEmailQueue is the single async send path for all mail. Welcome
// (transactional, immediate) and the marketing drip (delayed via ScheduledAt)
// both flow through it; the recipient + suppression are resolved fresh at send
// time so an unsubscribe or email change is honoured without touching the
// already-scheduled jobs.
const SendEmailQueue = "send_email"

// SendEmailTask carries the addressing indirection (user + tenant) rather than
// a baked recipient, so the worker re-resolves the current address. EmailKind
// (note: the field is not Kind — that name is taken by the river.JobArgs method)
// drives suppression semantics + whether an unsubscribe footer is required.
type SendEmailTask struct {
	UserID         string           `json:"user_id"`
	TenantID       string           `json:"tenant_id"`
	TemplateKey    string           `json:"template_key"`
	EmailKind      models.EmailKind `json:"email_kind"`
	IdempotencyKey string           `json:"idempotency_key"`
	// ToEmail / ToName address a recipient who is NOT (yet) a user — e.g. an
	// invitee, who has no users row until they accept. Used only when
	// UserID is empty: the worker then sends to ToEmail directly instead of
	// re-resolving the address from users. Exactly one of UserID / ToEmail is set.
	ToEmail string `json:"to_email,omitempty"`
	ToName  string `json:"to_name,omitempty"`
	// Vars carries per-message template variables that aren't derivable from the
	// user/tenant/config at send time — e.g. the one-time password-reset URL
	// or the invitation accept link + inviter + role. Empty for
	// templates that render purely from the resolved Data.
	Vars map[string]string `json:"vars,omitempty"`
}

// Kind implements river.JobArgs.
func (SendEmailTask) Kind() string { return SendEmailQueue }

// InsertOpts bounds retries and dedupes by args. The hard idempotency backstops
// are the Resend Idempotency-Key header + the email_logs unique index; ByArgs
// just stops an accidental duplicate enqueue from queueing twice.
func (SendEmailTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// SendEmailProcessor is the River worker for send_email.
type SendEmailProcessor struct {
	river.WorkerDefaults[SendEmailTask]
	Deps EmailDeps
	// Tenants skips mail for a suspended/deleted tenant — e.g. a drip
	// step scheduled before the tenant was frozen. Nil = no gate.
	Tenants TenantStatusReader
}

// Work is the River entrypoint; it runs under the task's tenant scope so the
// recipient lookup is correctly isolated.
func (p *SendEmailProcessor) Work(ctx context.Context, job *river.Job[SendEmailTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *SendEmailProcessor) Timeout(*river.Job[SendEmailTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &SendEmailProcessor{Deps: d.Email, Tenants: d.Tenants})
	})
}

// Process renders + sends one email. Only transient failures return an error
// (so River retries); every terminal outcome (skip, disabled, render failure,
// 4xx) is logged and returns nil.
func (p *SendEmailProcessor) Process(ctx context.Context, t SendEmailTask) error {
	dep := p.Deps
	if t.TemplateKey == "" || (t.UserID == "" && t.ToEmail == "") {
		slog.WarnContext(ctx, "send_email skipped: missing args", logging.AttrComponent, sendEmailComponent)
		return nil
	}
	if dep.Templates == nil {
		slog.WarnContext(ctx, "send_email skipped: deps not wired", logging.AttrComponent, sendEmailComponent)
		return nil
	}
	ctx = tenantctx.With(ctx, t.TenantID)

	// A frozen tenant sends no mail. A DB error retries.
	active, err := tenantIsActive(ctx, p.Tenants, t.TenantID)
	if err != nil {
		return err
	}
	if !active {
		slog.InfoContext(ctx, "send_email skipped: tenant not active", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey)
		return nil
	}

	rcpt, ok, err := p.resolveRecipient(ctx, t)
	if !ok {
		return err
	}
	logBase := models.EmailLog{
		TenantID:       t.TenantID,
		UserID:         t.UserID,
		TemplateID:     t.TemplateKey,
		Kind:           t.EmailKind,
		ToEmail:        rcpt.email,
		Provider:       models.ProviderResend,
		IdempotencyKey: t.IdempotencyKey,
	}
	return p.deliver(ctx, t, rcpt, logBase)
}

// deliver runs the send-time gates (sending disabled, suppression), renders
// the template and sends it, logging every terminal outcome.
func (p *SendEmailProcessor) deliver(ctx context.Context, t SendEmailTask, rcpt emailRecipient, logBase models.EmailLog) error {
	dep := p.Deps
	// Sending disabled (no Resend key wired): record and succeed.
	if dep.Sender == nil {
		p.writeLog(ctx, logBase, models.EmailLogSkippedDisabled, "", "")
		slog.InfoContext(ctx, "send_email skipped: sending disabled", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey)
		return nil
	}
	if dep.Suppressions != nil {
		suppressed, err := dep.Suppressions.IsSuppressed(ctx, rcpt.email, t.EmailKind)
		if err != nil {
			return err // transient (DB)
		}
		if suppressed {
			p.writeLog(ctx, logBase, models.EmailLogSkippedSuppressed, "", "")
			slog.InfoContext(ctx, "send_email skipped: suppressed", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey, "kind", string(t.EmailKind))
			return nil
		}
	}

	tmpl, err := dep.Templates.GetByKey(ctx, t.TemplateKey)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeLog(ctx, logBase, models.EmailLogFailed, "", "template not found: "+t.TemplateKey)
		slog.WarnContext(ctx, "send_email failed: template not found", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey)
		return nil // terminal
	}
	if err != nil {
		return err // transient (DB)
	}

	headers, unsubURL, ok, err := p.unsubscribeHeaders(ctx, t, rcpt.email)
	if err != nil {
		return err // transient (secret read)
	}
	if !ok {
		p.writeLog(ctx, logBase, models.EmailLogFailed, "", "no link secret for unsubscribe")
		slog.WarnContext(ctx, "send_email failed: no unsubscribe secret", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey)
		return nil // terminal
	}

	rendered, err := templates.Render(tmpl, templateData(rcpt, dep.AppBaseURL, unsubURL, t.Vars))
	if err != nil {
		p.writeLog(ctx, logBase, models.EmailLogFailed, "", "render: "+err.Error())
		slog.WarnContext(ctx, "send_email failed: render", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey, logging.AttrError, err)
		return nil // terminal
	}

	msgID, err := dep.Sender.Send(ctx, email.Message{
		From:           dep.From,
		ReplyTo:        dep.ReplyTo,
		To:             rcpt.email,
		Subject:        rendered.Subject,
		HTML:           rendered.HTML,
		Text:           rendered.Text,
		Headers:        headers,
		IdempotencyKey: t.IdempotencyKey,
	})
	if err != nil {
		return p.handleSendErr(ctx, t, logBase, rendered, err)
	}

	// The rendered body is kept beside the sent log so the operator Emails tab
	// renders it even after the Resend message ages out of retention.
	id := p.writeLog(ctx, logBase, models.EmailLogSent, msgID, "")
	p.writeBody(ctx, id, rendered, dep.From, dep.ReplyTo)
	slog.InfoContext(ctx, "send_email sent", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey, "kind", string(t.EmailKind))
	return nil
}

const sendEmailComponent = "jobs.send_email"

// emailRecipient is the resolved addressee plus the workspace name the
// templates greet them with.
type emailRecipient struct {
	name      string
	email     string
	workspace string
}

// resolveRecipient re-resolves a user recipient fresh from users, so an email
// change since enqueue is honoured and a deleted user is a clean terminal skip.
// A recipient who isn't a user yet (an invitee) is addressed from the task,
// with the workspace name riding the vars. ok is false when the caller must
// return err: nil for a terminal skip, non-nil for a transient DB error.
func (p *SendEmailProcessor) resolveRecipient(ctx context.Context, t SendEmailTask) (rcpt emailRecipient, ok bool, err error) {
	if t.UserID == "" {
		return emailRecipient{name: t.ToName, email: t.ToEmail, workspace: t.Vars["workspace_name"]}, true, nil
	}
	if p.Deps.Users == nil {
		slog.WarnContext(ctx, "send_email skipped: deps not wired", logging.AttrComponent, sendEmailComponent)
		return rcpt, false, nil
	}
	user, err := p.Deps.Users.GetByIDWithTenant(ctx, t.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		slog.InfoContext(ctx, "send_email skipped: user gone", logging.AttrComponent, sendEmailComponent, "user", t.UserID)
		return rcpt, false, nil
	}
	if err != nil {
		return rcpt, false, err // transient (DB)
	}
	rcpt = emailRecipient{name: user.Name, email: user.Email}
	if user.Tenant != nil {
		rcpt.workspace = user.Tenant.Name
	}
	return rcpt, true, nil
}

// unsubscribeHeaders builds the one-click unsubscribe headers marketing mail
// must carry, returning the unsubscribe URL for the template. Other kinds get
// no headers. ok is false when marketing mail has no link secret to sign the
// URL with; err is a transient secret-read failure.
func (p *SendEmailProcessor) unsubscribeHeaders(ctx context.Context, t SendEmailTask, toEmail string) (headers map[string]string, unsubURL string, ok bool, err error) {
	headers = map[string]string{}
	if t.EmailKind != models.EmailKindMarketing {
		return headers, "", true, nil
	}
	secret := ""
	if p.Deps.LinkSecret != nil {
		if secret, err = p.Deps.LinkSecret(ctx); err != nil {
			return nil, "", false, err
		}
	}
	if secret == "" {
		return nil, "", false, nil
	}
	unsubURL = p.Deps.unsubscribeURL(secret, toEmail)
	headers["List-Unsubscribe"] = "<" + unsubURL + ">"
	headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	return headers, unsubURL, true, nil
}

// templateData maps the recipient and the task's per-message vars onto the
// template data. Vars carry what isn't derivable at send time (reset and
// invite links, connection-expiry details, the operator's new-tenant
// details); templates that don't use a var leave it empty.
func templateData(rcpt emailRecipient, appURL, unsubURL string, vars map[string]string) templates.Data {
	return templates.Data{
		Name:           rcpt.name,
		WorkspaceName:  rcpt.workspace,
		AppURL:         appURL,
		UnsubscribeURL: unsubURL,
		ResetURL:       vars["reset_url"],
		InviteURL:      vars["invite_url"],
		InviterName:    vars["inviter_name"],
		Role:           vars["role"],
		Platform:       vars["platform"],
		AccountName:    vars["account_name"],
		Stage:          vars["stage"],
		ExpiresAt:      vars["expires_at"],
		ExpiresIn:      vars["expires_in"],
		ReconnectURL:   vars["reconnect_url"],
		TenantID:       vars["tenant_id"],
		TenantSlug:     vars["tenant_slug"],
		OwnerName:      vars["owner_name"],
		OwnerEmail:     vars["owner_email"],
		Tier:           vars["tier"],
		Status:         vars["status"],
		RegisteredAt:   vars["registered_at"],
		TenantURL:      vars["tenant_url"],
	}
}

// handleSendErr settles a failed send. A key cleared after enqueue records a
// disabled skip, a transient error retries, and any other failure persists
// what was attempted so an operator can see the body behind it.
func (p *SendEmailProcessor) handleSendErr(ctx context.Context, t SendEmailTask, logBase models.EmailLog, rendered templates.Rendered, err error) error {
	if email.IsDisabled(err) {
		p.writeLog(ctx, logBase, models.EmailLogSkippedDisabled, "", "")
		slog.InfoContext(ctx, "send_email skipped: sending disabled", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey)
		return nil
	}
	if email.IsTransient(err) {
		slog.WarnContext(ctx, "send_email transient failure; will retry", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey, logging.AttrError, err)
		return err
	}
	id := p.writeLog(ctx, logBase, models.EmailLogFailed, "", err.Error())
	p.writeBody(ctx, id, rendered, p.Deps.From, p.Deps.ReplyTo)
	slog.WarnContext(ctx, "send_email terminal failure", logging.AttrComponent, sendEmailComponent, "template", t.TemplateKey, logging.AttrError, err)
	return nil // terminal
}

// writeLog appends one email_logs row (best-effort). The mail decision is
// already made by the time this runs, so a log failure — including a unique
// violation from a prior attempt that already logged this idempotency key — is
// warned and swallowed rather than failing the job (which would re-send).
//
// Because every terminal send outcome (sent / failed / skipped) funnels through
// here — transient failures return earlier and are retried — this is also the
// single point where the outcome is mirrored into the tenant activity log.
//
// Returns the inserted row id, or "" when no row was written (logs repo unwired,
// id-gen failure, or insert error). The id lets the caller persist the rendered
// body against it; an empty id must NOT be used for that, since the
// email_bodies FK references a committed email_logs row.
func (p *SendEmailProcessor) writeLog(ctx context.Context, base models.EmailLog, status models.EmailLogStatus, providerMsgID, errMsg string) string {
	// Mirror the outcome into tenant_activity_events. The Recorder is
	// nil-safe and resolves the tenant from ctx (set in Process), so this never
	// blocks the send and is a no-op when analytics is disabled. Recorded
	// independently of the email_logs write below so activity is captured even
	// if the logs repo is unwired.
	p.recordActivity(ctx, base, status, providerMsgID, errMsg)

	if p.Deps.Logs == nil {
		return ""
	}
	id, err := models.NewID()
	if err != nil {
		slog.WarnContext(ctx, "email_log id gen failed", logging.AttrComponent, sendEmailComponent, logging.AttrError, err)
		return ""
	}
	row := base
	row.ID = id
	row.Status = status
	row.ProviderMessageID = providerMsgID
	row.Error = errMsg
	if err := p.Deps.Logs.Insert(ctx, &row); err != nil {
		slog.WarnContext(ctx, "email_log insert failed (best-effort)", logging.AttrComponent, sendEmailComponent, logging.AttrError, err)
		return "" // no committed parent row → don't attempt the body write
	}
	return id
}

// writeBody best-effort persists the rendered body against a just-written
// email_logs row so the operator Emails tab renders it even after the
// Resend message ages out of retention or the key is unset. It is a
// no-op when the log row wasn't written (empty id — the FK would fail) or the
// body store isn't wired. A body-store failure is warned and swallowed: a
// missing body must never fail or re-send the job. Call it only with an id
// returned by writeLog (i.e. after the parent row committed).
func (p *SendEmailProcessor) writeBody(ctx context.Context, emailLogID string, r templates.Rendered, from, replyTo string) {
	if emailLogID == "" || p.Deps.Bodies == nil {
		return
	}
	if err := p.Deps.Bodies.Insert(ctx, &models.EmailBody{
		EmailLogID: emailLogID,
		Subject:    r.Subject,
		HTML:       r.HTML,
		Text:       r.Text,
		From:       from,
		ReplyTo:    replyTo,
	}); err != nil {
		slog.WarnContext(ctx, "email_body insert failed (best-effort)", logging.AttrComponent, sendEmailComponent, logging.AttrError, err)
	}
}

// recordActivity emits one email-category activity event per terminal send
// outcome so the analytics DB carries a tenant-scoped, append-only trail of mail
// activity alongside the operational email_logs row. The type mirrors
// the email_logs status; the recipient address is deliberately omitted — the
// event is keyed by user + tenant, keeping raw PII out of analytics. The full
// error (if any) stays in email_logs; a capped copy rides the payload so failure
// reasons are queryable without a join.
func (p *SendEmailProcessor) recordActivity(ctx context.Context, base models.EmailLog, status models.EmailLogStatus, providerMsgID, errMsg string) {
	payload := map[string]any{
		"template": base.TemplateID,
		"kind":     string(base.Kind),
	}
	if providerMsgID != "" {
		payload["provider_message_id"] = providerMsgID
	}
	if errMsg != "" {
		payload["error"] = capString(errMsg, 256)
	}
	p.Deps.ActivityRecorder.Record(ctx, activity.CategoryEmail, emailActivityType(status),
		activity.WithUser(base.UserID),
		activity.WithEntity("email", base.TemplateID),
		activity.WithSource(activity.SourceJob),
		activity.WithStatus(string(status)),
		activity.WithPayload(payload),
	)
}

// emailActivityType maps an email_logs status to the activity type recorded in
// tenant_activity_events. The two skip outcomes share one type (email_skipped);
// the event's status field carries the specific reason (suppressed / disabled).
func emailActivityType(status models.EmailLogStatus) string {
	switch status {
	case models.EmailLogSent:
		return "email_sent"
	case models.EmailLogFailed:
		return "email_send_failed"
	case models.EmailLogSkippedSuppressed, models.EmailLogSkippedDisabled:
		return "email_skipped"
	default:
		// Defensive: the job only ever logs the statuses above, but keep a
		// stable, prefixed type rather than an empty one if that changes.
		return "email_" + string(status)
	}
}

// capString bounds a string to n bytes on a rune boundary so a long provider
// error can't bloat the activity payload. Analytics-grade truncation — the full
// value is preserved in email_logs.
func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
