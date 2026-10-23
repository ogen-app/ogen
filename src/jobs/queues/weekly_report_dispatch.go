package queues

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/settings"
	"github.com/ogen-app/ogen/src/usecase/weeklyreport"
)

// The weekly report runs as two jobs. weekly_report_dispatch ticks hourly and,
// for every workspace whose Monday 09:00 has arrived in its own time zone,
// enqueues build_weekly_report for that workspace and week. The build job is
// unique by its args across every state, completed included, and River keeps
// completed jobs for a day, which outlasts the Monday send window: so each
// workspace's week is built once, empty weeks included, with no table of its
// own. The build collects the week, renders it and enqueues one send_email per
// owner with the report in the job args.
const (
	WeeklyReportDispatchQueue = "weekly_report_dispatch"
	BuildWeeklyReportQueue    = "build_weekly_report"
)

const weeklyReportComp = "jobs.weekly_report"

// WeeklyReportDispatchTask is a marker payload.
type WeeklyReportDispatchTask struct{}

// Kind implements river.JobArgs.
func (WeeklyReportDispatchTask) Kind() string { return WeeklyReportDispatchQueue }

// InsertOpts mirrors the other periodic sweeps: one attempt, active-state
// uniqueness so overlapping ticks don't stack.
func (WeeklyReportDispatchTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: periodicUniqueOpts()}
}

// BuildWeeklyReportTask builds and mails one workspace's report for the week
// starting WeekStart (YYYY-MM-DD, a Monday in the workspace's zone).
type BuildWeeklyReportTask struct {
	TenantID  string `json:"tenant_id"`
	WeekStart string `json:"week_start"`
}

// Kind implements river.JobArgs.
func (BuildWeeklyReportTask) Kind() string { return BuildWeeklyReportQueue }

// InsertOpts makes the build unique per (tenant, week) in every state, so a
// later dispatcher tick the same Monday enqueues nothing. A failed build is
// retried a few times; nothing is mailed until it succeeds.
func (BuildWeeklyReportTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 3, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// WeeklyReportStore is what the report jobs need from the report repository.
type WeeklyReportStore interface {
	ListActiveTenants(ctx context.Context) ([]repository.ReportTenant, error)
	TenantName(ctx context.Context, tenantID string) (string, error)
	ListRecipients(ctx context.Context, tenantID string) ([]models.User, error)
}

// ReportCollector gathers one workspace's week.
type ReportCollector interface {
	Collect(ctx context.Context, tenantID, workspace string, w weeklyreport.Window) (weeklyreport.Facts, error)
}

// sentChecker reports whether an email with this idempotency key was already
// logged; the email log repository satisfies it.
type sentChecker interface {
	ExistsByIdempotencyKey(ctx context.Context, key string) (bool, error)
}

// JobInserter enqueues jobs; nil uses the River client on the worker context.
type JobInserter func(ctx context.Context, args []river.JobArgs) error

// WeeklyReportDeps wires both report jobs. A nil Store or Collector disables
// them.
type WeeklyReportDeps struct {
	Store      WeeklyReportStore
	Settings   repository.SettingRepository
	Collector  ReportCollector
	EmailLogs  sentChecker
	AppBaseURL string
}

func (d WeeklyReportDeps) enabled() bool { return d.Store != nil && d.Collector != nil }

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &WeeklyReportDispatchProcessor{Deps: d.WeeklyReport})
		river.AddWorker(w, &BuildWeeklyReportProcessor{Deps: d.WeeklyReport})
	})
}

// WeeklyReportDispatchProcessor runs one dispatch tick.
type WeeklyReportDispatchProcessor struct {
	river.WorkerDefaults[WeeklyReportDispatchTask]
	Deps   WeeklyReportDeps
	Insert JobInserter
	Now    func() time.Time
}

func (p *WeeklyReportDispatchProcessor) Work(ctx context.Context, job *river.Job[WeeklyReportDispatchTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(tenantctx.WithSystem(ctx))
}

func (p *WeeklyReportDispatchProcessor) Timeout(*river.Job[WeeklyReportDispatchTask]) time.Duration {
	return tenantSweepJobTimeout
}

// Process enqueues a build for every workspace whose report is due. It never
// fails the tick: a failed read is logged and the next tick tries again.
func (p *WeeklyReportDispatchProcessor) Process(ctx context.Context) error {
	if !p.Deps.enabled() {
		return nil
	}
	now := clock(p.Now)
	tenants, err := p.Deps.Store.ListActiveTenants(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "weekly report: list tenants failed", logging.AttrComponent, weeklyReportComp, logging.AttrError, err)
		return nil
	}
	var builds []river.JobArgs
	for _, t := range tenants {
		loc, zone := settings.WorkspaceTimezone(tenantctx.With(ctx, t.ID), p.Deps.Settings)
		if w, due := weeklyreport.Due(now, loc, zone); due {
			builds = append(builds, BuildWeeklyReportTask{TenantID: t.ID, WeekStart: w.StartDate()})
		}
	}
	if err := insertJobs(ctx, p.Insert, builds); err != nil {
		slog.ErrorContext(ctx, "weekly report: enqueue builds failed", logging.AttrComponent, weeklyReportComp, logging.AttrError, err)
	}
	return nil
}

