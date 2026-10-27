package queues

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"

	jobsv1 "github.com/ogen-app/ogen/gen/jobs/v1"
	"github.com/ogen-app/ogen/src/domain/models"
)

// TestEnqueueRequestCoversEveryEnqueuedKind checks that each kind ogen
// enqueues converts to the jobs.v1 args field of the same name, so
// river-service runs it under the kind ogen asked for.
func TestEnqueueRequestCoversEveryEnqueuedKind(t *testing.T) {
	campaign := "c1"
	kinds := []river.JobArgs{
		SubmitPostTask{PostID: "p1"},
		PollZernioStatusTask{PostID: "p1", PublisherPostID: "z1"},
		CancelZernioJobTask{PostID: "p1", Target: CancelTargetDraft, Actor: "u1"},
		WithdrawZernioPostTask{PublisherPostID: "z1", PostID: "p1", TenantID: "t1", Reason: WithdrawReasonPostDeleted, Actor: "u1"},
		PostFirstCommentTask{PostID: "p1", PublisherPostID: "z1"},
		BootstrapZernioProfileTask{TenantID: "t1"},
		TeardownZernioProfileTask{TenantID: "t1", Reason: TeardownReasonTrialUnpaid},
		SendEmailTask{UserID: "u1", TenantID: "t1", TemplateKey: "welcome", EmailKind: models.EmailKindTransactional, IdempotencyKey: "welcome:u1", Vars: map[string]string{"a": "b"}},
		NotifyHarborTenantRegisteredTask{TenantID: "t1"},
		ProcessPDFTask{AssetID: "a1", TenantID: "t1", OriginalName: "x.pdf", MimeType: "application/pdf"},
		ProcessDocumentTask{AssetID: "a1", TenantID: "t1", StorageKey: "k"},
		ProcessURLTask{AssetID: "a1", TenantID: "t1", SourceURL: "https://example.com", Refresh: true},
		ProcessImageTask{AssetID: "a1", TenantID: "t1", RunKey: "run-1", PinnedModel: "m"},
		ProcessAudioTask{AssetID: "a1", TenantID: "t1", RunKey: "run-1"},
		ReembedImageTask{AssetID: "a1", TenantID: "t1"},
		AnalyzeAssetTask{TenantID: "t1", AssetID: "a1", CampaignID: &campaign, Reason: "ingested"},
		ReanalyzeCampaignTask{TenantID: "t1", CampaignID: "c1"},
		BillingScheduledChangeTask{ChangeID: "ch1"},
		DistillBrandTask{TenantID: "t1", RunID: "r1"},
		DistillAssetIdeasTask{TenantID: "t1", RunID: "r1"},
	}
	for _, args := range kinds {
		t.Run(args.Kind(), func(t *testing.T) {
			req, err := enqueueRequest(args, nil)
			if err != nil {
				t.Fatalf("enqueueRequest: %v", err)
			}
			msg := req.ProtoReflect()
			field := msg.WhichOneof(msg.Descriptor().Oneofs().ByName("args"))
			if field == nil || string(field.Name()) != args.Kind() {
				t.Fatalf("args field = %v, want %s", field, args.Kind())
			}
			if !RelayableKind(args.Kind()) {
				t.Errorf("RelayableKind(%s) = false", args.Kind())
			}
		})
	}
}

func TestEnqueueRequestFields(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	req, err := enqueueRequest(SendEmailTask{
		UserID: "u1", TemplateKey: "drip_day2", EmailKind: models.EmailKindMarketing,
		IdempotencyKey: "drip_day2:u1", Vars: map[string]string{"k": "v"},
	}, &river.InsertOpts{ScheduledAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if !req.GetScheduledAt().AsTime().Equal(at) {
		t.Errorf("scheduled_at = %v, want %v", req.GetScheduledAt().AsTime(), at)
	}
	got := req.GetSendEmail()
	if got.GetEmailKind() != "marketing" || got.GetIdempotencyKey() != "drip_day2:u1" || got.GetVars()["k"] != "v" {
		t.Errorf("send_email args = %v", got)
	}

	if _, err := enqueueRequest(CleanupPostLogsTask{}, nil); err == nil {
		t.Error("a periodic kind has no args message, want an error")
	}
}

// TestEnqueueRequestKeepsFieldsMainAdded guards the fields that once had no
// jobs.v1 counterpart: without them a weekly report email went out empty and
// an unpaid-trial teardown ran as a workspace-delete teardown.
func TestEnqueueRequestKeepsFieldsMainAdded(t *testing.T) {
	report := &models.WeeklyReportView{SchemaVersion: 1, Workspace: "Acme", Headline: "12 posts went out"}
	req, err := enqueueRequest(SendEmailTask{UserID: "u1", TemplateKey: "weekly_report", Report: report}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SendEmailTaskFromArgs(req.GetSendEmail())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Report, report) {
		t.Errorf("report = %+v, want %+v", got.Report, report)
	}

	plain, err := SendEmailTaskFromArgs(&jobsv1.SendEmailArgs{UserId: "u1"})
	if err != nil || plain.Report != nil {
		t.Errorf("no report: got %+v, %v", plain.Report, err)
	}
	if _, err := SendEmailTaskFromArgs(&jobsv1.SendEmailArgs{Report: []byte("{")}); err == nil {
		t.Error("a malformed report decoded, want an error")
	}

	req, err = enqueueRequest(TeardownZernioProfileTask{TenantID: "t1", Reason: TeardownReasonTrialUnpaid}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := req.GetTeardownZernioProfile().GetReason(); r != TeardownReasonTrialUnpaid {
		t.Errorf("teardown reason = %q", r)
	}
}

func TestRelayKinds(t *testing.T) {
	got, err := RelayKinds([]string{" Send_Email", "send_email", "cleanup_post_logs", ""}, "dns:///river-service:9092")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "send_email,cleanup_post_logs" {
		t.Errorf("RelayKinds = %v", got)
	}
	if _, err := RelayKinds([]string{"send_emails"}, "dns:///river-service:9092"); err == nil || !strings.Contains(err.Error(), `"send_emails"`) {
		t.Errorf("unknown kind: err = %v", err)
	}
	if _, err := RelayKinds([]string{"send_email"}, ""); err == nil || !strings.Contains(err.Error(), "RIVER_SERVICE_ADDR") {
		t.Errorf("no address: err = %v", err)
	}
	if got, err := RelayKinds(nil, ""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v, %v", got, err)
	}
}

// TestPeriodicJobsSkipRelayedSweeps checks that a sweep river-service owns is
// not also scheduled by ogen.
func TestPeriodicJobsSkipRelayedSweeps(t *testing.T) {
	cfg := PeriodicConfig{CleanupEvery: time.Hour, ReconcileEvery: 5 * time.Minute, EmailCleanupEvery: time.Hour}
	if got := len(cfg.PeriodicJobs()); got != 3 {
		t.Fatalf("periodic jobs = %d, want 3", got)
	}
	cfg.Relayed = func(kind string) bool { return kind == CleanupEmailLogsQueue }
	if got := len(cfg.PeriodicJobs()); got != 2 {
		t.Fatalf("periodic jobs with cleanup_email_logs relayed = %d, want 2", got)
	}
}
