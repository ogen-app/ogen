package packs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

const workspacePackName = "workspace"

// workspacePack answers what happened and how the workspace is set up: its
// activity by day or week, the weekly report, a post's change history, its
// timezone, auto-publishing and connected accounts.
func workspacePack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name: workspacePackName,
		Summary: "say what happened on a day or over a week (published, failed, created), show or email the weekly report, say who changed a post and when, " +
			"and check the workspace's setup: its timezone, which platforms auto-publish, and whether its connected accounts need reconnecting, opening the connect flow when one does",
		Screens: []viewcontext.Screen{"activity", "campaign.activity", "settings", "campaign.settings", "profile", "post"},
		Begin:   beginResults(workspacePackName),
		End:     endResults(workspacePackName),
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			day := genkit.DefineTool(g, "activityReport",
				"Returns what happened on a day, a week or a range of days in the workspace's timezone: posts published and failed, and posts and campaigns created, with each day's totals.",
				assistantkit.Wrap(recorded(workspacePackName, "activityReport", nil, func(ctx context.Context, in ActivityInput) (*report.RangeReport, error) {
					return activityReport(ctx, d, in)
				})), ai.WithStrictSchema(false))
			setup := genkit.DefineTool(g, "workspaceSetup",
				"Returns the workspace's timezone, the platforms it auto-publishes to, and its connected social accounts with their health: valid, expiring, or needing a reconnect.",
				assistantkit.Wrap(recorded(workspacePackName, "workspaceSetup", nil, func(ctx context.Context, _ struct{}) (*SetupOut, error) {
					return workspaceSetup(ctx, d)
				})), ai.WithStrictSchema(false))
			specs := []assistantkit.Spec{
				spec(workspacePackName, day, assistantkit.TierRead,
					`Call this when the user asks what happened — "what went out yesterday?", "did anything fail today?", "how was last week?". Pass date (YYYY-MM-DD, default today) for one day, week ("this", "last", or any date in the week) for a Monday-to-Sunday week, or from and to for up to 31 days. campaignId narrows to one campaign (default the thread's). For the full weekly report, with performance and campaigns, use weeklyDigest.`),
				spec(workspacePackName, setup, assistantkit.TierRead,
					`Call this when the user asks about their setup, auto-publishing or a publishing problem that may be an account — "why didn't my LinkedIn post go out?", "are my accounts connected?", "does Instagram publish on its own?". If an account needs reconnecting, say so and offer openConnect for it.`),
			}
			specs = append(specs, digestSpecs(g, d)...)
			specs = append(specs, historySpecs(g, d)...)
			specs = append(specs, autoPublishSpecs(g, d)...)
			specs = append(specs, connectSpecs(g, d)...)
			return specs, nil
		},
	}
}

// ActivityInput is a day, a week or a range of the activity log.
type ActivityInput struct {
	Date       string `json:"date,omitempty"       jsonschema:"description=One day, YYYY-MM-DD; default today"`
	Week       string `json:"week,omitempty"       jsonschema:"description=A Monday-to-Sunday week: this, last, or any YYYY-MM-DD in it"`
	From       string `json:"from,omitempty"       jsonschema:"description=First day of a range, YYYY-MM-DD"`
	To         string `json:"to,omitempty"         jsonschema:"description=Last day of a range, YYYY-MM-DD; at most 31 days after from"`
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
}

func activityReport(ctx context.Context, d Deps, in ActivityInput) (*report.RangeReport, error) {
	tz, loc := workspaceZone(ctx, d)
	from, to, err := activityDays(in, time.Now().In(loc))
	if err != nil {
		return nil, err
	}
	campaign := in.CampaignID
	if campaign == "" {
		campaign = assistant.CampaignID(ctx)
	}
	r, err := d.Activity.Range(ctx, from, to, tz, campaign)
	switch {
	case errors.Is(err, report.ErrCampaignNotFound):
		return nil, assistantkit.Refused("that campaign doesn't exist")
	case errors.Is(err, report.ErrRangeTooLong):
		return nil, assistantkit.Refused(fmt.Sprintf("that's more than %d days; ask for a shorter range", report.MaxRangeDays))
	case errors.Is(err, report.ErrInvalidDate):
		return nil, assistantkit.Refused("dates are YYYY-MM-DD, and a range ends on or after its start")
	case err != nil:
		return nil, assistantkit.Refused("couldn't read that activity: " + err.Error())
	}
	return r, nil
}

