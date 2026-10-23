package queues

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email"
	"github.com/ogen-app/ogen/src/infra/email/templates"
)

func weeklyProcessor(sender email.Sender, logs *fakeEmailLogRepo) *SendEmailProcessor {
	p := newProcessor(sender, newFakeSuppRepo(), logs, "")
	p.Deps.Templates.(*fakeTemplateRepo).m[templates.KeyWeeklyReport] = &models.EmailTemplate{
		Key: templates.KeyWeeklyReport, Subject: "[[ .Report.Headline ]]",
		HTML: `<a href="[[ .UnsubscribeURL ]]">[[ .Report.Workspace ]]</a>`, Text: "[[ .Report.Lede ]]",
	}
	return p
}

func weeklyTask(report *models.WeeklyReportView) SendEmailTask {
	t := task(templates.KeyWeeklyReport, models.EmailKindTransactional)
	t.Report = report
	return t
}

// The weekly report renders the report carried by the job and has a one-click
// unsubscribe scoped to the recipient's membership.
func TestSendEmailWeeklyReport(t *testing.T) {
	sender := &fakeSender{id: "msg_1"}
	p := weeklyProcessor(sender, &fakeEmailLogRepo{})
	report := &models.WeeklyReportView{Workspace: "Acme", Headline: "2 posts went out last week.", Lede: "Nothing failed."}

	if err := p.Process(t.Context(), weeklyTask(report)); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d", len(sender.sent))
	}
	msg := sender.sent[0]
	if msg.Subject != "2 posts went out last week." || msg.Text != "Nothing failed." {
		t.Fatalf("message = %+v", msg)
	}
	unsub := strings.Trim(msg.Headers["List-Unsubscribe"], "<>")
	if !strings.HasPrefix(unsub, "https://app.example/api/email/weekly-report/unsubscribe?token=") ||
		msg.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" || !strings.Contains(msg.HTML, "weekly-report/unsubscribe") {
		t.Fatalf("unsubscribe = %q / %+v", unsub, msg.Headers)
	}
	u, _ := url.Parse(unsub)
	userID, err := email.VerifyScoped("link-secret", email.ScopeWeeklyReport, u.Query().Get("token"), time.Now())
	if err != nil || userID != "u1" {
		t.Fatalf("token subject = %q, %v", userID, err)
	}
}

type fakeReportPrefs map[string]bool

func (f fakeReportPrefs) OptedOut(_ context.Context, userID string) (bool, error) {
	out, ok := f[userID]
	if !ok {
		return false, sql.ErrNoRows
	}
	return out, nil
}

// An owner who opts out after the report was queued, or whose membership is
// gone, doesn't get it.
func TestSendEmailWeeklyReportRechecksOptOut(t *testing.T) {
	for name, prefs := range map[string]fakeReportPrefs{"opted out": {"u1": true}, "membership gone": {}} {
		t.Run(name, func(t *testing.T) {
			sender := &fakeSender{id: "msg_1"}
			logs := &fakeEmailLogRepo{}
			p := weeklyProcessor(sender, logs)
			p.Deps.ReportPrefs = prefs
			if err := p.Process(t.Context(), weeklyTask(&models.WeeklyReportView{Workspace: "Acme"})); err != nil {
				t.Fatal(err)
			}
			if len(sender.sent) != 0 || len(logs.rows) != 1 || logs.rows[0].Status != models.EmailLogSkippedSuppressed {
				t.Fatalf("sent=%d logs=%+v", len(sender.sent), logs.rows)
			}
		})
	}
}

// A weekly report job without its report fails the render terminally instead
// of retrying.
func TestSendEmailWeeklyReportMissing(t *testing.T) {
	sender := &fakeSender{id: "msg_1"}
	logs := &fakeEmailLogRepo{}
	p := weeklyProcessor(sender, logs)
	if err := p.Process(t.Context(), weeklyTask(nil)); err != nil {
		t.Fatalf("missing report must be terminal, got %v", err)
	}
	if len(sender.sent) != 0 || len(logs.rows) != 1 || logs.rows[0].Status != models.EmailLogFailed {
		t.Fatalf("sent=%d logs=%+v", len(sender.sent), logs.rows)
	}
}
