package queues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// DetectExpiringConnectionsQueue is the recurring connection-health sweep.
// Each tick reads every connected account's Zernio health, persists
// the snapshot, and emails workspace owners once per unhealthy episode and stage
// when Zernio flags the connection as expiring or needing a reconnect. It mirrors the
// follower-refresh sweep: a marker payload, self-registering, gated on the
// Zernio integration being configured, harmless no-op when it isn't.
const DetectExpiringConnectionsQueue = "detect_expiring_connections"

// defaultExpiryLeadDays is the heads-up window used when no lead is configured.
const defaultExpiryLeadDays = 7

const detectComp = "jobs.detect_expiring_connections"

// DetectExpiringConnectionsTask is a marker payload — this queue carries no
// per-tick data.
type DetectExpiringConnectionsTask struct{}

// Kind implements river.JobArgs.
func (DetectExpiringConnectionsTask) Kind() string { return DetectExpiringConnectionsQueue }

// InsertOpts mirrors the other periodic sweeps: one attempt (each tick records
// its own outcome and the persist/enqueue are idempotent), active-state
// uniqueness so overlapping ticks don't stack.
func (DetectExpiringConnectionsTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// DetectExpiringConnectionsProcessor wires one health-sweep tick. Zernio
// supplies the client + social-account repo (enumeration/persistence), Users
// resolves the owner recipients, EmailLogs backs the durable notify-once dedupe,
// and AppBaseURL builds the reconnect deep link. The email itself is enqueued as
// a send_email job via the River client on the worker context.
type DetectExpiringConnectionsProcessor struct {
	river.WorkerDefaults[DetectExpiringConnectionsTask]
	Zernio     ZernioDeps
	Users      repository.UserRepository
	EmailLogs  repository.EmailLogRepository
	AppBaseURL string
	LeadDays   int
	// Notifier drops an in-app notification alongside the email. Nil
	// (notification center unwired) is a no-op.
	Notifier *notify.Service
}

func (p *DetectExpiringConnectionsProcessor) Work(ctx context.Context, job *river.Job[DetectExpiringConnectionsTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Background job spans tenants; sweepTenant re-scopes per tenant.
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

func (p *DetectExpiringConnectionsProcessor) Timeout(*river.Job[DetectExpiringConnectionsTask]) time.Duration {
	return 60 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &DetectExpiringConnectionsProcessor{
			Zernio:     d.Zernio,
			Users:      d.Users,
			EmailLogs:  d.Email.Logs,
			AppBaseURL: d.Email.AppBaseURL,
			LeadDays:   d.ExpiryLeadDays,
			Notifier:   d.Notifier,
		})
	})
}

// Process runs one sweep tick. Per-tenant failures are recorded in the tick
// metric and log line but never returned to River — a single attempt records
// its own outcome and reschedules on the next interval.
func (p *DetectExpiringConnectionsProcessor) Process(ctx context.Context, _ DetectExpiringConnectionsTask) error {
	tickStart := time.Now()
	res, err := p.sweep(ctx, tickStart.UTC())
	jobs.ZernioHealthAccountsChecked.Add(int64(res.checked))
	jobs.ZernioConnectionExpiringDetected.Add(int64(res.expiringSoon))
	jobs.ZernioConnectionActionRequiredDetected.Add(int64(res.actionRequired))
	jobs.ZernioConnectionExpiryNotified.Add(int64(res.notified))
	if err != nil {
		jobs.ZernioHealthSweepFailed.Add(1)
		slog.ErrorContext(ctx, "connection-health sweep failed", logging.AttrComponent, detectComp,
			"checked", res.checked, "notified", res.notified, "tick_ms", time.Since(tickStart).Milliseconds(), logging.AttrError, err)
	} else {
		jobs.ZernioHealthSweepSucceeded.Add(1)
		slog.InfoContext(ctx, "connection-health sweep ok", logging.AttrComponent, detectComp,
			"checked", res.checked, "expiring_soon", res.expiringSoon, "action_required", res.actionRequired,
			"notified", res.notified, "tick_ms", time.Since(tickStart).Milliseconds())
	}
	return nil
}

// sweepResult accumulates a tick's counters across tenants.
type sweepResult struct {
	checked        int
	expiringSoon   int
	actionRequired int
	notified       int
}

