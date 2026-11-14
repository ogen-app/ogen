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
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/activity/report"
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

// ListFiltered keeps the post log's rules: newest first, on or after
// Since, at most Limit.
func (m memPostLogs) ListFiltered(_ context.Context, f repository.PostLogFilter) ([]models.PostLog, error) {
	logs := slices.DeleteFunc(slices.Clone(m.list), func(l models.PostLog) bool {
		return !f.Since.IsZero() && l.EventTimestamp.Before(f.Since)
	})
	slices.SortStableFunc(logs, func(a, b models.PostLog) int { return b.EventTimestamp.Compare(a.EventTimestamp) })
	if f.Limit > 0 && len(logs) > f.Limit {
		logs = logs[:f.Limit]
	}
	return logs, nil
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
	// out are the members who turned the email off; optedOut is everyone.
	out     map[string]bool
	emailed []string
	// dates are the weeks EmailTo was asked for.
	dates []string
}

func (m *memDigest) Build(_ context.Context, date string) (*weeklyreport.Week, error) {
	if date == "2026-11-10" {
		return nil, weeklyreport.ErrWeekNotOver
	}
	return &weeklyreport.Week{Start: "2026-11-02", End: "2026-11-08", View: models.WeeklyReportView{Headline: "3 posts went out last week."}}, nil
}

func (m *memDigest) CanEmail(_ context.Context, userID string) error {
	if m.optedOut || m.out[userID] {
		return weeklyreport.ErrOptedOut
	}
	return nil
}

func (m *memDigest) EmailTo(ctx context.Context, userID, date string) (*weeklyreport.Week, error) {
	if err := m.CanEmail(ctx, userID); err != nil {
		return nil, err
	}
	m.emailed = append(m.emailed, userID)
	m.dates = append(m.dates, date)
	return m.Build(ctx, date)
}

// The digest shows a finished week and emails it to the one asking; the
// week now in, a bad date and an opted-out member are refused.
func TestWeeklyDigest(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	dg := &memDigest{}
	d := Deps{Digest: dg, Users: memUsers{}}
	out, err := weeklyDigest(ctx, d, DigestInput{})
	if err != nil || out.From != "2026-11-02" || out.Report.Headline == "" {
		t.Fatalf("digest = %+v, %v", out, err)
	}
	_, err = weeklyDigest(ctx, d, DigestInput{Week: "2026-11-10"})
	refused(t, err)
	_, err = weeklyDigest(ctx, d, DigestInput{Week: "last monday"})
	refused(t, err)
	sent, err := emailWeeklyDigest(ctx, d, EmailDigestInput{})
	if err != nil || !slices.Equal(dg.emailed, []string{"u1"}) || !strings.Contains(sent.Done, "2026-11-02") ||
		len(sent.Sent) != 1 || sent.Sent[0].Name != "Olena" || sent.SentAt == nil {
		t.Fatalf("email = %+v, %v; to %v", sent, err, dg.emailed)
	}
	dg.optedOut = true
	refused(t, digestErr(dg.CanEmail(ctx, "u1")))
	if err := digestErr(errors.New("db down")); err == nil {
		t.Fatal("a read failure is lost")
	}
}

// twoLinkedIns is a workspace with two LinkedIn accounts and one Instagram.
type twoLinkedIns struct {
	repository.SocialAccountRepository
}

func (twoLinkedIns) ListActive(context.Context, string) ([]models.SocialAccount, error) {
	since := time.Date(2026, 4, 16, 8, 0, 0, 0, time.UTC)
	return []models.SocialAccount{
		{ID: "sa1", Platform: "linkedin", Username: "acme", NeedsReconnect: new(true), UnhealthySince: &since},
		{ID: "sa2", Platform: "linkedin", Username: "dan"},
		{ID: "sa3", Platform: "instagram", Username: "acme.ig"},
	}, nil
}

