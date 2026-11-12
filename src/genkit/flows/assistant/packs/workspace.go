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
				assistantkit.Wrap(recorded(workspacePackName, "activityReport", nil, func(ctx context.Context, in ActivityInput) (*ActivityOutput, error) {
					return activityReport(ctx, d, in)
				})), ai.WithStrictSchema(false))
			setup := genkit.DefineTool(g, "workspaceSetup",
				"Returns the workspace's timezone, the platforms it auto-publishes to, and its connected social accounts with their health: valid, expiring, or needing a reconnect.",
				assistantkit.Wrap(recorded(workspacePackName, "workspaceSetup", nil, func(ctx context.Context, _ struct{}) (*SetupOut, error) {
					return workspaceSetup(ctx, d)
				})), ai.WithStrictSchema(false))
			specs := []assistantkit.Spec{
				spec(workspacePackName, day, assistantkit.TierRead,
					`Call this when the user asks what happened — "what went out yesterday?", "did anything fail today?", "how was last week?". Pass date (YYYY-MM-DD, default today) for one day, week ("this", "last", or any date in the week) for a Monday-to-Sunday week, or from and to for up to 31 days. campaignId narrows to one campaign (default the thread's). Name a failed post by its title and say why it failed; when nothing went out, say when the next post goes (next_scheduled). For the full weekly report, with performance and campaigns, use weeklyDigest.`),
				spec(workspacePackName, setup, assistantkit.TierRead,
					`Call this when the user asks about their setup, auto-publishing or a publishing problem that may be an account — "why didn't my LinkedIn post go out?", "are my accounts connected?", "does Instagram publish on its own?". If an account needs reconnecting, say so, since when, and how many scheduled posts wait on it (waitingPosts), and offer openConnect for it.`),
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

// ActivityOutput is the range's report, with what the card says beside it:
// the failed posts' titles, the ideas decided, the next post to go out and
// the campaign it was narrowed to.
type ActivityOutput struct {
	*report.RangeReport
	IdeasDecided  int                `json:"ideas_decided"`
	NextScheduled *NextPost          `json:"next_scheduled,omitempty"`
	Campaign      *CampaignName      `json:"campaign,omitempty"`
	Step          *assistantkit.Step `json:"step,omitempty"`
}

// NextPost is the next post waiting to go out.
type NextPost struct {
	PostID     string    `json:"post_id"`
	Title      string    `json:"title"`
	PlatformID string    `json:"platform_id"`
	At         time.Time `json:"at"`
}

// CampaignName names a campaign.
type CampaignName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func activityReport(ctx context.Context, d Deps, in ActivityInput) (*ActivityOutput, error) {
	r, campaign, err := activityRange(ctx, d, in)
	if err != nil {
		return nil, err
	}
	out := &ActivityOutput{RangeReport: r}
	if err := activityExtras(ctx, d, out, campaign, time.Now()); err != nil {
		return nil, err
	}
	return out, nil
}

// activityExtras adds what the card says beside the report.
func activityExtras(ctx context.Context, d Deps, out *ActivityOutput, campaign string, now time.Time) error {
	if ids := failedIDs(out.Failed.Posts); len(ids) > 0 && d.Titles != nil {
		titles, err := d.Titles.Titles(ctx, viewcontext.KindPost, ids)
		if err != nil {
			return fmt.Errorf("read failed posts' titles: %w", err)
		}
		for i := range out.Failed.Posts {
			out.Failed.Posts[i].Title = titles[out.Failed.Posts[i].PostID]
		}
	}
	loc, err := time.LoadLocation(out.TZ)
	if err != nil {
		loc = time.UTC
	}
	from, ferr := time.ParseInLocation(time.DateOnly, out.From, loc)
	to, terr := time.ParseInLocation(time.DateOnly, out.To, loc)
	if ferr == nil && terr == nil && d.IdeaRepo != nil {
		if out.IdeasDecided, err = d.IdeaRepo.CountDecided(ctx, from, to.AddDate(0, 0, 1), campaign); err != nil {
			return fmt.Errorf("count ideas decided: %w", err)
		}
	}
	if d.Posts != nil {
		next, err := d.Posts.NextScheduled(ctx, now, campaign)
		if err != nil {
			return fmt.Errorf("read the next scheduled post: %w", err)
		}
		if next != nil && next.ScheduledAt != nil {
			out.NextScheduled = &NextPost{PostID: next.ID, Title: next.Title, PlatformID: next.PlatformID, At: *next.ScheduledAt}
		}
	}
	if campaign != "" && d.Campaigns != nil {
		if c, err := d.Campaigns.GetByID(ctx, campaign); err == nil {
			out.Campaign = &CampaignName{ID: c.ID, Name: c.Name}
		}
	}
	days := 1
	if ferr == nil && terr == nil {
		days = int(to.Sub(from).Hours()/24) + 1
	}
	out.Step = &assistantkit.Step{Subject: activitySubject(out.From, out.To, days)}
	return nil
}

func failedIDs(posts []report.FailedPost) []string {
	ids := make([]string, 0, len(posts))
	for _, p := range posts {
		ids = append(ids, p.PostID)
	}
	return ids
}

// activitySubject is the range the step read: a day, or its first and last.
func activitySubject(from, to string, days int) string {
	if days == 1 {
		return from
	}
	return from + " – " + to
}

// activityRange is the report in asks for, and the campaign it is narrowed
// to.
func activityRange(ctx context.Context, d Deps, in ActivityInput) (*report.RangeReport, string, error) {
	tz, loc := workspaceZone(ctx, d)
	from, to, err := activityDays(in, time.Now().In(loc))
	if err != nil {
		return nil, "", err
	}
	campaign := in.CampaignID
	if campaign == "" {
		campaign = assistant.CampaignID(ctx)
	}
	r, err := d.Activity.Range(ctx, from, to, tz, campaign)
	switch {
	case errors.Is(err, report.ErrCampaignNotFound):
		return nil, "", assistantkit.Refused("that campaign doesn't exist")
	case errors.Is(err, report.ErrRangeTooLong):
		return nil, "", assistantkit.Refused(fmt.Sprintf("that's more than %d days; ask for a shorter range", report.MaxRangeDays))
	case errors.Is(err, report.ErrInvalidDate):
		return nil, "", assistantkit.Refused("dates are YYYY-MM-DD, and a range ends on or after its start")
	case err != nil:
		return nil, "", assistantkit.Refused("couldn't read that activity: " + err.Error())
	}
	return r, campaign, nil
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
	// UnhealthySince is when the account stopped working, while it isn't.
	UnhealthySince *time.Time `json:"unhealthySince,omitempty"`
	// WaitingPosts counts the scheduled posts that go out through it: the
	// ones that name it, and those that name no account when it is its
	// platform's only one.
	WaitingPosts int `json:"waitingPosts"`
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
			UnhealthySince: a.UnhealthySince,
		})
	}
	if err := countWaiting(ctx, d, out.Accounts); err != nil {
		return nil, err
	}
	return out, nil
}

// countWaiting sets each account's waiting posts: those naming it, and
// those naming none on a platform it is the only account of, as publishing
// picks it for them.
func countWaiting(ctx context.Context, d Deps, accounts []AccountOut) error {
	if d.Posts == nil || len(accounts) == 0 {
		return nil
	}
	counts, err := d.Posts.ScheduledCounts(ctx)
	if err != nil {
		return fmt.Errorf("count scheduled posts: %w", err)
	}
	perPlatform := map[string]int{}
	for _, a := range accounts {
		perPlatform[a.Platform]++
	}
	for i := range accounts {
		a := &accounts[i]
		for _, c := range counts {
			if c.AccountID == a.ID || (c.AccountID == "" && perPlatform[a.Platform] == 1 && platformSlug(c.PlatformID) == a.Platform) {
				a.WaitingPosts += c.Count
			}
		}
	}
	return nil
}
