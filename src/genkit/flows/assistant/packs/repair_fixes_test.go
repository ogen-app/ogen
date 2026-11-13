package packs

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/firebase/genkit/go/genkit"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/genkit/flows/internal/assistantkit"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/usecase/post_actions/withdraw"
)

func (r *memPosts) SetScheduleIf(_ context.Context, _ bun.IDB, cs []repository.ScheduleChange, _ time.Time) ([]string, error) {
	set := []string{}
	for _, c := range cs {
		if i := slices.IndexFunc(r.posts, func(p models.Post) bool { return p.ID == c.PostID }); i >= 0 && !r.raced[c.PostID] {
			r.posts[i].ScheduledAt = c.To
			set = append(set, c.PostID)
		}
	}
	return set, nil
}

func (r *memPosts) SetPostTypeIf(_ context.Context, _ bun.IDB, cs []repository.PostTypeChange, _ time.Time) ([]string, error) {
	set := []string{}
	for _, c := range cs {
		if i := slices.IndexFunc(r.posts, func(p models.Post) bool { return p.ID == c.PostID }); i >= 0 && !r.raced[c.PostID] {
			r.posts[i].PlatformPostType, r.posts[i].Status = c.To, c.ToStatus
			set = append(set, c.PostID)
		}
	}
	return set, nil
}

func isRefusal(err error) bool {
	_, ok := errors.AsType[*assistantkit.Refusal](err)
	return ok
}

// memLinks maps posts to their ideas.
type memLinks map[string][]string

func (memLinks) Link(context.Context, bun.IDB, string, []string) error { return nil }
func (memLinks) IdeasOfPost(context.Context, string) ([]models.PostSourceIdea, error) {
	return nil, nil
}
func (memLinks) PostsOfIdea(context.Context, string) ([]models.IdeaPostRef, error) { return nil, nil }
func (l memLinks) IdeasOfCampaignPosts(context.Context, string) (map[string][]string, error) {
	return l, nil
}

// memRemover records deletes and withdrawals; published posts refuse.
type memRemover struct {
	posts     *memPosts
	deleted   []string
	cancelled []string
	// broken is the post whose delete fails.
	broken string
}

func (r *memRemover) Delete(_ context.Context, p *models.Post, _ string) (bool, error) {
	if p.ID == r.broken {
		return false, errors.New("storage timeout")
	}
	if p.Status == models.PostStatusPublished {
		return false, withdraw.ErrPublished
	}
	r.deleted = append(r.deleted, p.ID)
	r.posts.posts = slices.DeleteFunc(r.posts.posts, func(q models.Post) bool { return q.ID == p.ID })
	return true, nil
}

func (r *memRemover) Cancel(_ context.Context, id, _ string) error {
	r.cancelled = append(r.cancelled, id)
	return nil
}