func (r *sweepResult) add(o sweepResult) {
	r.checked += o.checked
	r.expiringSoon += o.expiringSoon
	r.actionRequired += o.actionRequired
	r.notified += o.notified
}

// sweep enumerates every tenant with active connected accounts and sweeps each
// under its own tenant scope. The returned error is the first tenant-level
// failure — used only for the tick metric, never propagated to River.
func (p *DetectExpiringConnectionsProcessor) sweep(ctx context.Context, now time.Time) (sweepResult, error) {
	var res sweepResult
	if p.Zernio.Client == nil || p.Zernio.SocialAccountRepo == nil || p.Users == nil {
		return res, errors.New("detect expiring connections: dependencies not configured")
	}
	pairs, err := p.Zernio.SocialAccountRepo.ListActiveTenantProfiles(tenantctx.WithSystem(ctx))
	if err != nil {
		return res, fmt.Errorf("detect expiring connections: list tenant profiles: %w", err)
	}

	var firstErr error
	for _, tp := range pairs {
		tctx := tenantctx.With(ctx, tp.TenantID)
		tres, terr := p.sweepTenant(tctx, tp.TenantID, tp.ProfileID, now)
		res.add(tres)
		if terr != nil {
			if firstErr == nil {
				firstErr = terr
			}
			slog.ErrorContext(tctx, "connection-health sweep: tenant failed", logging.AttrComponent, detectComp,
				"tenant_id", tp.TenantID, logging.AttrError, terr)
		}
	}
	return res, firstErr
}

// sweepTenant reads one tenant's account health, persists each snapshot, and
// notifies owners of accounts entering a notify stage. ctx is already
// tenant-scoped, so the health fetch is filtered to this tenant's profile and
// every write lands in the right tenant.
func (p *DetectExpiringConnectionsProcessor) sweepTenant(ctx context.Context, tenantID, profileID string, now time.Time) (sweepResult, error) {
	var res sweepResult
	if profileID == "" {
		return res, nil
	}

	apiStart := time.Now()
	healths, err := p.Zernio.Client.GetAccountsHealth(ctx, profileID)
	jobs.ObserveZernioCall(time.Since(apiStart))
	if err != nil {
		return res, err
	}
	if len(healths) == 0 {
		return res, nil
	}

	// The local active mirror both filters the health list (skip accounts we
	// don't track / are soft-deleted) and provides a display-name fallback.
	active, err := p.Zernio.SocialAccountRepo.ListActive(ctx, profileID)
	if err != nil {
		return res, err
	}
	known := make(map[string]models.SocialAccount, len(active))
	for _, a := range active {
		known[a.ID] = a
	}

	// Owners are the recipient set; resolve once per tenant. A lookup failure is
	// recorded but doesn't abort persistence (health is still worth saving).
	owners, ownersErr := p.Users.ListOwnersByTenant(ctx, tenantID)

	var firstErr error
	for _, h := range healths {
		local, ok := known[h.AccountID]
		if !ok {
			continue // not a tracked/active account
		}
		res.checked++

		stage := classifyHealth(h, now, p.leadDays())
		since := episodeStart(local.UnhealthySince, stage, now)
		// The episode start must be stored before notifying: it is part of the
		// dedupe key, so an unsaved one would re-notify on the next sweep.
		if perr := p.persistHealth(ctx, h, since, now); perr != nil {
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}

		switch stage {
		case templates.StageExpiringSoon:
			res.expiringSoon++
		case templates.StageActionRequired:
			res.actionRequired++
		default:
			continue // healthy
		}

		if ownersErr != nil {
			if firstErr == nil {
				firstErr = ownersErr
			}
			continue // can't notify without recipients
		}
		notified, nerr := p.notifyOwners(ctx, tenantID, owners, h, local, stage, *since, now)
		res.notified += notified
		if nerr != nil && firstErr == nil {
			firstErr = nerr
		}
	}
	return res, firstErr
}

// episodeStart returns when the account's current unhealthy episode began: the
// stored start while it stays unhealthy, now when it just turned unhealthy, and
// nil once it is healthy again (closing the episode).
func episodeStart(prev *time.Time, stage string, now time.Time) *time.Time {
	if stage == "" {
		return nil
	}
	if prev != nil {
		return prev
	}
	return &now
}

