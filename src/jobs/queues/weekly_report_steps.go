package queues

import (
	"context"
	"encoding/json"

	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// The weekly report under river-service. Neither job calls outside ogen, so
// each runs in one step over the same Process as its River worker: the
// dispatch hands river-service one build_weekly_report child per workspace
// due, and a build enqueues its emails through the Enqueuer, so send_email
// follows its own routing.

// registerWeeklyReport adds the weekly report jobs.
func (s *JobSteps) registerWeeklyReport() {
	d := s.deps
	dispatch := &WeeklyReportDispatchProcessor{Deps: d.WeeklyReport}
	s.kinds[WeeklyReportDispatchQueue] = dispatch.step
	build := &BuildWeeklyReportProcessor{Deps: d.WeeklyReport, Insert: func(ctx context.Context, args []JobArgs) error {
		for _, a := range args {
			if err := d.Jobs.enqueue(ctx, a, nil); err != nil {
				return err
			}
		}
		return nil
	}}
	s.kinds[BuildWeeklyReportQueue] = build.step
}

// step runs one dispatch tick and returns the builds due as child jobs.
func (p *WeeklyReportDispatchProcessor) step(ctx context.Context, _ StepInput) (StepOutput, error) {
	var children []ChildJob
	tick := *p
	tick.Insert = func(_ context.Context, args []JobArgs) error {
		for _, a := range args {
			raw, err := json.Marshal(a)
			if err != nil {
				return err
			}
			children = append(children, ChildJob{Kind: a.Kind(), Args: raw})
		}
		return nil
	}
	if err := tick.Process(tenantctx.WithSystem(ctx)); err != nil {
		return retry("weekly_report_dispatch", err)
	}
	return done("dispatched", children...)
}

// step builds one workspace's report and enqueues its emails.
func (p *BuildWeeklyReportProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task BuildWeeklyReportTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	if err := p.Process(ctx, task); err != nil {
		return retry("build_weekly_report", err)
	}
	return done("built")
}
