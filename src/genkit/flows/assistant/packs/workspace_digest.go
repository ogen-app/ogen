package packs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/firebase/genkit/go/ai"
	"github.com/firebase/genkit/go/genkit"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/usecase/weeklyreport"
)

// Digester builds the weekly report on request, the one the Monday email
// sends, and emails it to a member.
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
		"Emails the weekly report for a finished week to members of the workspace: the user asking, or the members named. Each gets it once per week; anyone who turned the email off is skipped, and only members can receive it. Offered for the user to confirm first.",
		assistantkit.Gated("emailWeeklyDigest", assistantkit.Gate[EmailDigestInput]{
			Precheck: func(ctx context.Context, in EmailDigestInput) error { return checkDigestEmail(ctx, d, in) },
			// Naming no member sends nothing: the result says why, without an
			// offer.
			Needs: func(ctx context.Context, in EmailDigestInput) bool {
				members, _, err := digestRecipients(ctx, d, in.Recipients)
				return err != nil || len(members) > 0
			},
			Describe: func(ctx context.Context, in EmailDigestInput) string { return digestSummary(ctx, d, in) },
			Card:     func(ctx context.Context, in EmailDigestInput) *assistantkit.Card { return digestCard(ctx, d, in) },
		}, assistantkit.Wrap(recorded(workspacePackName, "emailWeeklyDigest", nil, func(ctx context.Context, in EmailDigestInput) (*EmailDigestOutput, error) {
			return emailWeeklyDigest(ctx, d, in)
		}))), ai.WithStrictSchema(false))
	return []assistantkit.Spec{
		spec(workspacePackName, show, assistantkit.TierRead,
			`Call this when the user asks for the weekly report or digest, or a full picture of a past week — "show me last week's report", "how did the week of March 2 go?". week is any YYYY-MM-DD in the week; default last week. The week must be over: for the week so far, use activityReport with week "this".`),
		spec(workspacePackName, email, assistantkit.TierProposal,
			`Call this when the user asks to have the weekly report emailed — "email me last week's report", "send last week's report to Dan and Ann". recipients names the members it goes to, by name or email; leave it empty for the user asking. Only members can receive it: someone who isn't one is said in notSent, and the user can invite them from Settings › People. Each member gets a week once; one who turned the email off is skipped. Offered first.`),
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
	// Step is the week read, by its Monday.
	Step *assistantkit.Step `json:"step,omitempty"`
}

// maxDigestRecipients bounds the members one call emails.
const maxDigestRecipients = 10

// Why a recipient wasn't sent the report.
const (
	digestNotMember   = "not_member"
	digestOptedOut    = "opted_out"
	digestAlreadySent = "already_sent"
	digestFailed      = "failed"
)

// EmailDigestInput is the week to email and whom to.
type EmailDigestInput struct {
	Week       string   `json:"week,omitempty"       jsonschema:"description=Any YYYY-MM-DD in the week; default last week"`
	Recipients []string `json:"recipients,omitempty" jsonschema:"description=Members to email it to, by name or email; default the user asking"`
}

// DigestRecipient is a member the report went to.
type DigestRecipient struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Email  string `json:"email"`
}

// DigestSkip is a recipient the report didn't go to, and why: not_member,
// opted_out, already_sent or failed.
type DigestSkip struct {
	Recipient string `json:"recipient"`
	Reason    string `json:"reason"`
}

// EmailDigestOutput is who the report is on its way to, and who it isn't.
type EmailDigestOutput struct {
	Done    string            `json:"done"`
	From    string            `json:"from"`
	To      string            `json:"to"`
	Sent    []DigestRecipient `json:"sent"`
	NotSent []DigestSkip      `json:"notSent,omitempty"`
	// SentAt is when it was queued; absent when it went to no one.
	SentAt *time.Time         `json:"sentAt,omitempty"`
	Step   *assistantkit.Step `json:"step,omitempty"`
}

func weeklyDigest(ctx context.Context, d Deps, in DigestInput) (*DigestOutput, error) {
	if err := checkDigestDate(in.Week); err != nil {
		return nil, err
	}
	wk, err := d.Digest.Build(ctx, in.Week)
	if err != nil {
		return nil, digestErr(err)
	}
	return &DigestOutput{From: wk.Start, To: wk.End, Empty: wk.Empty, Report: wk.View, Step: &assistantkit.Step{Subject: wk.Start}}, nil
}