// persistHealth writes the health snapshot and episode start onto the local
// account row.
func (p *DetectExpiringConnectionsProcessor) persistHealth(ctx context.Context, h zernio.AccountHealth, unhealthySince *time.Time, now time.Time) error {
	tokenValid := h.TokenValid
	needsReconnect := h.NeedsReconnect
	return p.Zernio.SocialAccountRepo.UpdateHealth(ctx, h.AccountID, repository.SocialAccountHealth{
		TokenExpiresAt:      h.TokenExpiresAt,
		TokenValid:          &tokenValid,
		HealthStatus:        h.Status,
		NeedsReconnect:      &needsReconnect,
		LastHealthCheckedAt: now,
		UnhealthySince:      unhealthySince,
	})
}

// notifyOwners enqueues one connection_expiring email per owner, skipping any
// (account, stage, episode, owner) already notified. Returns the count enqueued.
func (p *DetectExpiringConnectionsProcessor) notifyOwners(ctx context.Context, tenantID string, owners []models.User, h zernio.AccountHealth, local models.SocialAccount, stage string, since, now time.Time) (int, error) {
	if len(owners) == 0 {
		slog.WarnContext(ctx, "connection-health: no owners to notify", logging.AttrComponent, detectComp, "tenant_id", tenantID, "account_id", h.AccountID)
		return 0, nil
	}
	notified := 0
	var firstErr error
	for _, owner := range owners {
		key := expiryIdempotencyKey(h.AccountID, stage, since, owner.ID)
		if p.EmailLogs != nil {
			exists, cerr := p.EmailLogs.ExistsByIdempotencyKey(ctx, key)
			if cerr != nil {
				if firstErr == nil {
					firstErr = cerr
				}
				continue
			}
			if exists {
				continue // already notified this owner for this (account, stage, episode)
			}
		}
		// Drop an in-app notification alongside the email, gated by the
		// same once-per-(account,stage,episode,owner) email dedupe above. Best-
		// effort — a notify failure never blocks the email or the sweep.
		p.emitNotification(ctx, owner.ID, h, local, stage)
		if eerr := p.enqueueEmail(ctx, owner, tenantID, h, local, stage, key, now); eerr != nil {
			if firstErr == nil {
				firstErr = eerr
			}
			continue
		}
		notified++
	}
	return notified, firstErr
}

// emitNotification drops one in-app notification for an owner whose account is
// entering a notify stage. It rides the same tenant-scoped ctx as the
// email enqueue, so the row lands in the right tenant; a nil Notifier is a
// no-op. The dedupe_key collapses repeats while the prior one is still unread,
// backing up the email-log dedupe the caller already applied.
func (p *DetectExpiringConnectionsProcessor) emitNotification(ctx context.Context, ownerID string, h zernio.AccountHealth, local models.SocialAccount, stage string) {
	level := models.NotificationLevelWarning
	title := "Reconnect needed soon"
	if stage == templates.StageActionRequired {
		level = models.NotificationLevelError
		title = "Reconnect required"
	}
	name := accountLabel(h, local)
	_ = p.Notifier.Emit(ctx, ownerID, notify.Spec{
		Level:      level,
		Type:       "connection." + stage,
		Title:      title,
		Body:       fmt.Sprintf("Your %s connection for %s needs attention.", platformLabel(h.Platform), name),
		EntityType: "social_account",
		EntityID:   h.AccountID,
		ActionURL:  p.reconnectURL(h.AccountID),
		DedupeKey:  "conn:" + h.AccountID + ":" + stage,
		Data: map[string]any{
			"platform": h.Platform,
			"stage":    stage,
		},
	})
}

// enqueueEmail inserts a send_email job for one owner. The River client is
// pulled from the worker context (the client is built after the workers are
// registered, so it can't ride the processor struct), mirroring the submit
// worker's poll enqueue.
func (p *DetectExpiringConnectionsProcessor) enqueueEmail(ctx context.Context, owner models.User, tenantID string, h zernio.AccountHealth, local models.SocialAccount, stage, idemKey string, now time.Time) error {
	client, err := river.ClientFromContextSafely[*sql.Tx](ctx)
	if err != nil || client == nil {
		return fmt.Errorf("river client unavailable for send_email enqueue: %w", err)
	}
	vars := map[string]string{
		"platform":      platformLabel(h.Platform),
		"account_name":  accountLabel(h, local),
		"stage":         stage,
		"reconnect_url": p.reconnectURL(h.AccountID),
	}
	if h.TokenExpiresAt != nil {
		vars["expires_at"] = h.TokenExpiresAt.UTC().Format("January 2, 2006")
		vars["expires_in"] = humanizeUntil(*h.TokenExpiresAt, now)
	}
	_, err = client.Insert(ctx, SendEmailTask{
		UserID:         owner.ID,
		TenantID:       tenantID,
		TemplateKey:    templates.KeyConnectionExpiring,
		EmailKind:      models.EmailKindTransactional,
		IdempotencyKey: idemKey,
		Vars:           vars,
	}, insertOptsWithRequestID(ctx, nil))
	return err
}

