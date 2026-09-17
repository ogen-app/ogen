package jobs

import (
	"expvar"
	"sync"
	"time"
)

// Metrics surfaces Ogen's job-queue health to operators (CON-69 §13).
// Counters are exposed via expvar at /debug/vars; latency averages
// live as a small in-process histogram so we don't pull a metrics
// library for the MVP. River's tables cover per-queue depth
// (upcoming/running/succeeded/failed) — this is what's left.
//
// Field naming is "ogen_jobs_*" so the expvar surface stays distinct
// from runtime/expvar's stock keys (memstats, cmdline, etc.).
var (
	// Submit lifecycle.
	ZernioSubmitSucceeded = expvar.NewInt("ogen_jobs_zernio_submit_succeeded")
	ZernioSubmitFailed    = expvar.NewInt("ogen_jobs_zernio_submit_failed")
	ZernioSubmitRetried   = expvar.NewInt("ogen_jobs_zernio_submit_retried")

	// Poll lifecycle.
	ZernioPollSucceeded = expvar.NewInt("ogen_jobs_zernio_poll_succeeded")
	ZernioPollFailed    = expvar.NewInt("ogen_jobs_zernio_poll_failed")
	ZernioPollRetried   = expvar.NewInt("ogen_jobs_zernio_poll_retried")

	// Cancel lifecycle.
	ZernioCancelSucceeded        = expvar.NewInt("ogen_jobs_zernio_cancel_succeeded")
	ZernioCancelAlreadyPublished = expvar.NewInt("ogen_jobs_zernio_cancel_already_published")
	ZernioCancelFailed           = expvar.NewInt("ogen_jobs_zernio_cancel_failed")

	// Reconciliation.
	ReconciliationTimeouts = expvar.NewInt("ogen_jobs_reconciliation_timeouts")

	// Manual-publish-due sweep (CON-285): posts past their manual-publish time
	// that triggered a post.manual_publish_due notification this tick.
	ManualPublishDueSwept = expvar.NewInt("ogen_jobs_manual_publish_due_swept")

	// Analytics refresh lifecycle (CON-93 §11).
	ZernioAnalyticsRefreshSucceeded = expvar.NewInt("ogen_jobs_zernio_analytics_refresh_succeeded")
	ZernioAnalyticsRefreshFailed    = expvar.NewInt("ogen_jobs_zernio_analytics_refresh_failed")
	// ZernioAnalyticsPostsUpserted counts posts whose metrics actually CHANGED
	// this tick (a new trend-history point was appended) — CON-236. Unchanged
	// and decay-skipped posts are tracked separately so the dedup/decay
	// reduction is observable at /debug/vars.
	ZernioAnalyticsPostsUpserted   = expvar.NewInt("ogen_jobs_zernio_analytics_posts_upserted")
	ZernioAnalyticsPostsUnchanged  = expvar.NewInt("ogen_jobs_zernio_analytics_posts_unchanged")
	ZernioAnalyticsPostsDueSkipped = expvar.NewInt("ogen_jobs_zernio_analytics_posts_due_skipped")

	// Follower refresh lifecycle (CON-153).
	ZernioFollowerRefreshSucceeded = expvar.NewInt("ogen_jobs_zernio_follower_refresh_succeeded")
	ZernioFollowerRefreshFailed    = expvar.NewInt("ogen_jobs_zernio_follower_refresh_failed")
	ZernioFollowerPointsInserted   = expvar.NewInt("ogen_jobs_zernio_follower_points_inserted")

	// External-post verification lifecycle (CON-153).
	ZernioExternalVerifySucceeded = expvar.NewInt("ogen_jobs_zernio_external_verify_succeeded")
	ZernioExternalVerifyFailed    = expvar.NewInt("ogen_jobs_zernio_external_verify_failed")
	ZernioExternalVerifyNotFound  = expvar.NewInt("ogen_jobs_zernio_external_verify_not_found")

	// Account disconnect lifecycle (CON-133, user-initiated via the API).
	ZernioAccountDisconnectSucceeded = expvar.NewInt("ogen_jobs_zernio_account_disconnect_succeeded")
	ZernioAccountDisconnectFailed    = expvar.NewInt("ogen_jobs_zernio_account_disconnect_failed")

	// Connection-expiry notification sweep (CON-219). Detected counts every
	// account entering a notify stage on a tick (may repeat across ticks until
	// reconnected); Notified counts emails actually enqueued (deduped).
	ZernioHealthSweepSucceeded             = expvar.NewInt("ogen_jobs_zernio_health_sweep_succeeded")
	ZernioHealthSweepFailed                = expvar.NewInt("ogen_jobs_zernio_health_sweep_failed")
	ZernioHealthAccountsChecked            = expvar.NewInt("ogen_jobs_zernio_health_accounts_checked")
	ZernioConnectionExpiringDetected       = expvar.NewInt("ogen_jobs_zernio_connection_expiring_detected")
	ZernioConnectionActionRequiredDetected = expvar.NewInt("ogen_jobs_zernio_connection_action_required_detected")
	ZernioConnectionExpiryNotified         = expvar.NewInt("ogen_jobs_zernio_connection_expiry_notified")

	// Headless connect-session lifecycle (CON-217, hide Zernio's account selector).
	ZernioConnectCallbackReceived = expvar.NewInt("ogen_jobs_zernio_connect_callback_received")
	ZernioConnectAutoFinalized    = expvar.NewInt("ogen_jobs_zernio_connect_auto_finalized")
	ZernioConnectSelectorShown    = expvar.NewInt("ogen_jobs_zernio_connect_selector_shown")
	ZernioConnectSelectSucceeded  = expvar.NewInt("ogen_jobs_zernio_connect_select_succeeded")
	ZernioConnectSelectFailed     = expvar.NewInt("ogen_jobs_zernio_connect_select_failed")
	ZernioConnectSessionExpired   = expvar.NewInt("ogen_jobs_zernio_connect_session_expired")
	ZernioConnectSessionsSwept    = expvar.NewInt("ogen_jobs_zernio_connect_sessions_swept")

	// Post Log lifecycle.
	PostLogTruncations = expvar.NewInt("ogen_jobs_postlog_truncations")
	PostLogCleaned     = expvar.NewInt("ogen_jobs_postlog_cleaned")

	// Zernio API latency (millisecond average over a sliding window).
	zernioLatency = newLatency()
)

func init() {
	expvar.Publish("ogen_jobs_zernio_latency_ms_avg", expvar.Func(func() any { return zernioLatency.Avg() }))
}

// ObserveZernioCall records a single API call duration so the
// expvar-exposed average updates. Safe to call from any goroutine.
func ObserveZernioCall(d time.Duration) {
	zernioLatency.Add(d)
}

type latency struct {
	mu   sync.Mutex
	buf  [256]time.Duration
	idx  int
	full bool
}

func newLatency() *latency { return &latency{} }

func (l *latency) Add(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf[l.idx] = d
	l.idx = (l.idx + 1) % len(l.buf)
	if l.idx == 0 {
		l.full = true
	}
}

func (l *latency) Avg() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.idx
	if l.full {
		n = len(l.buf)
	}
	if n == 0 {
		return 0
	}
	var total time.Duration
	for i := range n {
		total += l.buf[i]
	}
	return (total / time.Duration(n)).Milliseconds()
}