func emailWeeklyDigest(ctx context.Context, d Deps, in EmailDigestInput) (*EmailDigestOutput, error) {
	if err := checkDigestDate(in.Week); err != nil {
		return nil, err
	}
	members, outsiders, err := digestRecipients(ctx, d, in.Recipients)
	if err != nil {
		return nil, err
	}
	wk, err := d.Digest.Build(ctx, in.Week)
	if err != nil {
		return nil, digestErr(err)
	}
	out := &EmailDigestOutput{From: wk.Start, To: wk.End, Sent: []DigestRecipient{}}
	var failed error
	for _, o := range outsiders {
		out.NotSent = append(out.NotSent, DigestSkip{Recipient: o, Reason: digestNotMember})
	}
	for _, m := range members {
		// The week built above, so a batch crossing midnight on Monday
		// still sends the week it reports.
		_, err := d.Digest.EmailTo(ctx, m.ID, wk.Start)
		switch {
		case err == nil:
			out.Sent = append(out.Sent, DigestRecipient{UserID: m.ID, Name: cmp.Or(m.Name, m.Email), Role: m.Role, Email: m.Email})
		case errors.Is(err, weeklyreport.ErrOptedOut):
			out.NotSent = append(out.NotSent, DigestSkip{Recipient: cmp.Or(m.Name, m.Email), Reason: digestOptedOut})
		case errors.Is(err, weeklyreport.ErrAlreadySent):
			out.NotSent = append(out.NotSent, DigestSkip{Recipient: cmp.Or(m.Name, m.Email), Reason: digestAlreadySent})
		default:
			// The others still go; the result says who didn't get it.
			failed = cmp.Or(failed, err)
			out.NotSent = append(out.NotSent, DigestSkip{Recipient: cmp.Or(m.Name, m.Email), Reason: digestFailed})
		}
	}
	if failed != nil && len(out.Sent) == 0 {
		return nil, digestErr(failed)
	}
	out.Step = &assistantkit.Step{Subject: wk.Start}
	if len(out.Sent) == 0 {
		out.Done = "the report for " + wk.Start + " to " + wk.End + " went to no one; notSent says why"
		return out, nil
	}
	out.SentAt = new(time.Now().UTC())
	names := make([]string, len(out.Sent))
	for i, r := range out.Sent {
		names[i] = r.Name
	}
	out.Done = "the report for " + wk.Start + " to " + wk.End + " is on its way to " + strings.Join(names, ", ")
	return out, nil
}

// digestRecipients is the members named, by id, email or name, and the
// names that match no member; naming none is the user asking.
func digestRecipients(ctx context.Context, d Deps, named []string) (members []models.User, outsiders []string, err error) {
	if d.Users == nil {
		return nil, nil, assistantkit.Refused("the workspace's members can't be read here")
	}
	users, err := d.Users.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read members: %w", err)
	}
	if len(named) == 0 {
		named = []string{actor(ctx)}
	}
	if len(named) > maxDigestRecipients {
		return nil, nil, assistantkit.Refused(fmt.Sprintf("at most %d people at once", maxDigestRecipients))
	}
	seen := map[string]bool{}
	for _, n := range named {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		u, err := digestMember(users, n)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case u == nil:
			outsiders = append(outsiders, n)
		case !seen[u.ID]:
			seen[u.ID] = true
			members = append(members, *u)
		}
	}
	return members, outsiders, nil
}

// digestMember is the member n names: by id or email, else by a name only
// one member has. nil when none does; a name two members share is refused,
// so the report never goes to the wrong one.
func digestMember(users []models.User, n string) (*models.User, error) {
	if i := slices.IndexFunc(users, func(u models.User) bool { return u.ID == n || strings.EqualFold(u.Email, n) }); i >= 0 {
		return &users[i], nil
	}
	var match []models.User
	for _, u := range users {
		if u.Name != "" && strings.EqualFold(u.Name, n) {
			match = append(match, u)
		}
	}
	switch len(match) {
	case 0:
		return nil, nil
	case 1:
		return &match[0], nil
	}
	emails := make([]string, len(match))
	for i, u := range match {
		emails[i] = u.Email
	}
	return nil, assistantkit.Refused(fmt.Sprintf("%d members are called %s; say which by email", len(match), n), emails...)
}

// checkDigestEmail refuses a call no one could be emailed by: a bad week, or
// members who all turned the email off. Naming no member at all runs, to
// say so.
func checkDigestEmail(ctx context.Context, d Deps, in EmailDigestInput) error {
	if err := checkDigestDate(in.Week); err != nil {
		return err
	}
	members, _, err := digestRecipients(ctx, d, in.Recipients)
	if err != nil || len(members) == 0 {
		return err
	}
	var first error
	for _, m := range members {
		err := d.Digest.CanEmail(ctx, m.ID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, weeklyreport.ErrOptedOut) {
			return err
		}
		first = cmp.Or(first, err)
	}
	if len(members) == 1 && members[0].ID == actor(ctx) {
		return digestErr(first)
	}
	return assistantkit.Refused("none of them can receive it: they turned the weekly report email off in their profile")
}

// digestSummary is the offer's line.
func digestSummary(ctx context.Context, d Deps, in EmailDigestInput) string {
	week := "last week's report"
	if in.Week != "" {
		week = "the report for the week of " + in.Week
	}
	members, _, err := digestRecipients(ctx, d, in.Recipients)
	if err != nil || len(members) == 0 {
		return "Email " + week
	}
	if len(members) == 1 && members[0].ID == actor(ctx) {
		return "Email " + week + " to you"
	}
	return fmt.Sprintf("Email %s to %s", week, plural(len(members), "person", "people"))
}

// digestCard is the offer's card: each member it goes to, and each name
// that isn't one.
func digestCard(ctx context.Context, d Deps, in EmailDigestInput) *assistantkit.Card {
	members, outsiders, err := digestRecipients(ctx, d, in.Recipients)
	if err != nil || len(members) == 0 {
		return nil
	}
	c := &assistantkit.Card{Verb: "Email to " + plural(len(members), "person", "people"),
		Footnote: "Each gets the week once; anyone who turned the email off is skipped."}
	if len(members) == 1 && members[0].ID == actor(ctx) {
		c.Verb = "Email it to me"
	}
	for _, m := range members {
		c.Rows = append(c.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowRecipient, Value: cmp.Or(m.Name, m.Email), Note: m.Role + " · " + m.Email})
	}
	for _, o := range outsiders {
		c.Rows = append(c.Rows, assistantkit.CardRow{Kind: assistantkit.CardRowRecipient, Value: o, Note: "Not a member, so it won't go to them"})
	}
	return c
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
