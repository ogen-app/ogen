package queues

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/kernel/logging"
)

// NotifyHarborQueue delivers the CON-229 "a new tenant registered" event to
// Harbor. On a committed signup the signup tx enqueues one of these; the worker
// POSTs a signed JSON body to Harbor's inbound webhook, which then resolves the
// operator recipients and calls back EmailAdminService.NotifyOperatorsTenant-
// Registered. Kept out of the signup request path (durable + retryable) so
// signup never blocks on Harbor reachability.
const NotifyHarborQueue = "notify_harbor_tenant_registered"

// harborHTTPClient bounds the outbound webhook call. The per-attempt River
// Timeout also caps it via ctx; this is the belt-and-suspenders transport
// deadline shared across attempts. Redirects are NOT followed: Go would rewrite
// a 3xx POST into a bodyless GET and a final 2xx would then falsely record
// delivery — a redirect here means a misconfigured webhook URL, so the 3xx is
// returned as-is and handled as a terminal non-2xx status.
var harborHTTPClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// NotifyHarborTenantRegisteredTask carries only the tenant id; Harbor re-derives
// everything else by calling back the send RPC (which re-loads the tenant).
type NotifyHarborTenantRegisteredTask struct {
	TenantID string `json:"tenant_id"`
}

// Kind implements river.JobArgs.
func (NotifyHarborTenantRegisteredTask) Kind() string { return NotifyHarborQueue }

// InsertOpts bounds retries and dedupes by args, so a double-enqueue for one
// tenant can't fan out two webhooks.
func (NotifyHarborTenantRegisteredTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// HarborNotifyDeps configures the outbound webhook. URL empty ⇒ the worker is a
// clean no-op (feature off). Secret empty ⇒ the body is sent unsigned (only
// acceptable on a trusted network).
type HarborNotifyDeps struct {
	URL    string
	Secret string
}

// NotifyHarborTenantRegisteredProcessor is the River worker for the outbound
// registration webhook.
type NotifyHarborTenantRegisteredProcessor struct {
	river.WorkerDefaults[NotifyHarborTenantRegisteredTask]
	Deps HarborNotifyDeps
}

// Work is the River entrypoint.
func (p *NotifyHarborTenantRegisteredProcessor) Work(ctx context.Context, job *river.Job[NotifyHarborTenantRegisteredTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *NotifyHarborTenantRegisteredProcessor) Timeout(*river.Job[NotifyHarborTenantRegisteredTask]) time.Duration {
	return 20 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &NotifyHarborTenantRegisteredProcessor{Deps: d.HarborNotify})
	})
}

// process POSTs the signed event. Only transient outcomes (network error, 5xx,
// 429) return an error so River retries; everything else is terminal (logged,
// returns nil).
func (p *NotifyHarborTenantRegisteredProcessor) process(ctx context.Context, t NotifyHarborTenantRegisteredTask) error {
	const comp = "jobs.notify_harbor_tenant_registered"
	if t.TenantID == "" {
		slog.WarnContext(ctx, "notify_harbor skipped: missing tenant_id", logging.AttrComponent, comp)
		return nil
	}
	if p.Deps.URL == "" {
		// Feature off (HARBOR_WEBHOOK_URL unset): nothing to deliver.
		slog.DebugContext(ctx, "notify_harbor skipped: webhook url unset", logging.AttrComponent, comp)
		return nil
	}

	body, err := json.Marshal(map[string]string{"event": "tenant.registered", "tenant_id": t.TenantID})
	if err != nil {
		slog.WarnContext(ctx, "notify_harbor failed: marshal body", logging.AttrComponent, comp, logging.AttrError, err)
		return nil // terminal — a retry can't fix a marshal error
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Deps.URL, bytes.NewReader(body))
	if err != nil {
		slog.WarnContext(ctx, "notify_harbor failed: build request", logging.AttrComponent, comp, logging.AttrError, err)
		return nil // terminal (e.g. malformed URL)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.Deps.Secret != "" {
		mac := hmac.New(sha256.New, []byte(p.Deps.Secret))
		_, _ = mac.Write(body)
		req.Header.Set("X-Ogen-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := harborHTTPClient.Do(req)
	if err != nil {
		// Network/timeout — transient, let River retry.
		slog.WarnContext(ctx, "notify_harbor transient failure; will retry", logging.AttrComponent, comp, "tenant_id", t.TenantID, logging.AttrError, err)
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain a bounded amount so the connection can be reused.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		slog.InfoContext(ctx, "notify_harbor delivered", logging.AttrComponent, comp, "tenant_id", t.TenantID, "status", resp.StatusCode)
		return nil
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		// Server-side / rate-limited — transient, retry.
		slog.WarnContext(ctx, "notify_harbor transient status; will retry", logging.AttrComponent, comp, "tenant_id", t.TenantID, "status", resp.StatusCode)
		return fmt.Errorf("harbor webhook returned status %d", resp.StatusCode)
	default:
		// Other 4xx — terminal; a retry won't help (e.g. bad signature / route).
		slog.WarnContext(ctx, "notify_harbor terminal status", logging.AttrComponent, comp, "tenant_id", t.TenantID, "status", resp.StatusCode)
		return nil
	}
}
