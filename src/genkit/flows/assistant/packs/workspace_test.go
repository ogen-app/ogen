package packs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/weeklyreport"
)

// A day, a week (this, last, or one holding a date) or a range; nothing is
// today.
func TestActivityDays(t *testing.T) {
	today := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC) // a Wednesday
	cases := []struct {
		in       ActivityInput
		from, to string
	}{
		{ActivityInput{}, "2026-11-11", "2026-11-11"},
		{ActivityInput{Date: "2026-11-01"}, "2026-11-01", "2026-11-01"},
		{ActivityInput{Week: "this"}, "2026-11-09", "2026-11-15"},
		{ActivityInput{Week: "Last"}, "2026-11-02", "2026-11-08"},
		{ActivityInput{Week: "2026-11-01"}, "2026-10-26", "2026-11-01"},
		{ActivityInput{From: "2026-11-01", To: "2026-11-05"}, "2026-11-01", "2026-11-05"},
		{ActivityInput{From: "2026-11-01"}, "2026-11-01", "2026-11-11"},
	}
	for _, c := range cases {
		from, to, err := activityDays(c.in, today)
		if err != nil || from != c.from || to != c.to {
			t.Fatalf("activityDays(%+v) = %s..%s, %v; want %s..%s", c.in, from, to, err, c.from, c.to)
		}
	}
	_, _, err := activityDays(ActivityInput{Week: "next"}, today)
	refused(t, err)
}

type memAllowlist struct {
	repository.AutoPublishAllowlistRepository
	list []string
}

func (m *memAllowlist) List(context.Context) ([]string, error) { return slices.Clone(m.list), nil }
func (m *memAllowlist) ReplaceIf(_ context.Context, _ bun.IDB, from, to []string) (bool, error) {
	return m.replaceIf(from, to), nil
}

func (m *memAllowlist) replaceIf(from, to []string) bool {
	if !slices.Equal(slices.Sorted(slices.Values(m.list)), slices.Sorted(slices.Values(from))) {
		return false
	}
	m.list = slices.Clone(to)
	return true
}

func platformsKnown(slug string) (string, bool) {
	label, ok := map[string]string{"linkedin": "LinkedIn", "x": "X", "instagram": "Instagram"}[slug]
	return label, ok
}

// The setup says which platforms auto-publish and marks the accounts on
// them.
func TestWorkspaceSetup_AutoPublish(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	out, err := workspaceSetup(ctx, Deps{Settings: memSettings{}, SocialAccounts: memAccounts{}, AutoPublish: &memAllowlist{list: []string{"linkedin"}},
		ProfileID: func(context.Context) (string, error) { return "prof", nil }})
	if err != nil || !slices.Equal(out.AutoPublish, []string{"linkedin"}) || out.AutoPublishNote == "" || !out.Accounts[0].AutoPublish {
		t.Fatalf("setup = %+v, %v", out, err)
	}
}

// Turning a platform on or off changes only it, can be undone, and is
// refused for an unknown platform or a change that changes nothing.
func TestSetAutoPublish(t *testing.T) {
	ctx, touched := turn(t, workspace, nil)
	allow := &memAllowlist{list: []string{"x"}}
	actions := &memActions{}
	d := Deps{AutoPublish: allow, ConnectPlatform: platformsKnown, Actions: actions}

	out, err := setAutoPublish(ctx, d, AutoPublishInput{Platform: " LinkedIn ", On: true})
	if err != nil || !slices.Equal(allow.list, []string{"linkedin", "x"}) || out.UndoID == "" || !strings.Contains(out.Done, "LinkedIn") {
		t.Fatalf("on = %+v, %v; list %v", out, err, allow.list)
	}
	if len(actions.rows) != 1 || actions.rows[0].Kind != models.AssistantActionAutoPublish || len(touched()) != 1 {
		t.Fatalf("undo log = %+v, touched %v", actions.rows, touched())
	}
	if _, err := setAutoPublish(ctx, d, AutoPublishInput{Platform: "x"}); err != nil || !slices.Equal(allow.list, []string{"linkedin"}) {
		t.Fatalf("off = %v; list %v", err, allow.list)
	}
	_, err = setAutoPublish(ctx, d, AutoPublishInput{Platform: "linkedin", On: true})
	refused(t, err)
	_, err = setAutoPublish(ctx, d, AutoPublishInput{Platform: "myspace", On: true})
	refused(t, err)
}

type memVersions struct {
	repository.PostVersionRepository
	list []models.PostVersion
}

func (m memVersions) ListByPostID(context.Context, string) ([]models.PostVersion, error) {
	return m.list, nil
}

type memPostLogs struct {
	repository.PostLogRepository
	list []models.PostLog
}

func (m memPostLogs) ListFiltered(context.Context, repository.PostLogFilter) ([]models.PostLog, error) {
	return m.list, nil
}

type memUsers struct{ repository.UserRepository }

func (memUsers) List(context.Context) ([]models.User, error) {
	return []models.User{{ID: "u1", Name: "Olena"}, {ID: "u2", Email: "max@acme.test"}}, nil
}

