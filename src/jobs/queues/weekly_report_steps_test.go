package queues

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWeeklyReportDispatchStepHandsBuildsToRiverService(t *testing.T) {
	p := &WeeklyReportDispatchProcessor{
		Deps: WeeklyReportDeps{Store: fakeReportStore{}, Collector: &fakeCollector{}},
		Now:  func() time.Time { return time.Date(2026, 10, 19, 9, 5, 0, 0, time.UTC) },
	}
	out, err := p.step(t.Context(), StepInput{Kind: WeeklyReportDispatchQueue})
	if err != nil || out.Done == nil {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if len(out.Done.Enqueue) != 2 {
		t.Fatalf("children = %+v, want a build per workspace", out.Done.Enqueue)
	}
	var first BuildWeeklyReportTask
	if c := out.Done.Enqueue[0]; c.Kind != BuildWeeklyReportQueue || json.Unmarshal(c.Args, &first) != nil ||
		first != (BuildWeeklyReportTask{TenantID: "tn-busy", WeekStart: "2026-10-12"}) {
		t.Fatalf("first child = %s %s", c.Kind, c.Args)
	}
}

func TestBuildWeeklyReportStepEnqueuesThroughTheEnqueuer(t *testing.T) {
	// With no enqueuer the emails can't be queued, so the build retries
	// rather than dropping them.
	s := NewJobSteps(Deps{WeeklyReport: WeeklyReportDeps{Store: fakeReportStore{}, Collector: &fakeCollector{busy: "tn-busy"}, EmailLogs: fakeSentLog{}}})
	out, err := s.Step(t.Context(), StepInput{Kind: BuildWeeklyReportQueue, Args: []byte(`{"tenant_id":"tn-busy","week_start":"2026-10-12"}`)})
	if err != nil || out.Retry == nil {
		t.Fatalf("out = %+v, err = %v; want a retry", out, err)
	}

	var got capture
	p := &BuildWeeklyReportProcessor{
		Deps:   WeeklyReportDeps{Store: fakeReportStore{}, Collector: &fakeCollector{busy: "tn-busy"}, EmailLogs: fakeSentLog{}},
		Insert: got.insert,
	}
	out, err = p.step(t.Context(), StepInput{Kind: BuildWeeklyReportQueue, Args: []byte(`{"tenant_id":"tn-busy","week_start":"2026-10-12"}`)})
	if err != nil || out.Done == nil || out.Done.Outcome != "built" || len(got.args) != 2 || len(out.Done.Enqueue) != 0 {
		t.Fatalf("out = %+v, err = %v, sends = %d", out, err, len(got.args))
	}
}
