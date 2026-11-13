package packs

import (
	"context"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
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
	if err != nil || !slices.Equal(got.IdeaIDs, []string{"i1", "i2"}) || got.CampaignID != "c1" || !slices.Equal(changes, []string{"1 of the 3 ideas no longer exist"}) {
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
		!slices.Equal(changes, []string{"1 of the 2 posts are no longer in the campaign"}) {
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
	if err != nil || len(got.Moves) != 1 || got.Moves[0].PostID != "p1" || !slices.Equal(changes, []string{"A post the offer moved was deleted"}) {
		t.Fatalf("got %+v, %v, %v", got, changes, err)
	}
	if got, changes, err := movesThatHold(ctx, d, ApplyMovesInput{CampaignID: "c1", Moves: in.Moves[:1]}); err != nil || len(got.Moves) != 1 || changes != nil {
		t.Fatalf("all hold: %+v, %v, %v", got, changes, err)
	}
	_, _, err = movesThatHold(ctx, d, ApplyMovesInput{CampaignID: "c1", Moves: in.Moves[1:]})
	refused(t, err)
}

// A move that no longer holds says why, by its post's title.
func TestWhyMoveFails(t *testing.T) {
	c := &models.Campaign{}
	at := day("2027-02-20")
	m := Move{PostID: "p1", FromAt: at, To: "2027-03-02"}
	for _, tc := range []struct {
		post models.Post
		want string
	}{
		{models.Post{Title: "Spa", Status: models.PostStatusScheduled, ScheduledAt: at}, "“Spa” was scheduled since — scheduled posts stay put"},
		{models.Post{Title: "Spa", Status: models.PostStatusPublished, ScheduledAt: at}, "“Spa” was published since"},
		{models.Post{Status: models.PostStatusDraft, ScheduledAt: day("2027-02-21")}, "A post was moved since"},
		{models.Post{Title: "Spa", Status: models.PostStatusDraft, ScheduledAt: at}, "“Spa” can't go to 2027-03-02 any more: the campaign doesn't publish then"},
	} {
		if got := whyMoveFails(c, &tc.post, m); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.post, got, tc.want)
		}
	}
}
