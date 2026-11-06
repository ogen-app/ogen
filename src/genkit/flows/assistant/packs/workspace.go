package packs

import (
	"context"
	"errors"
	"time"

	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

const workspacePackName = "workspace"

// workspacePack answers what happened day by day and how the workspace is
// set up: its timezone and the health of its connected accounts.
func workspacePack(d Deps) assistant.Pack {
	return assistant.Pack{
		Name:    workspacePackName,
		Summary: "say what happened on a day (published, failed, created) and check the workspace's setup: its timezone and whether its connected accounts need reconnecting",
		Screens: []viewcontext.Screen{"activity", "campaign.activity", "settings", "profile"},
		Define: func(g *genkit.Genkit) ([]assistantkit.Spec, error) {
			day := genkit.DefineTool(g, "activityReport",
				"Returns what happened on a day in the workspace's timezone: posts published and failed, and posts and campaigns created.",
				assistantkit.Wrap(func(ctx context.Context, in ActivityInput) (*report.Report, error) { return activityReport(ctx, d, in) }))
			setup := genkit.DefineTool(g, "workspaceSetup",
				"Returns the workspace's timezone and its connected social accounts with their health: valid, expiring, or needing a reconnect.",
				assistantkit.Wrap(func(ctx context.Context, _ struct{}) (*SetupOut, error) { return workspaceSetup(ctx, d) }))
			return []assistantkit.Spec{
				spec(workspacePackName, day, assistantkit.TierRead,
					`Call this when the user asks what happened — "what went out yesterday?", "did anything fail today?". date is YYYY-MM-DD, default today; campaignId narrows to one campaign (default the thread's).`),
				spec(workspacePackName, setup, assistantkit.TierRead,
					`Call this when the user asks about their setup or a publishing problem that may be an account — "why didn't my LinkedIn post go out?", "are my accounts connected?". If an account needs reconnecting, say so and navigate to settings.`),
			}, nil
		},
	}
}

// ActivityInput is a day of the activity log.
type ActivityInput struct {
	Date       string `json:"date,omitempty"       jsonschema:"description=YYYY-MM-DD; default today"`
	CampaignID string `json:"campaignId,omitempty" jsonschema:"description=Defaults to the thread's campaign"`
}

func activityReport(ctx context.Context, d Deps, in ActivityInput) (*report.Report, error) {
	_, tz := settings.WorkspaceTimezone(ctx, d.Settings)
	if tz == "" || tz == "Local" {
		tz = "UTC"
	}
	date := in.Date
	if date == "" {
		loc, _ := time.LoadLocation(tz)
		date = time.Now().In(loc).Format(time.DateOnly)
	}
	campaign := in.CampaignID
	if campaign == "" {
		campaign = assistant.CampaignID(ctx)
	}
	r, err := d.Activity.Report(ctx, date, tz, campaign)
	switch {
	case errors.Is(err, report.ErrCampaignNotFound):
		return nil, assistantkit.Refused("that campaign doesn't exist")
	case err != nil:
		return nil, assistantkit.Refused("couldn't read that day: " + err.Error())
	}
	return r, nil
}

// SetupOut is the workspace's setup.
type SetupOut struct {
	Timezone string       `json:"timezone"`
	Accounts []AccountOut `json:"accounts"`
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
}

func workspaceSetup(ctx context.Context, d Deps) (*SetupOut, error) {
	_, tz := settings.WorkspaceTimezone(ctx, d.Settings)
	out := &SetupOut{Timezone: tz, Accounts: []AccountOut{}}
	if d.SocialAccounts == nil || d.ProfileID == nil {
		out.Note = "connected accounts can't be read here"
		return out, nil
	}
	profile, err := d.ProfileID(ctx)
	if err != nil || profile == "" {
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
			Health: a.HealthStatus, TokenExpiresAt: a.TokenExpiresAt,
		})
	}
	return out, nil
}