// Each account says since when it stopped working and how many scheduled
// posts wait on it: those naming it, and those naming none on a platform it
// is the only account of. With two on a platform, those are no one's.
func TestWorkspaceSetup_Waiting(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	li, ig := zernio.LookupSqidByZernioID("linkedin"), zernio.LookupSqidByZernioID("instagram")
	posts := &waitingPosts{counts: []repository.ScheduledCount{
		{PlatformID: li, Status: models.PostStatusScheduled, Count: 5},
		{PlatformID: li, AccountID: "sa1", Status: models.PostStatusScheduled, Count: 2},
		{PlatformID: li, AccountID: "sa1", Status: models.PostStatusScheduledForManualPublish, Count: 1},
		{PlatformID: ig, Status: models.PostStatusScheduledForManualPublish, Count: 3},
	}}
	out, err := workspaceSetup(ctx, Deps{Settings: memSettings{}, SocialAccounts: twoLinkedIns{}, Posts: posts,
		ProfileID: func(context.Context) (string, error) { return "prof", nil }})
	if err != nil {
		t.Fatal(err)
	}
	var waiting []int
	for _, a := range out.Accounts {
		waiting = append(waiting, a.WaitingPosts)
	}
	if !slices.Equal(waiting, []int{3, 0, 3}) || out.Accounts[0].UnhealthySince == nil || out.Accounts[1].UnhealthySince != nil {
		t.Fatalf("accounts = %+v", out.Accounts)
	}
}

// A change says what changed and how many posts already scheduled the other
// way keep their route; the card says the same before the yes.
func TestSetAutoPublish_ScheduledUnaffected(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	li, x := zernio.LookupSqidByZernioID("linkedin"), zernio.LookupSqidByZernioID("twitter")
	if li == "" || x == "" {
		t.Fatal("the built-in catalog lacks linkedin or twitter")
	}
	posts := &waitingPosts{counts: []repository.ScheduledCount{
		{PlatformID: li, Status: models.PostStatusScheduledForManualPublish, Count: 2},
		{PlatformID: li, AccountID: "acc", Status: models.PostStatusScheduledForManualPublish, Count: 1},
		{PlatformID: li, Status: models.PostStatusScheduled, Count: 4},
		{PlatformID: x, Status: models.PostStatusScheduledForManualPublish, Count: 9},
	}}
	d := Deps{AutoPublish: &memAllowlist{}, ConnectPlatform: platformsKnown, Actions: &memActions{}, Posts: posts}

	card := autoPublishCard(ctx, d, AutoPublishInput{Platform: "linkedin", On: true})
	if card.Verb != "Turn on for LinkedIn" || !strings.Contains(card.Footnote, "the 3 posts already scheduled keep") {
		t.Fatalf("card = %+v", card)
	}
	out, err := setAutoPublish(ctx, d, AutoPublishInput{Platform: "linkedin", On: true})
	if err != nil || out.Platform != "linkedin" || out.PlatformLabel != "LinkedIn" || !out.On || out.ScheduledUnaffected != 3 ||
		!strings.Contains(out.Done, "scheduled from now on go out on their own; the 3 posts already scheduled keep") {
		t.Fatalf("on = %+v, %v", out, err)
	}
	// Turning it off leaves the ones going out on their own.
	out, err = setAutoPublish(ctx, d, AutoPublishInput{Platform: "linkedin"})
	if err != nil || out.On || out.ScheduledUnaffected != 4 {
		t.Fatalf("off = %+v, %v", out, err)
	}
}

// failingCounts can't count the scheduled posts.
type failingCounts struct{ repository.PostRepository }

func (failingCounts) ScheduledCounts(context.Context) ([]repository.ScheduledCount, error) {
	return nil, errors.New("db down")
}

// A count that fails stops the change before it is saved, so no change is
// left without its undo.
func TestSetAutoPublish_CountFailsFirst(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	allow, actions := &memAllowlist{}, &memActions{}
	d := Deps{AutoPublish: allow, ConnectPlatform: platformsKnown, Actions: actions, Posts: failingCounts{}}
	if _, err := setAutoPublish(ctx, d, AutoPublishInput{Platform: "linkedin", On: true}); err == nil {
		t.Fatal("a failed count is lost")
	}
	if len(allow.list) != 0 || len(actions.rows) != 0 {
		t.Fatalf("list %v, undo %v; want nothing saved", allow.list, actions.rows)
	}
}

