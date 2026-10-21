package queues

import (
	"testing"

	"github.com/riverqueue/river"
)

// TestAllWorkersSelfRegister asserts the B2 self-registration: every job file's
// init() appended exactly one registrar, and RegisterAll wires them all to a
// River registry without panicking (river.AddWorker panics on duplicate kinds,
// so this also proves the kinds are distinct).
func TestAllWorkersSelfRegister(t *testing.T) {
	const wantWorkers = 32 // analyze-asset + reanalyze-campaign + backfill + distill-asset-ideas (one registrar), distill-brand + expire-brand-distills (one registrar), submit, poll, post-first-comment, cancel, withdraw, sweep-orphans, sweep-pending-uploads, cleanup, reconcile, analytics, followers, bootstrap-profile, teardown-profile, process-pdf, process-document, process-audio, process-image, reembed-image, process-url, send-email, cleanup-email-logs, cleanup-connect-sessions, cleanup-plugin-pairings, detect-expiring-connections, cleanup-notifications, detect-manual-publish-due, notify-harbor-tenant-registered, cleanup-login-security, weekly-report-dispatch + build-weekly-report (one registrar), billing reconcile + webhook sweep + scheduled change (one registrar)
	if len(registrars) != wantWorkers {
		t.Fatalf("self-registered workers: got %d, want %d", len(registrars), wantWorkers)
	}
	RegisterAll(river.NewWorkers(), Deps{})
}
