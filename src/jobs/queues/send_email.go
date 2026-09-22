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

// SendEmailQueue is the single async send path for all mail (CON-154). Welcome
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
	// invitee (CON-26), who has no users row until they accept. Used only when
	// UserID is empty: the worker then sends to ToEmail directly instead of
	// re-resolving the address from users. Exactly one of UserID / ToEmail is set.
	ToEmail string `json:"to_email,omitempty"`
	ToName  string `json:"to_name,omitempty"`
	// Vars carries per-message template variables that aren't derivable from the
	// user/tenant/config at send time — e.g. the one-time password-reset URL
	// (CON-161) or the invitation accept link + inviter + role (CON-26). Empty for
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
	// Tenants skips mail for a suspended/deleted tenant (CON-190) — e.g. a drip
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
	const comp = "jobs.send_email"
	dep := p.Deps
	if t.TemplateKey == "" || (t.UserID == "" && t.ToEmail == "") {
		slog.WarnContext(ctx, "send_email skipped: missing args", logging.AttrComponent, comp)
		return nil
	}
	if dep.Templates == nil {
		slog.WarnContext(ctx, "send_email skipped: deps not wired", logging.AttrComponent, comp)
		return nil
	}
	ctx = tenantctx.With(ctx, t.TenantID)

	// Skip mail for a suspended/deleted tenant (CON-190): a frozen tenant sends no
	// welcome/drip/transactional mail. A DB error retries; otherwise terminal.
	if active, aerr := tenantIsActive(ctx, p.Tenants, t.TenantID); aerr != nil {
		return aerr
	} else if !active {
		slog.InfoContext(ctx, "send_email skipped: tenant not active", logging.AttrComponent, comp, "template", t.TemplateKey)
		return nil
	}

	// Resolve the recipient. The usual path re-resolves it fresh from users (so an
	// email change since enqueue is honoured and a deleted user is a clean terminal
	// skip). When the mail targets someone who isn't a user yet — an invitee, CON-26
	// — the address is carried on the task itself; the workspace name then rides the
	// vars, since there's no user/tenant to load.
	var recipientName, toEmail, workspace string
	if t.UserID != "" {
		if dep.Users == nil {
			slog.WarnContext(ctx, "send_email skipped: deps not wired", logging.AttrComponent, comp)
			return nil
		}
		user, err := dep.Users.GetByIDWithTenant(ctx, t.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			slog.InfoContext(ctx, "send_email skipped: user gone", logging.AttrComponent, comp, "user", t.UserID)
			return nil
		} else if err != nil {
			return err // transient (DB)
		}
		recipientName, toEmail = user.Name, user.Email
		if user.Tenant != nil {
			workspace = user.Tenant.Name
		}
	} else {
		recipientName, toEmail = t.ToName, t.ToEmail
		workspace = t.Vars["workspace_name"]
	}

	logBase := models.EmailLog{
		TenantID:       t.TenantID,
		UserID:         t.UserID,
		TemplateID:     t.TemplateKey,
		Kind:           t.EmailKind,
		ToEmail:        toEmail,
		Provider:       models.ProviderResend,
		IdempotencyKey: t.IdempotencyKey,
	}

	// Sending disabled (no Resend key wired): record and succeed.
	if dep.Sender == nil {
		p.writeLog(ctx, logBase, models.EmailLogSkippedDisabled, "", "")
		slog.InfoContext(ctx, "send_email skipped: sending disabled", logging.AttrComponent, comp, "template", t.TemplateKey)
		return nil
	}

	// Send-time suppression gate (CON-154 D2).
	if dep.Suppressions != nil {
		suppressed, err := dep.Suppressions.IsSuppressed(ctx, toEmail, t.EmailKind)
		if err != nil {
			return err // transient (DB)
		}
		if suppressed {
			p.writeLog(ctx, logBase, models.EmailLogSkippedSuppressed, "", "")
			slog.InfoContext(ctx, "send_email skipped: suppressed", logging.AttrComponent, comp, "template", t.TemplateKey, "kind", string(t.EmailKind))
			return nil
		}
	}

	tmpl, err := dep.Templates.GetByKey(ctx, t.TemplateKey)
	if errors.Is(err, sql.ErrNoRows) {
		p.writeLog(ctx, logBase, models.EmailLogFailed, "", "template not found: "+t.TemplateKey)
		slog.WarnContext(ctx, "send_email failed: template not found", logging.AttrComponent, comp, "template", t.TemplateKey)
		return nil // terminal
	} else if err != nil {
		return err // transient (DB)
	}

	// Marketing mail must carry an unsubscribe affordance.
	headers := map[string]string{}
	unsubURL := ""
	if t.EmailKind == models.EmailKindMarketing {
		secret := ""
		if dep.LinkSecret != nil {
			secret, err = dep.LinkSecret(ctx)
			if err != nil {
				return err // transient (secret read)
			}
		}
		if secret == "" {
			p.writeLog(ctx, logBase, models.EmailLogFailed, "", "no link secret for unsubscribe")
			slog.WarnContext(ctx, "send_email failed: no unsubscribe secret", logging.AttrComponent, comp, "template", t.TemplateKey)
			return nil // terminal
		}
		unsubURL = dep.unsubscribeURL(secret, toEmail)
		headers["List-Unsubscribe"] = "<" + unsubURL + ">"
		headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	}

	rendered, err := templates.Render(tmpl, templates.Data{
		Name:           recipientName,
		WorkspaceName:  workspace,
		AppURL:         dep.AppBaseURL,
		UnsubscribeURL: unsubURL,
		// Per-message vars: the password_reset template reads ResetURL (CON-161);
		// the invitation template reads InviteURL/InviterName/Role (CON-26); the
		// connection_expiring template reads Platform/AccountName/Stage/Expires*/
		// ReconnectURL (CON-219); other templates leave them empty.
		ResetURL:     t.Vars["reset_url"],
		InviteURL:    t.Vars["invite_url"],
		InviterName:  t.Vars["inviter_name"],
		Role:         t.Vars["role"],
		Platform:     t.Vars["platform"],
		AccountName:  t.Vars["account_name"],
		Stage:        t.Vars["stage"],
		ExpiresAt:    t.Vars["expires_at"],
		ExpiresIn:    t.Vars["expires_in"],
		ReconnectURL: t.Vars["reconnect_url"],
	})
	if err != nil {
		p.writeLog(ctx, logBase, models.EmailLogFailed, "", "render: "+err.Error())
		slog.WarnContext(ctx, "send_email failed: render", logging.AttrComponent, comp, "template", t.TemplateKey, logging.AttrError, err)
		return nil // terminal
	}

	msgID, err := dep.Sender.Send(ctx, email.Message{
		From:           dep.From,
		ReplyTo:        dep.ReplyTo,
		To:             toEmail,
		Subject:        rendered.Subject,
		HTML:           rendered.HTML,
		Text:           rendered.Text,
		Headers:        headers,
		IdempotencyKey: t.IdempotencyKey,
	})
	if err != nil {
		if email.IsDisabled(err) {
			// No Resend key configured at send time (e.g. cleared after enqueue):
			// record as disabled, not failed — the subsystem is intentionally off.
			p.writeLog(ctx, logBase, models.EmailLogSkippedDisabled, "", "")
			slog.InfoContext(ctx, "send_email skipped: sending disabled", logging.AttrComponent, comp, "template", t.TemplateKey)
			return nil
		}
		if email.IsTransient(err) {
			slog.WarnContext(ctx, "send_email transient failure; will retry", logging.AttrComponent, comp, "template", t.TemplateKey, logging.AttrError, err)
			return err
		}
		// Post-render terminal failure: persist what we attempted to send (CON-306)
		// so an operator can see the rendered body behind a failed delivery.
		id := p.writeLog(ctx, logBase, models.EmailLogFailed, "", err.Error())
		p.writeBody(ctx, id, rendered, dep.From, dep.ReplyTo)
		slog.WarnContext(ctx, "send_email terminal failure", logging.AttrComponent, comp, "template", t.TemplateKey, logging.AttrError, err)
		return nil // terminal
	}

	// Persist the rendered body alongside the sent log (CON-306) so the operator
	// Emails tab renders it even after the Resend message ages out of retention.
	id := p.writeLog(ctx, logBase, models.EmailLogSent, msgID, "")
	p.writeBody(ctx, id, rendered, dep.From, dep.ReplyTo)
	slog.InfoContext(ctx, "send_email sent", logging.AttrComponent, comp, "template", t.TemplateKey, "kind", string(t.EmailKind))
	return nil
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
// body against it (CON-306); an empty id must NOT be used for that, since the
// email_bodies FK references a committed email_logs row.
func (p *SendEmailProcessor) writeLog(ctx context.Context, base models.EmailLog, status models.EmailLogStatus, providerMsgID, errMsg string) string {
	// Mirror the outcome into tenant_activity_events (CON-125). The Recorder is
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
		slog.WarnContext(ctx, "email_log id gen failed", logging.AttrComponent, "jobs.send_email", logging.AttrError, err)
		return ""
	}
	row := base
	row.ID = id
	row.Status = status
	row.ProviderMessageID = providerMsgID
	row.Error = errMsg
	if err := p.Deps.Logs.Insert(ctx, &row); err != nil {
		slog.WarnContext(ctx, "email_log insert failed (best-effort)", logging.AttrComponent, "jobs.send_email", logging.AttrError, err)
		return "" // no committed parent row → don't attempt the body write
	}
	return id
}

// writeBody best-effort persists the rendered body against a just-written
// email_logs row so the operator Emails tab (CON-192) renders it even after the
// Resend message ages out of retention or the key is unset (CON-306). It is a
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
		slog.WarnContext(ctx, "email_body insert failed (best-effort)", logging.AttrComponent, "jobs.send_email", logging.AttrError, err)
	}
}

// recordActivity emits one email-category activity event per terminal send
// outcome so the analytics DB carries a tenant-scoped, append-only trail of mail
// activity alongside the operational email_logs row (CON-125). The type mirrors
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