// A schedule moved says from when to when, a change the assistant made says
// who asked, the latest failure says why, and a narrowed history says how.
func TestPostHistory_Changes(t *testing.T) {
	at := func(day, h int) time.Time { return time.Date(2026, 4, day, h, 0, 0, 0, time.UTC) }
	sched, failed := models.PostStatusScheduled, models.PostStatusFailed
	d := Deps{
		Posts: examplePosts{byID: map[string]*models.Post{"p1": {ID: "p1", Title: "Easter brunch", FailureReason: "Instagram needs reconnecting"}}},
		Versions: memVersions{list: []models.PostVersion{
			{VersionNumber: 1, CreatedAt: at(1, 9), Creator: models.PostVersionCreatorUser},
			{VersionNumber: 2, CreatedAt: at(2, 9), Creator: models.PostVersionCreatorUser, CreatedBy: "u1"},
		}},
		PostLogs: memPostLogs{list: []models.PostLog{
			{EventTimestamp: at(21, 9), EventType: models.PostLogEventUserSchedule, Actor: "u1", Payload: `{"scheduled_at":"2026-04-22T10:00:00Z"}`},
			{EventTimestamp: at(20, 10), EventType: models.PostLogEventStateTransition, Actor: models.ActorSystem, FromStatus: &sched, ToStatus: &failed},
			{EventTimestamp: at(15, 15), EventType: models.PostLogEventUserSchedule, Actor: "u1", Payload: `{"scheduled_at":"2026-04-20T10:00:00Z","trigger":"assistant"}`},
			{EventTimestamp: at(14, 9), EventType: models.PostLogEventUserSchedule, Actor: "u1", Payload: `{"scheduled_at":"2026-04-18T10:00:00Z","trigger":"api"}`},
			{EventTimestamp: at(13, 9), EventType: models.PostLogEventUserCancel, Actor: "u1"},
			{EventTimestamp: at(12, 9), EventType: models.PostLogEventUserSchedule, Actor: "u1", Payload: `{"scheduled_at":"2026-04-17T10:00:00Z"}`},
		}},
		Users: memUsers{},
	}
	ctx, _ := turn(t, viewcontext.Scope{Kind: models.AssistantScopePost, ID: "p1"}, nil)
	out, err := postHistory(ctx, d, PostHistoryInput{})
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, e := range out.Entries {
		changes = append(changes, e.Change)
	}
	if !slices.Equal(changes, []string{"scheduled", "failed", "rescheduled", "scheduled", "unscheduled", "scheduled", "version", "version"}) {
		t.Fatalf("changes = %v", changes)
	}
	// After a failure the next schedule is new: the failed one ended.
	if again := out.Entries[0]; again.From != nil || !again.To.Equal(at(22, 10)) {
		t.Fatalf("schedule after the failure = %+v", again)
	}
	failure, moved, after := out.Entries[1], out.Entries[2], out.Entries[3]
	if failure.Reason != "Instagram needs reconnecting" || failure.Kind != "event" {
		t.Fatalf("failure = %+v", failure)
	}
	if moved.Source != "assistant" || moved.By != "Olena" || !moved.From.Equal(at(18, 10)) || !moved.To.Equal(at(20, 10)) {
		t.Fatalf("rescheduled = %+v; want the assistant, asked by Olena, 18 → 20", moved)
	}
	// After an unschedule the next schedule is new, not a move.
	if after.From != nil || !after.To.Equal(at(18, 10)) || after.Source != "" {
		t.Fatalf("scheduled = %+v", after)
	}
	if out.UnrecordedBefore == nil || !out.UnrecordedBefore.Equal(at(1, 9)) || out.Timezone != "UTC" || out.Scope != nil {
		t.Fatalf("unrecorded %v, tz %q, scope %+v", out.UnrecordedBefore, out.Timezone, out.Scope)
	}

	out, err = postHistory(ctx, d, PostHistoryInput{Since: "2026-04-14", Changes: []string{"scheduled", "rescheduled"}})
	if err != nil || len(out.Entries) != 3 || out.Scope == nil || out.Scope.Since != "2026-04-14" || len(out.Scope.Changes) != 2 {
		t.Fatalf("narrowed = %+v, %v", out, err)
	}
	_, err = postHistory(ctx, d, PostHistoryInput{Changes: []string{"edited"}})
	refused(t, err)
	_, err = postHistory(ctx, d, PostHistoryInput{Since: "Monday"})
	refused(t, err)
}

