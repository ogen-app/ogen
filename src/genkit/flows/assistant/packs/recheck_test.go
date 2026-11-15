package packs

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// memTitles knows the ids in byKind; inCampaign are the ids of the thread's
// campaign.
type memTitles struct {
	repository.EntityTitleRepository
	byKind     map[viewcontext.EntityKind][]string
	inCampaign []string
}

func (m memTitles) Titles(_ context.Context, kind viewcontext.EntityKind, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if slices.Contains(m.byKind[kind], id) {
			out[id] = id
		}
	}
	return out, nil
}

func (m memTitles) CampaignTitles(ctx context.Context, kind viewcontext.EntityKind, _ string, ids []string) (map[string]string, error) {
	out, _ := m.Titles(ctx, kind, ids)
	for id := range out {
		if !slices.Contains(m.inCampaign, id) {
			delete(out, id)
		}
	}
	return out, nil
}

// A confirmed call on ids keeps the ones still there: all of them leaves
// it as offered, some narrows it, none refuses it.
func TestRecheckIDs(t *testing.T) {
	d := Deps{Titles: memTitles{byKind: map[viewcontext.EntityKind][]string{viewcontext.KindIdea: {"i1", "i2"}}}}
	recheck := recheckIDs(d, viewcontext.KindIdea, func(_ context.Context, in MoveIdeasInput) (string, []string) { return ideasIn(in.IdeaIDs) },
		func(in MoveIdeasInput, ids []string) MoveIdeasInput { in.IdeaIDs = ids; return in })
	ctx, _ := turn(t, workspace, nil)

	if got, changes, err := recheck(ctx, MoveIdeasInput{IdeaIDs: []string{"i1", "i2", "i1"}, CampaignID: "c1"}); err != nil || len(got.IdeaIDs) != 3 || changes != nil {
		t.Fatalf("all there: %+v, %v, %v; want it as offered", got, changes, err)
	}
	got, changes, err := recheck(ctx, MoveIdeasInput{IdeaIDs: []string{"i1", "gone", "i2"}, CampaignID: "c1"})
	if err != nil || !slices.Equal(got.IdeaIDs, []string{"i1", "i2"}) || got.CampaignID != "c1" || !slices.Equal(assistantkit.Reasons(changes), []string{"1 of the 3 ideas no longer exist"}) ||
		changes[0].Code != assistantkit.ChangeDeleted || changes[0].Item != "idea" || changes[0].Count != 1 || changes[0].Of != 3 {
		t.Fatalf("some gone: %+v, %v, %v", got, changes, err)
	}
	_, _, err = recheck(ctx, MoveIdeasInput{IdeaIDs: []string{"gone"}})
	refused(t, err)
}

// A post fix keeps the posts still in the campaign.
func TestRecheckIDs_CampaignPosts(t *testing.T) {
	d := Deps{Titles: memTitles{byKind: map[viewcontext.EntityKind][]string{viewcontext.KindPost: {"p1", "p2"}}, inCampaign: []string{"p1"}}}
	recheck := recheckIDs(d, viewcontext.KindPost, campaignPostsIn, withPosts)
	ctx, _ := turn(t, campaign1, nil)
	if got, changes, err := recheck(ctx, PostsRef{PostIDs: []string{"p1", "p2"}}); err != nil || !slices.Equal(got.PostIDs, []string{"p1"}) ||
		!slices.Equal(assistantkit.Reasons(changes), []string{"1 of the 2 posts are no longer in the campaign"}) || changes[0].Code != assistantkit.ChangeLeftCampaign {
		t.Fatalf("got %+v, %v, %v", got, changes, err)
	}
}

// Confirmed moves keep the ones that still hold.
func TestMovesThatHold(t *testing.T) {
	d, _, _ := repairDeps()
	ctx, _ := turn(t, campaign1, nil)
	in := ApplyMovesInput{CampaignID: "c1", Moves: []Move{
		{PostID: "p1", From: "2027-02-20", FromAt: day("2027-02-20"), To: "2027-03-02"},
		{PostID: "gone", To: "2027-03-10"},
	}}
	got, changes, err := movesThatHold(ctx, d, in)
	if err != nil || len(got.Moves) != 1 || got.Moves[0].PostID != "p1" || !slices.Equal(assistantkit.Reasons(changes), []string{"A post the offer moved was deleted"}) ||
		changes[0].Code != assistantkit.ChangeDeleted || changes[0].Count != 1 || changes[0].Of != 2 {
		t.Fatalf("got %+v, %v, %v", got, changes, err)
	}
	if got, changes, err := movesThatHold(ctx, d, ApplyMovesInput{CampaignID: "c1", Moves: in.Moves[:1]}); err != nil || len(got.Moves) != 1 || changes != nil {
		t.Fatalf("all hold: %+v, %v, %v", got, changes, err)
	}
	_, _, err = movesThatHold(ctx, d, ApplyMovesInput{CampaignID: "c1", Moves: in.Moves[1:]})
	refused(t, err)
}