func (p *DetectExpiringConnectionsProcessor) reconnectURL(accountID string) string {
	base := strings.TrimRight(p.AppBaseURL, "/")
	return base + "/workspace-settings?reconnect=" + url.QueryEscape(accountID)
}

func (p *DetectExpiringConnectionsProcessor) leadDays() int {
	if p.LeadDays > 0 {
		return p.LeadDays
	}
	return defaultExpiryLeadDays
}

// classifyHealth maps a Zernio health entry to a notify stage, or "" for
// healthy. Zernio's verdict is authoritative: for platforms whose short-lived
// access token Zernio refreshes itself (YouTube, X, …) tokenExpiresAt is always
// hours away while the connection is perfectly healthy, so the token date only
// decides the stage when Zernio sends no recognised status. action_required wins
// over expiring_soon. leadDays is the heads-up window for that fallback.
func classifyHealth(h zernio.AccountHealth, now time.Time, leadDays int) string {
	if h.Status == "error" || h.NeedsReconnect {
		return templates.StageActionRequired
	}
	switch h.Status {
	case "warning":
		return templates.StageExpiringSoon
	case "healthy":
		return ""
	}
	if h.TokenExpiresAt == nil {
		return ""
	}
	if !h.TokenExpiresAt.After(now) {
		return templates.StageActionRequired
	}
	if h.TokenExpiresAt.Sub(now) <= time.Duration(leadDays)*24*time.Hour {
		return templates.StageExpiringSoon
	}
	return ""
}

// expiryIdempotencyKey keys notify-once on (account, stage, episode start,
// owner): re-sweeping an ongoing episode never re-sends, even as the reported
// token expiry moves, while a new episode after recovery yields a fresh key.
func expiryIdempotencyKey(accountID, stage string, since time.Time, ownerID string) string {
	return "conn_expiring:" + accountID + ":" + stage + ":" + since.UTC().Format(time.RFC3339) + ":" + ownerID
}

// accountLabel is the human name for an account, preferring the live health
// payload and falling back to the local mirror, then the platform.
func accountLabel(h zernio.AccountHealth, local models.SocialAccount) string {
	for _, s := range []string{h.DisplayName, h.Username, local.DisplayName, local.Username} {
		if s != "" {
			return s
		}
	}
	return "your account"
}

// platformLabels maps Zernio platform slugs to display names; slugs not listed
// fall back to a capitalized form.
var platformLabels = map[string]string{
	"facebook":       "Facebook",
	"instagram":      "Instagram",
	"linkedin":       "LinkedIn",
	"twitter":        "X (Twitter)",
	"tiktok":         "TikTok",
	"youtube":        "YouTube",
	"threads":        "Threads",
	"pinterest":      "Pinterest",
	"reddit":         "Reddit",
	"bluesky":        "Bluesky",
	"googlebusiness": "Google Business",
	"telegram":       "Telegram",
	"snapchat":       "Snapchat",
	"discord":        "Discord",
	"slack":          "Slack",
	"whatsapp":       "WhatsApp",
}

func platformLabel(slug string) string {
	if s, ok := platformLabels[slug]; ok {
		return s
	}
	if slug == "" {
		return "social"
	}
	return strings.ToUpper(slug[:1]) + slug[1:]
}

// humanizeUntil renders a coarse "in N days" relative to now, floored at days.
func humanizeUntil(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "today"
	}
	days := int(d.Hours() / 24)
	switch {
	case days <= 0:
		return "in less than a day"
	case days == 1:
		return "in 1 day"
	default:
		return fmt.Sprintf("in %d days", days)
	}
}