// When the log has more entries than a history reads, what is older than
// the oldest one read is left out, and the history says there is more,
// even when nothing newer matches.
func TestPostHistory_LogWindowFull(t *testing.T) {
	start := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	logs := []models.PostLog{{EventTimestamp: start, EventType: models.PostLogEventUserCancel, Actor: "u1"}}
	for i := range historyLogWindow {
		logs = append(logs, models.PostLog{EventTimestamp: start.Add(time.Duration(i+1) * time.Hour), EventType: models.PostLogEventTaskStarted, Actor: models.ActorSystem})
	}
	d := Deps{
		Posts:    examplePosts{byID: map[string]*models.Post{"p1": {ID: "p1", Title: "Easter brunch"}}},
		Versions: memVersions{list: []models.PostVersion{{VersionNumber: 1, CreatedAt: start.Add(-time.Hour), Creator: models.PostVersionCreatorUser, CreatedBy: "u1"}}},
		PostLogs: memPostLogs{list: logs},
		Users:    memUsers{},
	}
	ctx, _ := turn(t, viewcontext.Scope{Kind: models.AssistantScopePost, ID: "p1"}, nil)
	out, err := postHistory(ctx, d, PostHistoryInput{Changes: []string{"unscheduled"}})
	if err != nil || len(out.Entries) != 0 || !out.More {
		t.Fatalf("out = %+v, %v; want no entries and more", out, err)
	}
	// Since narrows what is read, so a later day fits the window and its
	// history is whole: the machinery after it is no change at all.
	out, err = postHistory(ctx, d, PostHistoryInput{Since: "2026-04-15"})
	if err != nil || out.More || len(out.Entries) != 0 {
		t.Fatalf("since = %+v, %v", out, err)
	}
}

// The report goes to the members named, by name or email, once each;
// someone who isn't a member, or turned the email off, is said and skipped.
func TestEmailWeeklyDigest_Recipients(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	dg := &memDigest{out: map[string]bool{"u2": true}}
	d := Deps{Digest: dg, Users: memUsers{}}

	out, err := emailWeeklyDigest(ctx, d, EmailDigestInput{Recipients: []string{"olena", "MAX@acme.test", "u1", "dan@else.test"}})
	if err != nil || !slices.Equal(dg.emailed, []string{"u1"}) || len(out.Sent) != 1 {
		t.Fatalf("out = %+v, %v; emailed %v", out, err, dg.emailed)
	}
	want := []DigestSkip{{Recipient: "dan@else.test", Reason: digestNotMember}, {Recipient: "max@acme.test", Reason: digestOptedOut}}
	if !slices.Equal(out.NotSent, want) {
		t.Fatalf("not sent = %+v; want %+v", out.NotSent, want)
	}
	// Every send is the week reported, not "last week" worked out again.
	if !slices.Equal(dg.dates, []string{"2026-11-02"}) {
		t.Fatalf("emailed weeks = %v", dg.dates)
	}

	// No member named sends nothing and needs no offer; the result says why.
	out, err = emailWeeklyDigest(ctx, d, EmailDigestInput{Recipients: []string{"dan@else.test"}})
	if err != nil || len(out.Sent) != 0 || out.SentAt != nil || out.NotSent[0].Reason != digestNotMember {
		t.Fatalf("outsider = %+v, %v", out, err)
	}
	if err := checkDigestEmail(ctx, d, EmailDigestInput{Recipients: []string{"dan@else.test"}}); err != nil {
		t.Fatalf("precheck = %v; an outsider alone runs to say so", err)
	}
	// Members who all turned it off are refused before an offer.
	refused(t, checkDigestEmail(ctx, d, EmailDigestInput{Recipients: []string{"max@acme.test"}}))
	_, _, err = digestRecipients(ctx, d, slices.Repeat([]string{"u1"}, maxDigestRecipients+1))
	refused(t, err)

	// The card names each member and each outsider.
	card := digestCard(ctx, d, EmailDigestInput{Recipients: []string{"Olena", "max@acme.test", "dan@else.test"}})
	if card.Verb != "Email to 2 people" || len(card.Rows) != 3 || card.Rows[0].Value != "Olena" || !strings.HasPrefix(card.Rows[2].Note, "Not a member") {
		t.Fatalf("card = %+v", card)
	}
	if card := digestCard(ctx, d, EmailDigestInput{}); card.Verb != "Email it to me" {
		t.Fatalf("own card = %+v", card)
	}
	if line := digestSummary(ctx, d, EmailDigestInput{Week: "2026-11-02", Recipients: []string{"u1", "u2"}}); line != "Email the report for the week of 2026-11-02 to 2 people" {
		t.Fatalf("summary = %q", line)
	}
}

// twoDans is a workspace with two members of one name.
type twoDans struct{ repository.UserRepository }

func (twoDans) List(context.Context) ([]models.User, error) {
	return []models.User{{ID: "u1", Name: "Dan", Email: "dan@acme.test"}, {ID: "u2", Name: "Dan", Email: "dan.cole@acme.test"}}, nil
}