// A moves card has a row per day pair, earliest first, with how many.
func TestMovesCard(t *testing.T) {
	card := movesCard(ApplyMovesInput{Moves: []Move{
		{PostID: "p1", From: "2026-12-24", To: "2026-12-25"},
		{PostID: "p2", To: "2026-12-28"},
		{PostID: "p3", From: "2026-12-24", To: "2026-12-25"},
	}})
	if card.Action != assistantkit.CardActionMove || card.Count != 3 || card.Verb != "Move 3 posts" || len(card.Rows) != 2 {
		t.Fatalf("card = %+v", card)
	}
	undated, thu := card.Rows[0], card.Rows[1]
	if undated.Value != "No date → Mon 28 · 1 post" || undated.Start != "" || undated.End != "2026-12-28" || undated.Count != 1 {
		t.Fatalf("undated row = %+v", undated)
	}
	if thu.Kind != assistantkit.CardRowMove || thu.Value != "Thu 24 → Fri 25 · 2 posts" || thu.Start != "2026-12-24" || thu.End != "2026-12-25" || thu.Count != 2 {
		t.Fatalf("Thursday row = %+v", thu)
	}
}

// A move that no longer holds says why, by its post's title.
func TestWhyMoveFails(t *testing.T) {
	c := &models.Campaign{}
	at := day("2027-02-20")
	m := Move{PostID: "p1", FromAt: at, To: "2027-03-02"}
	for _, tc := range []struct {
		post       models.Post
		code, want string
	}{
		{models.Post{Title: "Spa", Status: models.PostStatusScheduled, ScheduledAt: at}, assistantkit.ChangeScheduled, "“Spa” was scheduled since — scheduled posts stay put"},
		{models.Post{Title: "Spa", Status: models.PostStatusPublished, ScheduledAt: at}, assistantkit.ChangePublished, "“Spa” was published since"},
		{models.Post{Status: models.PostStatusDraft, ScheduledAt: day("2027-02-21")}, assistantkit.ChangeMoved, "A post was moved since"},
		{models.Post{Title: "Spa", Status: models.PostStatusDraft, ScheduledAt: at}, assistantkit.ChangeDayClosed, "“Spa” can't go to 2027-03-02 any more: the campaign doesn't publish then"},
	} {
		code := whyMoveFails(c, &tc.post, m)
		changes := moveChanges(t.Context(), Deps{}, []Move{m}, []failedMove{{code: code, move: m, post: &tc.post}})
		if code != tc.code || len(changes) != 1 || changes[0].Reason != tc.want || changes[0].Title != tc.post.Title {
			t.Errorf("%+v: got %q %+v, want %q %q", tc.post, code, changes, tc.code, tc.want)
		}
	}
}

// Moves that no longer hold are told by what happened on which day, and who
// did it: the members by name, the one confirming as you.
func TestMoveChanges_WhoAndWhichDay(t *testing.T) {
	ctx, _ := turn(t, campaign1, nil)
	d := Deps{Users: memUsers{}}
	thu := func(id string) Move { return Move{PostID: id, From: "2026-12-24", To: "2026-12-25"} }
	moves := []Move{thu("p1"), thu("p2"), thu("p3"), {PostID: "p4", From: "2026-12-28", To: "2026-12-29"}}
	failed := []failedMove{
		{code: assistantkit.ChangeScheduled, move: moves[0], post: &models.Post{Title: "A", UpdatedBy: "u2"}},
		{code: assistantkit.ChangeScheduled, move: moves[1], post: &models.Post{Title: "B", UpdatedBy: "u2"}},
		{code: assistantkit.ChangeMoved, move: moves[3], post: &models.Post{Title: "Spa", UpdatedBy: "u1"}},
		{code: assistantkit.ChangePublished, move: moves[2], post: &models.Post{Title: "C", UpdatedBy: "u2"}},
	}
	changes := moveChanges(ctx, d, moves, failed)
	want := []string{
		"max@acme.test scheduled 2 of the Thursday posts since — scheduled posts stay put",
		"You moved “Spa” since",
		"“C” was published since",
	}
	if !slices.Equal(assistantkit.Reasons(changes), want) {
		t.Fatalf("reasons = %q, want %q", assistantkit.Reasons(changes), want)
	}
	if s := changes[0]; s.Code != assistantkit.ChangeScheduled || s.Count != 2 || s.Of != 3 || s.Day != "2026-12-24" || s.Title != "" ||
		!slices.Equal(s.By, []models.ChangedBy{{UserID: "u2", Name: "max@acme.test", Count: 2}}) {
		t.Fatalf("scheduled = %+v", s)
	}
	// A publish is the publisher's: no member is named for it.
	if p := changes[2]; p.By != nil || p.Count != 1 || p.Title != "C" {
		t.Fatalf("published = %+v", p)
	}
}
