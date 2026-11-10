package weeklyreport

import (
	"context"
	"errors"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// Errors OnDemand returns for the caller to explain.
var (
	// ErrWeekNotOver is a week that hasn't ended yet in the workspace's zone.
	ErrWeekNotOver = errors.New("weekly report: that week isn't over yet")
	// ErrInvalidDate is a date that isn't YYYY-MM-DD.
	ErrInvalidDate = errors.New("weekly report: invalid date")
	// ErrOptedOut is a member who turned the weekly report email off.
	ErrOptedOut = errors.New("weekly report: the email is turned off for this member")
	// ErrAlreadySent is a week's report already emailed to the member on
	// request.
	ErrAlreadySent = errors.New("weekly report: already emailed for that week")
)

// OnDemandStore is what OnDemand reads besides the week.
type OnDemandStore interface {
	TenantName(ctx context.Context, tenantID string) (string, error)
	OptedOut(ctx context.Context, userID string) (bool, error)
}

// SendFunc queues the report email to one member under an idempotency key.
type SendFunc func(ctx context.Context, userID, tenantID, key string, view models.WeeklyReportView) error

// OnDemand builds a workspace's weekly report when a member asks for it, the
// same one the Monday email sends, and emails it to that member alone.
type OnDemand struct {
	Collector  *Collector
	Store      OnDemandStore
	Settings   repository.SettingRepository
	AppBaseURL string
	// Send queues the email; nil leaves EmailTo failing.
	Send SendFunc
	// Sent reports whether an email under the key was already sent; nil
	// leaves the email log's unique key to drop a repeat.
	Sent func(ctx context.Context, key string) (bool, error)
	Now  func() time.Time
}

// Week is one built report.
type Week struct {
	// Start and End are the week's first and last days, YYYY-MM-DD.
	Start string
	End   string
	View  models.WeeklyReportView
	// Empty is a week with nothing worth a Monday email.
	Empty bool
}

// Build is the report of the Monday-to-Sunday week holding date
// (YYYY-MM-DD in the workspace's zone), or of last full week when date is
// empty. ctx must be scoped to the workspace.
func (o *OnDemand) Build(ctx context.Context, date string) (*Week, error) {
	tenantID, _ := tenantctx.From(ctx)
	loc, zone, err := settings.LookupWorkspaceTimezone(ctx, o.Settings)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if o.Now != nil {
		now = o.Now()
	}
	start, err := weekStart(date, now, loc)
	if err != nil {
		return nil, err
	}
	w := WeekOf(start, loc, zone, now)
	if w.End.After(now) {
		return nil, ErrWeekNotOver
	}
	name, err := o.Store.TenantName(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	facts, err := o.Collector.Collect(ctx, tenantID, name, w)
	if err != nil {
		return nil, err
	}
	return &Week{Start: w.StartDate(), End: w.End.AddDate(0, 0, -1).Format(time.DateOnly), View: Render(facts, o.AppBaseURL),
		Empty: facts.Empty()}, nil
}

// CanEmail reports why userID can't be emailed the report, if they can't.
func (o *OnDemand) CanEmail(ctx context.Context, userID string) error {
	if o.Send == nil {
		return errors.New("weekly report: no email sender")
	}
	out, err := o.Store.OptedOut(ctx, userID)
	if err != nil {
		return err
	}
	if out {
		return ErrOptedOut
	}
	return nil
}

// EmailTo builds the week's report and queues it to userID alone, once per
// week and member: asking again the same week sends nothing more. Its key
// is apart from the Monday email's, so neither suppresses the other.
func (o *OnDemand) EmailTo(ctx context.Context, userID, date string) (*Week, error) {
	if err := o.CanEmail(ctx, userID); err != nil {
		return nil, err
	}
	wk, err := o.Build(ctx, date)
	if err != nil {
		return nil, err
	}
	tenantID, _ := tenantctx.From(ctx)
	key := "weekly_report_on_demand:" + tenantID + ":" + wk.Start + ":" + userID
	if o.Sent != nil {
		sent, err := o.Sent(ctx, key)
		if err != nil {
			return nil, err
		}
		if sent {
			return wk, ErrAlreadySent
		}
	}
	if err := o.Send(ctx, userID, tenantID, key, wk.View); err != nil {
		return nil, err
	}
	return wk, nil
}

// weekStart is the Monday of the week holding date in loc, as a UTC
// calendar date the way WeekOf reads it; for an empty date, the Monday of
// the week before the one now is in.
func weekStart(date string, now time.Time, loc *time.Location) (time.Time, error) {
	day := now.In(loc).AddDate(0, 0, -7)
	if date != "" {
		d, err := time.ParseInLocation(time.DateOnly, date, loc)
		if err != nil {
			return time.Time{}, ErrInvalidDate
		}
		day = d
	}
	back := (int(day.Weekday()) + 6) % 7 // days since Monday
	y, m, d := day.AddDate(0, 0, -back).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
}