// A name two members share is refused with their emails, never guessed; an
// email or id still picks one of them.
func TestDigestRecipients_AmbiguousName(t *testing.T) {
	ctx, _ := turn(t, workspace, nil)
	d := Deps{Users: twoDans{}}
	_, _, err := digestRecipients(ctx, d, []string{"dan"})
	r, ok := errors.AsType[*assistantkit.Refusal](err)
	if !ok || !slices.Equal(r.Alternatives, []string{"dan@acme.test", "dan.cole@acme.test"}) {
		t.Fatalf("refusal = %v", err)
	}
	members, _, err := digestRecipients(ctx, d, []string{"DAN.COLE@acme.test"})
	if err != nil || len(members) != 1 || members[0].ID != "u2" {
		t.Fatalf("by email = %+v, %v", members, err)
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

// decidedIdeas counts the ideas decided in a window, noting what it was asked.
type decidedIdeas struct {
	repository.IdeaRepository
	from, to time.Time
	campaign string
}

func (r *decidedIdeas) CountDecided(_ context.Context, from, to time.Time, campaign string) (int, error) {
	r.from, r.to, r.campaign = from, to, campaign
	return 12, nil
}

// waitingPosts is the posts waiting to go out.
type waitingPosts struct {
	repository.PostRepository
	next   *models.Post
	counts []repository.ScheduledCount
	after  time.Time
}

func (r *waitingPosts) NextScheduled(_ context.Context, after time.Time, _ string) (*models.Post, error) {
	r.after = after
	return r.next, nil
}

func (r *waitingPosts) ScheduledCounts(context.Context) ([]repository.ScheduledCount, error) {
	return r.counts, nil
}

// The week card gets the failed posts' titles, the ideas decided over the
// range's local days, the next post to go out and the campaign.
func TestActivityExtras(t *testing.T) {
	now := time.Date(2026, 4, 21, 9, 0, 0, 0, time.UTC)
	at := now.Add(33 * time.Hour)
	ideas := &decidedIdeas{}
	posts := &waitingPosts{next: &models.Post{ID: "p9", Title: "Easter brunch", PlatformID: "instagram", ScheduledAt: &at}}
	d := Deps{
		Titles: memTitles{byKind: map[viewcontext.EntityKind][]string{viewcontext.KindPost: {"p1"}}}, IdeaRepo: ideas, Posts: posts,
		Campaigns: &memCampaigns{c: &models.Campaign{ID: "c1", Name: "Spring reopening"}},
	}
	out := &ActivityOutput{RangeReport: &report.RangeReport{From: "2026-04-14", To: "2026-04-20", TZ: "Europe/Madrid",
		Failed: report.Failed{Total: 1, Posts: []report.FailedPost{{PostID: "p1", FailureReason: "token expired"}}}}}
	if err := activityExtras(t.Context(), d, out, "c1", now); err != nil {
		t.Fatal(err)
	}
	if out.Failed.Posts[0].Title != "p1" || out.IdeasDecided != 12 || out.Campaign == nil || out.Campaign.Name != "Spring reopening" {
		t.Fatalf("out = %+v", out)
	}
	madrid, _ := time.LoadLocation("Europe/Madrid")
	if !ideas.from.Equal(time.Date(2026, 4, 14, 0, 0, 0, 0, madrid)) || !ideas.to.Equal(time.Date(2026, 4, 21, 0, 0, 0, 0, madrid)) || ideas.campaign != "c1" {
		t.Fatalf("decided asked for %v – %v in %q; want the local days, end exclusive", ideas.from, ideas.to, ideas.campaign)
	}
	if out.NextScheduled == nil || out.NextScheduled.Title != "Easter brunch" || !out.NextScheduled.At.Equal(at) || !posts.after.Equal(now) {
		t.Fatalf("next = %+v after %v", out.NextScheduled, posts.after)
	}
	if out.Step == nil || out.Step.Subject != "2026-04-14 – 2026-04-20" {
		t.Fatalf("step = %+v", out.Step)
	}

	// Nothing waiting and no campaign leave those out.
	posts.next = nil
	out = &ActivityOutput{RangeReport: &report.RangeReport{From: "2026-04-21", To: "2026-04-21", TZ: "UTC"}}
	if err := activityExtras(t.Context(), d, out, "", now); err != nil || out.NextScheduled != nil || out.Campaign != nil || out.Step.Subject != "2026-04-21" {
		t.Fatalf("quiet = %+v, %v", out, err)
	}
}
