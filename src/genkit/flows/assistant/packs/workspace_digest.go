package packs

import (
	"context"
	"errors"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/weeklyreport"
)

// Digester builds the weekly report on request, the one the Monday email
// sends, and emails it to the member asking.
type Digester interface {
	Build(ctx context.Context, date string) (*weeklyreport.Week, error)
	CanEmail(ctx context.Context, userID string) error
	EmailTo(ctx context.Context, userID, date string) (*weeklyreport.Week, error)
}

func digestSpecs(g *genkit.Genkit, d Deps) []assistantkit.Spec {
	if d.Digest == nil {
		return nil
	}
	show := genkit.DefineTool(g, "weeklyDigest",
		"Returns the weekly report for a finished Monday-to-Sunday week, the one owners get by email on Mondays: what was published and failed, how posts performed, campaign goals, ideas, the team, and what needs attention.",
		assistantkit.Wrap(recorded(workspacePackName, "weeklyDigest", nil, func(ctx context.Context, in DigestInput) (*DigestOutput, error) {
			return weeklyDigest(ctx, d, in)
		})), ai.WithStrictSchema(false))
	email := genkit.DefineTool(g, "emailWeeklyDigest",
		"Emails the weekly report for a finished week to the user asking, and no one else. Always offered for the user to confirm first.",
		assistantkit.Gated("emailWeeklyDigest", assistantkit.Gate[DigestInput]{
			Precheck: func(ctx context.Context, in DigestInput) error {
				if err := checkDigestDate(in.Week); err != nil {
					return err
				}
				return digestErr(d.Digest.CanEmail(ctx, actor(ctx)))
			},
			Summary: func(in DigestInput) string {
				if in.Week == "" {
					return "Email last week's report to you"
				}
				return "Email you the report for the week of " + in.Week
			},
		}, assistantkit.Wrap(recorded(workspacePackName, "emailWeeklyDigest", nil, func(ctx context.Context, in DigestInput) (*EmailDigestOutput, error) {
			return emailWeeklyDigest(ctx, d, in)
		}))), ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(workspacePackName, show, assistantkit.TierRead,
			`Call this when the user asks for the weekly report or digest, or a full picture of a past week — "show me last week's report", "how did the week of March 2 go?". week is any YYYY-MM-DD in the week; default last week. The week must be over: for the week so far, use activityReport with week "this".`),
		spec(workspacePackName, email, assistantkit.TierProposal,
			`Call this when the user asks to have the weekly report emailed or sent to them — "email me last week's report". It goes to the user asking only, once per week; there's no sending it to other people. Always offered first.`),
	}
}

// DigestInput is the week of the report.
type DigestInput struct {
	Week string `json:"week,omitempty" jsonschema:"description=Any YYYY-MM-DD in the week; default last week"`
}

// DigestOutput is a week's report.
type DigestOutput struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Empty is a week with nothing worth the Monday email; the report still
	// says so.
	Empty  bool                    `json:"empty"`
	Report models.WeeklyReportView `json:"report"`
}

// EmailDigestOutput says the report is on its way.
type EmailDigestOutput struct {
	Done string `json:"done"`
	From string `json:"from"`
	To   string `json:"to"`
}

func weeklyDigest(ctx context.Context, d Deps, in DigestInput) (*DigestOutput, error) {
	if err := checkDigestDate(in.Week); err != nil {
		return nil, err
	}
	wk, err := d.Digest.Build(ctx, in.Week)
	if err != nil {
		return nil, digestErr(err)
	}
	return &DigestOutput{From: wk.Start, To: wk.End, Empty: wk.Empty, Report: wk.View}, nil
}

func emailWeeklyDigest(ctx context.Context, d Deps, in DigestInput) (*EmailDigestOutput, error) {
	wk, err := d.Digest.EmailTo(ctx, actor(ctx), in.Week)
	if err != nil {
		return nil, digestErr(err)
	}
	return &EmailDigestOutput{Done: "the report for " + wk.Start + " to " + wk.End + " is on its way to your inbox", From: wk.Start, To: wk.End}, nil
}

func checkDigestDate(week string) error {
	if week == "" {
		return nil
	}
	if _, err := time.Parse(time.DateOnly, week); err != nil {
		return assistantkit.Refused("week is a YYYY-MM-DD in the week")
	}
	return nil
}

// digestErr turns what the report refuses into a refusal the model relays.
func digestErr(err error) error {
	switch {
	case errors.Is(err, weeklyreport.ErrWeekNotOver):
		return assistantkit.Refused("that week isn't over yet; the report covers finished weeks, so use activityReport for the week so far")
	case errors.Is(err, weeklyreport.ErrInvalidDate):
		return assistantkit.Refused("week is a YYYY-MM-DD in the week")
	case errors.Is(err, weeklyreport.ErrOptedOut):
		return assistantkit.Refused("you've turned the weekly report email off in your profile; turn it back on there, or read the report here with weeklyDigest")
	case errors.Is(err, weeklyreport.ErrAlreadySent):
		return assistantkit.Refused("that week's report was already emailed to you on request; it's in your inbox")
	}
	return err
}