// BuildWeeklyReportProcessor builds one workspace's report and enqueues its
// emails.
type BuildWeeklyReportProcessor struct {
	river.WorkerDefaults[BuildWeeklyReportTask]
	Deps   WeeklyReportDeps
	Insert JobInserter
	Now    func() time.Time
}

func (p *BuildWeeklyReportProcessor) Work(ctx context.Context, job *river.Job[BuildWeeklyReportTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.Process(ctx, job.Args)
}

func (p *BuildWeeklyReportProcessor) Timeout(*river.Job[BuildWeeklyReportTask]) time.Duration {
	return 2 * time.Minute
}

// Process builds the report. A read failure returns the error so River retries
// the build; a workspace that is gone, an empty week, or no recipients ends it
// quietly.
func (p *BuildWeeklyReportProcessor) Process(ctx context.Context, t BuildWeeklyReportTask) error {
	d := p.Deps
	if !d.enabled() {
		return nil
	}
	start, err := time.Parse(time.DateOnly, t.WeekStart)
	if err != nil {
		slog.WarnContext(ctx, "weekly report: bad week start", logging.AttrComponent, weeklyReportComp, "week_start", t.WeekStart)
		return nil
	}
	ctx = tenantctx.With(ctx, t.TenantID)
	name, err := d.Store.TenantName(ctx, t.TenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // gone or no longer active
	}
	if err != nil {
		return err
	}
	loc, zone := settings.WorkspaceTimezone(ctx, d.Settings)
	facts, err := d.Collector.Collect(ctx, t.TenantID, name, weeklyreport.WeekOf(start, loc, zone, clock(p.Now)))
	if err != nil {
		jobs.WeeklyReportBuildErrors.Add(1)
		return err
	}
	for _, s := range facts.SectionErrors {
		jobs.WeeklyReportSectionErrors.Add(s, 1)
	}
	if facts.Empty() {
		jobs.WeeklyReportSkippedEmpty.Add(1)
		return nil
	}
	sends, err := p.sends(ctx, t, weeklyreport.Render(facts, d.AppBaseURL))
	if err != nil {
		return err
	}
	if err := insertJobs(ctx, p.Insert, sends); err != nil {
		return err
	}
	jobs.WeeklyReportBuilt.Add(1)
	jobs.WeeklyReportEnqueued.Add(int64(len(sends)))
	return nil
}

// sends builds one send_email job per owner who hasn't opted out and hasn't
// already been sent this week's report.
func (p *BuildWeeklyReportProcessor) sends(ctx context.Context, t BuildWeeklyReportTask, view models.WeeklyReportView) ([]river.JobArgs, error) {
	owners, err := p.Deps.Store.ListRecipients(ctx, t.TenantID)
	if err != nil {
		return nil, err
	}
	var out []river.JobArgs
	for _, u := range owners {
		key := "weekly_report:" + t.TenantID + ":" + t.WeekStart + ":" + u.ID
		if p.Deps.EmailLogs != nil {
			sent, err := p.Deps.EmailLogs.ExistsByIdempotencyKey(ctx, key)
			if err != nil {
				return nil, err
			}
			if sent {
				continue
			}
		}
		out = append(out, SendEmailTask{
			UserID:         u.ID,
			TenantID:       t.TenantID,
			TemplateKey:    templates.KeyWeeklyReport,
			EmailKind:      models.EmailKindTransactional,
			IdempotencyKey: key,
			Report:         &view,
		})
	}
	return out, nil
}

func clock(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now().UTC()
}

// insertJobs enqueues args with insert, or with the River client carried by
// the worker context when insert is nil.
func insertJobs(ctx context.Context, insert JobInserter, args []river.JobArgs) error {
	if len(args) == 0 {
		return nil
	}
	if insert != nil {
		return insert(ctx, args)
	}
	client, err := river.ClientFromContextSafely[*sql.Tx](ctx)
	if err != nil {
		return err
	}
	params := make([]river.InsertManyParams, len(args))
	for i, a := range args {
		params[i] = river.InsertManyParams{Args: a}
	}
	_, err = client.InsertMany(ctx, params)
	return err
}
