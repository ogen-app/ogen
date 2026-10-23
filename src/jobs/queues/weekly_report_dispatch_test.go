package queues

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/weeklyreport"
)

// fakeReportStore serves two active workspaces: tn-busy with one owner left
// after the opt-out filter, and tn-idle.
type fakeReportStore struct{}

func (fakeReportStore) ListActiveTenants(context.Context) ([]repository.ReportTenant, error) {
	return []repository.ReportTenant{{ID: "tn-busy", Name: "Acme"}, {ID: "tn-idle", Name: "Idle"}}, nil
}

func (fakeReportStore) TenantName(_ context.Context, id string) (string, error) {
	switch id {
	case "tn-busy":
		return "Acme", nil
	case "tn-idle":
		return "Idle", nil
	}
	return "", sql.ErrNoRows
}

func (fakeReportStore) ListRecipients(_ context.Context, id string) ([]models.User, error) {
	if id == "tn-busy" {
		return []models.User{{ID: "own-1"}, {ID: "own-2"}}, nil
	}
	return nil, nil
}

// fakeCollector reports one published post for the busy tenant and nothing
// for anyone else.
type fakeCollector struct {
	busy  string
	calls int
	err   error
}

func (f *fakeCollector) Collect(_ context.Context, tenantID, workspace string, w weeklyreport.Window) (weeklyreport.Facts, error) {
	f.calls++
	facts := weeklyreport.Facts{Window: w, Workspace: workspace, ActiveMembers: -1}
	if f.err != nil {
		return facts, f.err
	}
	if tenantID == f.busy {
		facts.Published = []repository.ReportPost{{ID: "p1", Platform: "LinkedIn", PublishedAt: w.Start.Add(time.Hour)}}
	}
	return facts, nil
}

type fakeSentLog map[string]bool

func (f fakeSentLog) ExistsByIdempotencyKey(_ context.Context, key string) (bool, error) {
	return f[key], nil
}

type capture struct{ args []river.JobArgs }

func (c *capture) insert(_ context.Context, args []river.JobArgs) error {
	c.args = append(c.args, args...)
	return nil
}

func TestWeeklyReportDispatchEnqueuesDueBuilds(t *testing.T) {
	var got capture
	p := &WeeklyReportDispatchProcessor{
		Deps:   WeeklyReportDeps{Store: fakeReportStore{}, Collector: &fakeCollector{}},
		Insert: got.insert,
	}
	// Every workspace is on UTC (no timezone setting): not due before 09:00.
	p.Now = func() time.Time { return time.Date(2026, 10, 19, 8, 59, 0, 0, time.UTC) }
	if err := p.Process(t.Context()); err != nil || len(got.args) != 0 {
		t.Fatalf("before nine: err=%v builds=%v", err, got.args)
	}
	p.Now = func() time.Time { return time.Date(2026, 10, 19, 9, 5, 0, 0, time.UTC) }
	if err := p.Process(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []river.JobArgs{
		BuildWeeklyReportTask{TenantID: "tn-busy", WeekStart: "2026-10-12"},
		BuildWeeklyReportTask{TenantID: "tn-idle", WeekStart: "2026-10-12"},
	}
	if !slices.Equal(got.args, want) {
		t.Fatalf("builds = %+v", got.args)
	}
}

// The build is unique per (tenant, week) in every state, completed included,
// which is what keeps a later tick from building the week again.
func TestBuildWeeklyReportIsUniquePerWeek(t *testing.T) {
	opts := BuildWeeklyReportTask{}.InsertOpts()
	if !opts.UniqueOpts.ByArgs || len(opts.UniqueOpts.ByState) != 0 {
		t.Fatalf("unique opts = %+v (empty ByState is River's default, which includes completed)", opts.UniqueOpts)
	}
}

func TestBuildWeeklyReport(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 19, 9, 5, 0, 0, time.UTC) }
	newProc := func(c *fakeCollector, sent fakeSentLog, got *capture) *BuildWeeklyReportProcessor {
		return &BuildWeeklyReportProcessor{
			Deps:   WeeklyReportDeps{Store: fakeReportStore{}, Collector: c, EmailLogs: sent, AppBaseURL: "https://app.example"},
			Insert: got.insert, Now: now,
		}
	}
	task := BuildWeeklyReportTask{TenantID: "tn-busy", WeekStart: "2026-10-12"}

	t.Run("mails each owner not yet sent", func(t *testing.T) {
		var got capture
		sent := fakeSentLog{"weekly_report:tn-busy:2026-10-12:own-2": true}
		if err := newProc(&fakeCollector{busy: "tn-busy"}, sent, &got).Process(t.Context(), task); err != nil {
			t.Fatal(err)
		}
		if len(got.args) != 1 {
			t.Fatalf("sends = %+v", got.args)
		}
		s := got.args[0].(SendEmailTask)
		if s.UserID != "own-1" || s.TenantID != "tn-busy" || s.TemplateKey != templates.KeyWeeklyReport ||
			s.IdempotencyKey != "weekly_report:tn-busy:2026-10-12:own-1" || s.Report == nil ||
			s.Report.Headline != "1 post went out last week." || s.Report.RangeLabel != "Oct 12 – 18, 2026" {
			t.Fatalf("send = %+v report=%+v", s, s.Report)
		}
	})

	t.Run("empty week mails nothing", func(t *testing.T) {
		var got capture
		idle := BuildWeeklyReportTask{TenantID: "tn-idle", WeekStart: "2026-10-12"}
		if err := newProc(&fakeCollector{busy: "tn-busy"}, nil, &got).Process(t.Context(), idle); err != nil || len(got.args) != 0 {
			t.Fatalf("err=%v sends=%v", err, got.args)
		}
	})

	t.Run("gone workspace ends quietly", func(t *testing.T) {
		var got capture
		c := &fakeCollector{busy: "tn-busy"}
		gone := BuildWeeklyReportTask{TenantID: "tn-gone", WeekStart: "2026-10-12"}
		if err := newProc(c, nil, &got).Process(t.Context(), gone); err != nil || len(got.args) != 0 || c.calls != 0 {
			t.Fatalf("err=%v sends=%v calls=%d", err, got.args, c.calls)
		}
	})

	t.Run("failed read is retried", func(t *testing.T) {
		var got capture
		c := &fakeCollector{busy: "tn-busy", err: errors.New("db down")}
		if err := newProc(c, nil, &got).Process(t.Context(), task); err == nil || len(got.args) != 0 {
			t.Fatalf("err=%v sends=%v", err, got.args)
		}
	})
}

func TestPeriodicJobsWeeklyReportNeedsInterval(t *testing.T) {
	base := PeriodicConfig{CleanupEvery: time.Hour, ReconcileEvery: 5 * time.Minute}
	if got := len(base.PeriodicJobs()); got != 2 {
		t.Fatalf("zero interval must not register the dispatcher: got %d jobs", got)
	}
	base.WeeklyReportEvery = time.Hour
	if got := len(base.PeriodicJobs()); got != 3 {
		t.Fatalf("positive interval must register the dispatcher: got %d jobs", got)
	}
}