// A campaign that narrowed a platform's post types gets a finding for the
// posts of the dropped type, each fix offered through the tool that makes it.
func TestCheckCalendar_PostTypeFixes(t *testing.T) {
	d, posts, _ := repairDeps()
	d.Remove = &memRemover{posts: posts}
	c := d.Campaigns.(*memCampaigns).c
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "li", PostTypes: []string{"text", "image"}}}
	posts.posts = append(posts.posts, models.Post{ID: "v", Title: "Clip", Status: models.PostStatusDraft, PlatformID: "li",
		PlatformPostType: "video", ScheduledAt: day("2027-03-10")})
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	ctx = assistantkit.WithProposalSink(ctx, sink)

	out, err := checkCalendar(ctx, d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(out.Findings, func(f FindingOut) bool { return f.Code == "post_type_not_allowed" })
	if i < 0 {
		t.Fatalf("findings = %+v", out.Findings)
	}
	f := out.Findings[i]
	var tools []string
	for _, fix := range f.Fixes {
		if fix.ProposalID == "" {
			t.Fatalf("fix without an offer: %+v", fix)
		}
		p := sink.got[slices.IndexFunc(sink.got, func(p assistantkit.Proposal) bool { return p.Summary == fix.Title })]
		tools = append(tools, p.Tool)
	}
	want := []string{"changePostType", "changePostType", "updateCampaignPlatforms", "unschedulePosts", "deletePosts"}
	if !slices.Equal(tools, want) {
		t.Fatalf("tools = %v, want %v", tools, want)
	}
	enable := sink.got[slices.IndexFunc(sink.got, func(p assistantkit.Proposal) bool { return p.Tool == "updateCampaignPlatforms" })]
	if in := enable.Args.(campaign_assistant.UpdateCampaignPlatformsInput); in.Platforms[0].Platform != "li" || !slices.Equal(in.Platforms[0].PostTypes, []string{"video"}) {
		t.Fatalf("enable = %+v", in)
	}
	// Its card names the platform with the types it will run, and the posts
	// it fixes.
	if card := enable.Card; card == nil || card.Verb != "Re-add video" || len(card.Rows) != 2 ||
		card.Rows[0].Kind != assistantkit.CardRowPlatform || !strings.HasSuffix(card.Rows[0].Note, "+ video") ||
		card.Rows[1].Kind != assistantkit.CardRowFixes {
		t.Fatalf("re-add card = %+v", enable.Card)
	}

	// Without a way to delete posts, that fix isn't offered.
	d.Remove = nil
	out, err = checkCalendar(ctx, d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	f = out.Findings[slices.IndexFunc(out.Findings, func(f FindingOut) bool { return f.Code == "post_type_not_allowed" })]
	if slices.ContainsFunc(f.Fixes, func(x FixOffer) bool { return x.Kind == "delete" }) {
		t.Fatalf("delete offered without a remover: %+v", f.Fixes)
	}
}

// Two posts from one idea close together get a move of the later one,
// offered as a single applyCalendarMoves move from its exact date.
func TestCheckCalendar_MovePostFix(t *testing.T) {
	d, posts, _ := repairDeps()
	d.Links = memLinks{"p2": {"i"}, "p3": {"i"}}
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	ctx = assistantkit.WithProposalSink(ctx, sink)

	out, err := checkCalendar(ctx, d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(out.Findings, func(f FindingOut) bool { return f.Code == "same_idea_close" })
	if i < 0 {
		t.Fatalf("findings = %+v", out.Findings)
	}
	fix := out.Findings[i].Fixes[0]
	p := sink.got[slices.IndexFunc(sink.got, func(p assistantkit.Proposal) bool { return p.Summary == fix.Title })]
	in := p.Args.(ApplyMovesInput)
	// p3 is scheduled, so p2 moves.
	if p.Tool != "applyCalendarMoves" || len(in.Moves) != 1 || in.Moves[0].PostID != "p2" || !in.Moves[0].FromAt.Equal(*posts.posts[1].ScheduledAt) {
		t.Fatalf("move offer = %+v", p)
	}
	// It is a choice of free slots, the nearest recommended and offered
	// as the plain move, each its own move at the publishing time.
	if len(p.Options) < 2 || !p.Options[0].Recommended || p.Options[1].Recommended || p.Options[0].ID != in.Moves[0].To ||
		p.Card == nil || p.Card.Verb != "Move to "+p.Options[0].Title || !strings.HasSuffix(p.Card.Meta, "free slots") {
		t.Fatalf("slots = %+v, card %+v", p.Options, p.Card)
	}
	second := p.Options[1].Args.(ApplyMovesInput).Moves[0]
	if p.Options[1].Tool != "applyCalendarMoves" || second.To != p.Options[1].ID || second.ToAt == nil || second.ToAt.Hour() != 9 {
		t.Fatalf("second slot = %+v", p.Options[1])
	}
	if len(fix.Options) != len(p.Options) || fix.Card == nil {
		t.Fatalf("the finding's fix carries no choice: %+v", fix)
	}
}

func skippedIDs(s []SkippedPost) []string {
	out := make([]string, len(s))
	for i, p := range s {
		out[i] = p.PostID
	}
	return out
}

// Unscheduling clears an unscheduled post's date, withdraws a scheduled one,
// and leaves the rest with a reason.
func TestUnschedulePosts(t *testing.T) {
	d, posts, _ := repairDeps()
	rm := &memRemover{posts: posts}
	d.Remove = rm
	posts.posts = append(posts.posts,
		models.Post{ID: "pub", Status: models.PostStatusPublished, PlatformID: "li", ScheduledAt: day("2027-03-02")},
		models.Post{ID: "raced", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: day("2027-03-05")})
	posts.raced = map[string]bool{"raced": true}
	ctx, touched := turn(t, campaign1, nil)

	out, err := unschedulePosts(ctx, d, PostsRef{PostIDs: []string{"p1", "p3", "pub", "raced", "elsewhere"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed != 1 || out.Withdrawing != 1 || !slices.Equal(rm.cancelled, []string{"p3"}) {
		t.Fatalf("out = %+v, cancelled %v", out, rm.cancelled)
	}
	if got := skippedIDs(out.Skipped); !slices.Equal(got, []string{"elsewhere", "pub", "raced"}) {
		t.Fatalf("skipped = %v", got)
	}
	if posts.posts[0].ScheduledAt != nil {
		t.Fatalf("p1 still dated %v", posts.posts[0].ScheduledAt)
	}
	// The rows say what each post was: p3 back to ready keeping its date,
	// p1 off the calendar, with the date it had.
	if len(out.Posts) != 2 {
		t.Fatalf("rows = %+v", out.Posts)
	}
	if w := out.Posts[0]; w.PostID != "p3" || w.ToStatus != models.PostStatusReadyForPublish || w.ScheduledAt == nil || w.Platform != "LinkedIn" {
		t.Fatalf("withdrawn row = %+v", w)
	}
	if u := out.Posts[1]; u.PostID != "p1" || u.Title != "Early" || u.WasAt == nil || !u.WasAt.Equal(*day("2027-02-20")) || u.ScheduledAt != nil {
		t.Fatalf("unscheduled row = %+v", u)
	}
	if len(touched()) == 0 {
		t.Fatal("the change wasn't announced")
	}
}

// A post type changes only on posts Ogen alone holds, to a type the
// campaign allows; a ready post goes back to draft.
func TestChangePostType(t *testing.T) {
	d, posts, _ := repairDeps()
	d.Campaigns.(*memCampaigns).c.TargetPlatforms = models.CampaignPlatforms{{ID: "li", PostTypes: []string{"text", "image"}}}
	for i := range posts.posts {
		posts.posts[i].PlatformPostType = "video"
	}
	d.Attachments = memAttachments{n: map[string]int{"p2": 1}}
	ctx, _ := turn(t, campaign1, nil)

	out, err := changePostType(ctx, d, ChangePostTypeInput{PostIDs: []string{"p1", "p2", "p3"}, PostType: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed != 2 || !slices.Equal(skippedIDs(out.Skipped), []string{"p3"}) {
		t.Fatalf("out = %+v", out)
	}
	if p := posts.posts[1]; p.PlatformPostType != "text" || p.Status != models.PostStatusDraft {
		t.Fatalf("p2 = %s %s", p.PlatformPostType, p.Status)
	}
	// Each row says the types, what was kept and the date it keeps.
	if len(out.Posts) != 2 || out.Posts[1].PostID != "p2" {
		t.Fatalf("rows = %+v", out.Posts)
	}
	if r := out.Posts[1]; r.FromType != "video" || r.ToType != "text" || r.FromStatus != models.PostStatusReadyForPublish ||
		r.ToStatus != models.PostStatusDraft || !slices.Equal(r.Kept, []string{"media"}) || r.ScheduledAt == nil {
		t.Fatalf("p2 row = %+v", r)
	}
	if out, err = changePostType(ctx, d, ChangePostTypeInput{PostIDs: []string{"p1"}, PostType: "carousel"}); err != nil || out.Changed != 0 {
		t.Fatalf("a type the campaign doesn't allow = %+v, %v", out, err)
	}
	if _, err := changePostType(ctx, d, ChangePostTypeInput{PostIDs: []string{"p1"}, PostType: models.PostTypeThread}); !isRefusal(err) {
		t.Fatalf("thread = %v", err)
	}
}

// Deleting keeps published posts.
func TestDeletePosts(t *testing.T) {
	d, posts, _ := repairDeps()
	rm := &memRemover{posts: posts}
	d.Remove = rm
	posts.posts[1].Status = models.PostStatusPublished
	ctx, _ := turn(t, campaign1, nil)

	out, err := deletePosts(ctx, d, PostsRef{PostIDs: []string{"p1", "p2"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Changed != 1 || !slices.Equal(rm.deleted, []string{"p1"}) || !slices.Equal(skippedIDs(out.Skipped), []string{"p2"}) {
		t.Fatalf("out = %+v, deleted %v", out, rm.deleted)
	}
	if len(out.Posts) != 1 || out.Posts[0].PostID != "p1" || out.Posts[0].FromStatus != models.PostStatusDraft || out.Posts[0].ToStatus != "" {
		t.Fatalf("rows = %+v", out.Posts)
	}
}

// memAttachments counts attachments per post.
type memAttachments struct {
	repository.PostAttachmentRepository
	n map[string]int
}

func (m memAttachments) ListByPostIDs(_ context.Context, ids []string) ([]models.PostAttachment, error) {
	var out []models.PostAttachment
	for _, id := range ids {
		for range m.n[id] {
			out = append(out, models.PostAttachment{PostID: id})
		}
	}
	return out, nil
}

// The delete offer's card names each post with its status, platform and
// date, what goes with its files, and that deleting is final.
func TestDeleteCard(t *testing.T) {
	d, posts, _ := repairDeps()
	d.Attachments = memAttachments{n: map[string]int{"p1": 2}}
	ctx, _ := turn(t, campaign1, nil)
	c := d.Campaigns.(*memCampaigns).c
	card := deleteCard(ctx, d, c, posts.posts, PostsRef{PostIDs: []string{"p1"}})
	if card.Verb != "Delete post" || !strings.HasPrefix(card.Footnote, "Deleting is final") || len(card.Rows) != 2 {
		t.Fatalf("card = %+v", card)
	}
	if r := card.Rows[0]; r.Kind != assistantkit.CardRowPost || r.Value != "Early" || r.Note != "Draft · LinkedIn · 20 Feb" {
		t.Fatalf("post row = %+v", r)
	}
	if r := card.Rows[1]; r.Kind != assistantkit.CardRowMedia || r.Value != "Its 2 files" {
		t.Fatalf("media row = %+v", r)
	}
	// Files that can't be counted are said to go, not left out as none.
	d.Attachments = nil
	card = deleteCard(ctx, d, c, posts.posts, PostsRef{PostIDs: []string{"p1"}})
	if len(card.Rows) != 2 || card.Rows[1].Value != "Its files, if any" {
		t.Fatalf("uncounted card = %+v", card.Rows)
	}
}

// A delete that fails for one post deletes the rest, and retries just it;
// one that fails for every post fails.
func TestDeletePosts_RetriesWhatFailed(t *testing.T) {
	d, posts, _ := repairDeps()
	rm := &memRemover{posts: posts, broken: "p2"}
	d.Remove = rm
	ctx, _ := turn(t, campaign1, nil)

	in := PostsRef{PostIDs: []string{"p1", "p2"}}
	out, err := deletePosts(ctx, d, in)
	if err != nil || out.Changed != 1 || len(out.Failed) != 1 || out.Failed[0].PostID != "p2" {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	retry, notMade, made, ok := postRetry(in, out)
	if !ok || !slices.Equal(retry.PostIDs, []string{"p2"}) || len(notMade) != 1 || made != 1 || !strings.Contains(notMade[0].Reason, "storage timeout") {
		t.Fatalf("retry = %+v, notMade = %+v, ok = %t", retry, notMade, ok)
	}
	if _, err := deletePosts(ctx, d, PostsRef{PostIDs: []string{"p2"}}); err == nil {
		t.Fatal("a delete that deleted nothing must fail")
	}
}

// The repair pack registers the tools its fixes run: deleting only with a
// remover, rewriting only with the post writer.
func TestRepairPack_RegistersFixTools(t *testing.T) {
	tools := func(d Deps) []string {
		specs, err := repairPack(d).Define(genkit.Init(t.Context()))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, s := range specs {
			names = append(names, s.Name)
		}
		return names
	}
	d, posts, _ := repairDeps()
	d.Remove = &memRemover{posts: posts}
	d.Revise = func(context.Context, string, string, string) (Revised, error) { return Revised{Versioned: true}, nil }
	got := tools(d)
	for _, want := range []string{"applyCalendarMoves", "unschedulePosts", "changePostType", "deletePosts", "revisePost"} {
		if !slices.Contains(got, want) {
			t.Errorf("%s isn't registered: %v", want, got)
		}
	}
	d.Remove, d.Revise = nil, nil
	if got := tools(d); slices.Contains(got, "deletePosts") || slices.Contains(got, "revisePost") {
		t.Fatalf("registered without their services: %v", got)
	}
}

// Posts left on a removed platform are offered the platform back, off the
// calendar, or deleted.
func TestCheckCalendar_PlatformNotTargetedFixes(t *testing.T) {
	d, posts, _ := repairDeps()
	d.Remove = &memRemover{posts: posts}
	c := d.Campaigns.(*memCampaigns).c
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "li"}}
	posts.posts = append(posts.posts, models.Post{ID: "th1", Title: "Thread", Status: models.PostStatusDraft, PlatformID: "th", ScheduledAt: day("2027-03-10")})
	sink := &offers{}
	ctx, _ := turn(t, campaign1, nil)
	ctx = assistantkit.WithProposalSink(ctx, sink)

	out, err := checkCalendar(ctx, d, CampaignRef{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(out.Findings, func(f FindingOut) bool { return f.Code == "platform_not_targeted" })
	if i < 0 {
		t.Fatalf("findings = %+v", out.Findings)
	}
	var tools []string
	for _, fix := range out.Findings[i].Fixes {
		p := sink.got[slices.IndexFunc(sink.got, func(p assistantkit.Proposal) bool { return p.Summary == fix.Title })]
		tools = append(tools, p.Tool)
	}
	if !slices.Equal(tools, []string{"updateCampaignPlatforms", "unschedulePosts", "deletePosts"}) {
		t.Fatalf("tools = %v", tools)
	}
	enable := sink.got[slices.IndexFunc(sink.got, func(p assistantkit.Proposal) bool { return p.Tool == "updateCampaignPlatforms" })]
	if in := enable.Args.(campaign_assistant.UpdateCampaignPlatformsInput); in.Platforms[0].Platform != "th" || len(in.Platforms[0].PostTypes) != 0 {
		t.Fatalf("enable = %+v", in)
	}
}
