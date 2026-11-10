package weeklyreport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

type fakeOnDemandStore struct{ optedOut bool }

func (fakeOnDemandStore) TenantName(context.Context, string) (string, error) { return "Acme", nil }
func (f fakeOnDemandStore) OptedOut(context.Context, string) (bool, error) {
	return f.optedOut, nil
}

// A date picks the Monday of its week; none picks last full week's.
func TestWeekStart(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) // a Tuesday
	for date, want := range map[string]string{"": "2026-11-02", "2026-11-08": "2026-11-02", "2026-11-02": "2026-11-02", "2026-11-09": "2026-11-09"} {
		got, err := weekStart(date, now, time.UTC)
		if err != nil || got.Format(time.DateOnly) != want {
			t.Fatalf("weekStart(%q) = %v, %v; want %s", date, got, err, want)
		}
	}
	if _, err := weekStart("10/11", now, time.UTC); !errors.Is(err, ErrInvalidDate) {
		t.Fatalf("bad date = %v", err)
	}
}

// Last full week is built; the week now is in is refused; the email goes
// to the one member asking, under its own key, unless they turned it off.
func TestOnDemand(t *testing.T) {
	ctx := tenantctx.With(t.Context(), "t1")
	var sent []string
	o := &OnDemand{Collector: &Collector{Store: fakeStore{}}, Store: fakeOnDemandStore{},
		Now: func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC) },
		Send: func(_ context.Context, userID, tenantID, key string, view models.WeeklyReportView) error {
			sent = append(sent, userID+" "+tenantID+" "+key+" "+view.Workspace)
			return nil
		}}
	wk, err := o.Build(ctx, "")
	if err != nil || wk.Start != "2026-11-02" || wk.End != "2026-11-08" || wk.View.Workspace != "Acme" {
		t.Fatalf("week = %+v, %v", wk, err)
	}
	if _, err := o.Build(ctx, "2026-11-10"); !errors.Is(err, ErrWeekNotOver) {
		t.Fatalf("this week = %v", err)
	}
	if _, err := o.EmailTo(ctx, "u1", "2026-11-04"); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "u1 t1 weekly_report_on_demand:t1:2026-11-02:u1 Acme" {
		t.Fatalf("sent = %v", sent)
	}
	o.Sent = func(_ context.Context, key string) (bool, error) {
		return key == "weekly_report_on_demand:t1:2026-11-02:u1", nil
	}
	if _, err := o.EmailTo(ctx, "u1", "2026-11-04"); !errors.Is(err, ErrAlreadySent) || len(sent) != 1 {
		t.Fatalf("again = %v, sent %v", err, sent)
	}
	o.Store = fakeOnDemandStore{optedOut: true}
	if _, err := o.EmailTo(ctx, "u1", ""); !errors.Is(err, ErrOptedOut) || len(sent) != 1 {
		t.Fatalf("opted out = %v, sent %v", err, sent)
	}
}
