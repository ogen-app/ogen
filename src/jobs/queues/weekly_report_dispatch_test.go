package queues

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
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

type capture struct{ args []JobArgs }

func (c *capture) insert(_ context.Context, args []JobArgs) error {
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
	want := []JobArgs{
		BuildWeeklyReportTask{TenantID: "tn-busy", WeekStart: "2026-10-12"},
		BuildWeeklyReportTask{TenantID: "tn-idle", WeekStart: "2026-10-12"},
	}
	if !slices.Equal(got.args, want) {
		t.Fatalf("builds = %+v", got.args)
	}
}

// zoneSettings serves the timezone setting per tenant; failing tenants return a
// read error.
type zoneSettings struct {
	zones   map[string]string
	failing map[string]bool
}

func (z zoneSettings) GetByKey(ctx context.Context, _ string) (*models.Setting, error) {
	tid, _ := tenantctx.From(ctx)
	if z.failing[tid] {
		return nil, errors.New("connection reset")
	}
	if zone, ok := z.zones[tid]; ok {
		return &models.Setting{Value: zone}, nil
	}
	return nil, sql.ErrNoRows
}
func (zoneSettings) List(context.Context) ([]models.Setting, error)               { return nil, nil }
func (zoneSettings) ListTenantIDsByKey(context.Context, string) ([]string, error) { return nil, nil }
func (zoneSettings) Upsert(context.Context, *models.Setting) error                { return nil }
func (zoneSettings) Delete(context.Context, string) (bool, error)                 { return false, nil }

// A workspace whose zone can't be read is skipped rather than judged on UTC;
// one whose zone is still before Monday 09:00 is not due yet.
func TestWeeklyReportDispatchUsesWorkspaceZone(t *testing.T) {
	var got capture
	p := &WeeklyReportDispatchProcessor{
		Deps: WeeklyReportDeps{
			Store:     fakeReportStore{},
			Collector: &fakeCollector{},
			Settings: zoneSettings{
				zones:   map[string]string{"tn-idle": "America/New_York"},
				failing: map[string]bool{"tn-busy": true},
			},
		},
		Insert: got.insert,
		Now:    func() time.Time { return time.Date(2026, 10, 19, 9, 5, 0, 0, time.UTC) },
	}
	if err := p.Process(t.Context()); err != nil || len(got.args) != 0 {
		t.Fatalf("err=%v builds=%v", err, got.args)
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