// workspaceZone is the workspace's timezone, UTC when it has none.
func workspaceZone(ctx context.Context, d Deps) (string, *time.Location) {
	_, tz := settings.WorkspaceTimezone(ctx, d.Settings)
	loc, err := time.LoadLocation(tz)
	if tz == "" || tz == "Local" || err != nil {
		return "UTC", time.UTC
	}
	return tz, loc
}

// activityDays is the first and last day in asks for, on today's calendar:
// a range, a week, or one day.
func activityDays(in ActivityInput, today time.Time) (from, to string, err error) {
	switch {
	case in.From != "" || in.To != "":
		from, to = in.From, in.To
		if from == "" {
			from = to
		}
		if to == "" {
			to = today.Format(time.DateOnly)
		}
		return from, to, nil
	case in.Week != "":
		day := today
		switch strings.ToLower(in.Week) {
		case "this":
		case "last":
			day = today.AddDate(0, 0, -7)
		default:
			d, err := time.ParseInLocation(time.DateOnly, in.Week, today.Location())
			if err != nil {
				return "", "", assistantkit.Refused(`week is "this", "last", or a YYYY-MM-DD in it`)
			}
			day = d
		}
		monday := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
		return monday.Format(time.DateOnly), monday.AddDate(0, 0, 6).Format(time.DateOnly), nil
	case in.Date != "":
		return in.Date, in.Date, nil
	}
	return today.Format(time.DateOnly), today.Format(time.DateOnly), nil
}

// SetupOut is the workspace's setup.
type SetupOut struct {
	Timezone string `json:"timezone"`
	// AutoPublish is the platforms whose scheduled posts go out on their
	// own; nil when it can't be read here.
	AutoPublish     []string     `json:"autoPublish"`
	AutoPublishNote string       `json:"autoPublishNote,omitempty"`
	Accounts        []AccountOut `json:"accounts"`
	// Note says why accounts are missing, when they are.
	Note string `json:"note,omitempty"`
}

// AccountOut is a connected account's health.
type AccountOut struct {
	ID             string     `json:"id"`
	Platform       string     `json:"platform"`
	Username       string     `json:"username"`
	NeedsReconnect bool       `json:"needsReconnect"`
	Health         string     `json:"health,omitempty"`
	TokenExpiresAt *time.Time `json:"tokenExpiresAt,omitempty"`
	AutoPublish    bool       `json:"autoPublish"`
}

const autoPublishNote = "A post scheduled on an auto-publish platform goes out on its own at its time; on any other platform it waits for someone to publish it by hand."

func workspaceSetup(ctx context.Context, d Deps) (*SetupOut, error) {
	_, tz := settings.WorkspaceTimezone(ctx, d.Settings)
	out := &SetupOut{Timezone: tz, Accounts: []AccountOut{}}
	if d.AutoPublish != nil {
		list, err := d.AutoPublish.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("read auto-publish platforms: %w", err)
		}
		out.AutoPublish, out.AutoPublishNote = append([]string{}, list...), autoPublishNote
	}
	if d.SocialAccounts == nil || d.ProfileID == nil {
		out.Note = "connected accounts can't be read here"
		return out, nil
	}
	profile, err := d.ProfileID(ctx)
	if err != nil {
		return nil, fmt.Errorf("read publishing profile: %w", err)
	}
	if profile == "" {
		out.Note = "no publishing profile is connected yet"
		return out, nil
	}
	accounts, err := d.SocialAccounts.ListActive(ctx, profile)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		out.Accounts = append(out.Accounts, AccountOut{
			ID: a.ID, Platform: a.Platform, Username: a.Username, NeedsReconnect: a.NeedsReconnect != nil && *a.NeedsReconnect,
			Health: a.HealthStatus, TokenExpiresAt: a.TokenExpiresAt, AutoPublish: slices.Contains(out.AutoPublish, a.Platform),
		})
	}
	return out, nil
}