// The history merges versions and the log's changes, newest first, by
// name; machinery events are left out, and older versions say no one was
// recorded.
func TestPostHistory(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 3, h, 0, 0, 0, time.UTC) }
	draft, ready := models.PostStatusDraft, models.PostStatusReadyForPublish
	d := Deps{
		Posts: examplePosts{byID: map[string]*models.Post{"p1": {ID: "p1", Title: "Launch"}}},
		Versions: memVersions{list: []models.PostVersion{
			{VersionNumber: 1, CreatedAt: at(9), Creator: models.PostVersionCreatorUser, Note: "Initial version"},
			{VersionNumber: 2, CreatedAt: at(11), Creator: models.PostVersionCreatorAssistant, CreatedBy: "u2", Note: "Tighter"},
		}},
		PostLogs: memPostLogs{list: []models.PostLog{
			{EventTimestamp: at(12), EventType: models.PostLogEventUserSchedule, Actor: "u1"},
			{EventTimestamp: at(12), EventType: models.PostLogEventZernioPoll, Actor: models.ActorSystem},
			{EventTimestamp: at(10), EventType: models.PostLogEventStateTransition, Actor: "gone", FromStatus: &draft, ToStatus: &ready},
		}},
		Users: memUsers{},
	}
	ctx, _ := turn(t, viewcontext.Scope{Kind: models.AssistantScopePost, ID: "p1"}, nil)
	out, err := postHistory(ctx, d, PostHistoryInput{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range out.Entries {
		got = append(got, e.What+" by "+e.By)
	}
	want := []string{"scheduled by Olena", "Tighter by the assistant, for max@acme.test", "status draft → ready_for_publish by a former member",
		"Initial version by a person (not recorded)"}
	if !slices.Equal(got, want) || out.Title != "Launch" || out.Note == "" {
		t.Fatalf("history = %q, note %q", got, out.Note)
	}
	if out, _ := postHistory(ctx, d, PostHistoryInput{Limit: 2}); len(out.Entries) != 2 || !out.More {
		t.Fatalf("limited = %+v", out)
	}
	none, _ := turn(t, workspace, nil)
	_, err = postHistory(none, d, PostHistoryInput{})
	refused(t, err)
	_, err = postHistory(none, d, PostHistoryInput{PostID: "nope"})
	refused(t, err)
}

type memDigest struct {
	optedOut bool
	emailed  []string
}

func (m *memDigest) Build(_ context.Context, date string) (*weeklyreport.Week, error) {
	if date == "2026-11-10" {
		return nil, weeklyreport.ErrWeekNotOver
	}
	return &weeklyreport.Week{Start: "2026-11-02", End: "2026-11-08", View: models.WeeklyReportView{Headline: "3 posts went out last week."}}, nil
}

func (m *memDigest) CanEmail(context.Context, string) error {
	if m.optedOut {
		return weeklyreport.ErrOptedOut
	}
	return nil
}

func (m *memDigest) EmailTo(ctx context.Context, userID, date string) (*weeklyreport.Week, error) {
	m.emailed = append(m.emailed, userID)
	return m.Build(ctx, date)
}

// The digest shows a finished week and emails it to the one asking; the
// week now in, a bad date and an opted-out member are refused.
func TestWeeklyDigest(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	dg := &memDigest{}
	d := Deps{Digest: dg}
	out, err := weeklyDigest(ctx, d, DigestInput{})
	if err != nil || out.From != "2026-11-02" || out.Report.Headline == "" {
		t.Fatalf("digest = %+v, %v", out, err)
	}
	_, err = weeklyDigest(ctx, d, DigestInput{Week: "2026-11-10"})
	refused(t, err)
	_, err = weeklyDigest(ctx, d, DigestInput{Week: "last monday"})
	refused(t, err)
	sent, err := emailWeeklyDigest(ctx, d, DigestInput{})
	if err != nil || !slices.Equal(dg.emailed, []string{"u1"}) || !strings.Contains(sent.Done, "2026-11-02") {
		t.Fatalf("email = %+v, %v; to %v", sent, err, dg.emailed)
	}
	dg.optedOut = true
	refused(t, digestErr(dg.CanEmail(ctx, "u1")))
	if err := digestErr(errors.New("db down")); err == nil {
		t.Fatal("a read failure is lost")
	}
}

// openConnect reconnects a connected account or connects a platform;
// anything else is refused.
func TestOpenConnect(t *testing.T) {
	d := Deps{ConnectPlatform: platformsKnown, Settings: memSettings{}, SocialAccounts: memAccounts{},
		ProfileID: func(context.Context) (string, error) { return "prof", nil }}
	ctx, _ := turn(t, workspace, nil)
	if out, err := openConnect(ctx, d, ConnectInput{AccountID: "sa1"}); err != nil || !strings.Contains(out.Done, "reconnect the LinkedIn") {
		t.Fatalf("reconnect = %+v, %v", out, err)
	}
	if out, err := openConnect(ctx, d, ConnectInput{Platform: "Instagram"}); err != nil || !strings.Contains(out.Done, "connect an account on Instagram") {
		t.Fatalf("connect = %+v, %v", out, err)
	}
	for _, in := range []ConnectInput{{}, {AccountID: "someone-elses"}, {Platform: "myspace"}} {
		_, err := openConnect(ctx, d, in)
		refused(t, err)
	}
}
