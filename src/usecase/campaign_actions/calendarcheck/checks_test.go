package calendarcheck

import (
	"slices"
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func kinds(fs []Fix) []FixKind {
	out := make([]FixKind, len(fs))
	for i, f := range fs {
		out[i] = f.Kind
	}
	return out
}

// Posts of a type the campaign narrowed away get a finding per platform and
// type, with the type allowed again, a conversion to each type it allows,
// taking them off the calendar and deleting them as fixes.
func TestCheck_PostTypeNotAllowed(t *testing.T) {
	c := campaign()
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "li", PostTypes: []string{"text", "image", models.PostTypeLinkPost}}}
	c.Platforms = []models.Platform{{ID: "li", Name: "LinkedIn"}}
	posts := []models.Post{
		{ID: "v1", Status: models.PostStatusDraft, PlatformID: "li", PlatformPostType: "video", ScheduledAt: at("2026-12-08T09:00")},
		{ID: "v2", Status: models.PostStatusPlanned, PlatformID: "li", PlatformPostType: "video"},
		{ID: "ok", Status: models.PostStatusDraft, PlatformID: "li", PlatformPostType: "text", ScheduledAt: at("2026-12-09T09:00")},
		{ID: "gone", Status: models.PostStatusPublished, PlatformID: "li", PlatformPostType: "video", ScheduledAt: at("2026-12-02T09:00")},
	}
	f := find(Check(c, posts, nil, *at("2026-11-01T00:00")), CodePostTypeNotAllowed)
	if f == nil || !slices.Equal(f.PostIDs, []string{"v1", "v2"}) {
		t.Fatalf("finding = %+v", f)
	}
	want := []FixKind{FixEnablePostType, FixConvertPostType, FixConvertPostType, FixUnschedule, FixDelete}
	if !slices.Equal(kinds(f.Fixes), want) {
		t.Fatalf("fixes = %v, want %v", kinds(f.Fixes), want)
	}
	if e := f.Fixes[0]; e.PlatformID != "li" || e.PostType != "video" {
		t.Fatalf("enable = %+v", e)
	}
	// The link post isn't offered: a video has no link to carry over.
	if f.Fixes[1].PostType != "text" || f.Fixes[2].PostType != "image" {
		t.Fatalf("converts = %+v", f.Fixes[1:3])
	}
	if !slices.Equal(f.Fixes[3].PostIDs, []string{"v1"}) {
		t.Fatalf("unschedule = %+v", f.Fixes[3])
	}
}

// Two posts from one idea on one platform within a week are a finding whose
// fix moves the later one to the nearest day a week from the other. The
// same idea on another platform isn't.
func TestCheck_SameIdeaClose(t *testing.T) {
	posts := []models.Post{
		{ID: "a", Title: "A", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: at("2026-12-07T09:00")},
		{ID: "b", Title: "B", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: at("2026-12-09T09:00")},
		{ID: "x", Title: "X", Status: models.PostStatusDraft, PlatformID: "x", ScheduledAt: at("2026-12-07T09:00")},
	}
	ideas := map[string][]string{"a": {"i1"}, "b": {"i2", "i1"}, "x": {"i1"}}
	got := Check(campaign(), posts, ideas, *at("2026-11-01T00:00"))
	f := find(got, CodeSameIdea)
	if f == nil || !slices.Equal(f.PostIDs, []string{"a", "b"}) {
		t.Fatalf("finding = %+v (codes %v)", f, codes(got))
	}
	// The 14th is the first weekday a week after the 7th.
	if len(f.Fixes) != 1 || f.Fixes[0].Kind != FixMovePost || f.Fixes[0].PostID != "b" || f.Fixes[0].To != "2026-12-14" {
		t.Fatalf("fix = %+v", f.Fixes)
	}
	if n := len(slices.DeleteFunc(slices.Clone(got), func(f Finding) bool { return f.Code != CodeSameIdea })); n != 1 {
		t.Fatalf("%d same-idea findings, want 1", n)
	}
}

// A scheduled later post stays; the earlier one moves instead.
func TestCheck_SameSourceMovesTheMovable(t *testing.T) {
	posts := []models.Post{
		{ID: "a", Title: "A", Status: models.PostStatusDraft, PlatformID: "li", UsedAssetIDs: models.StringSlice{"s"}, ScheduledAt: at("2026-12-08T09:00")},
		{ID: "b", Title: "B", Status: models.PostStatusScheduled, PlatformID: "li", UsedAssetIDs: models.StringSlice{"s"}, ScheduledAt: at("2026-12-09T09:00")},
	}
	f := find(Check(campaign(), posts, nil, *at("2026-11-01T00:00")), CodeSameSource)
	if f == nil || len(f.Fixes) != 1 || f.Fixes[0].PostID != "a" || f.Fixes[0].To != "2026-12-07" {
		t.Fatalf("finding = %+v", f)
	}
}

// Posts to one account closer than the slot are a finding; the same times
// on two accounts aren't.
func TestCheck_CrowdedSlot(t *testing.T) {
	posts := []models.Post{
		{ID: "a", Title: "A", Status: models.PostStatusDraft, PlatformID: "li", SocialAccountID: "acc", ScheduledAt: at("2026-12-08T09:00")},
		{ID: "b", Title: "B", Status: models.PostStatusDraft, PlatformID: "li", SocialAccountID: "acc", ScheduledAt: at("2026-12-08T09:20")},
		{ID: "c", Title: "C", Status: models.PostStatusDraft, PlatformID: "li", SocialAccountID: "other", ScheduledAt: at("2026-12-08T09:10")},
		{ID: "d", Title: "D", Status: models.PostStatusDraft, PlatformID: "li", SocialAccountID: "acc", ScheduledAt: at("2026-12-08T10:00")},
	}
	got := Check(campaign(), posts, nil, *at("2026-11-01T00:00"))
	f := find(got, CodeCrowdedSlot)
	if f == nil || !slices.Equal(f.PostIDs, []string{"a", "b"}) {
		t.Fatalf("finding = %+v (codes %v)", f, codes(got))
	}
	if len(f.Fixes) != 1 || f.Fixes[0].PostID != "b" {
		t.Fatalf("fix = %+v", f.Fixes)
	}
	if !strings.Contains(f.Message, "within 30 minutes") {
		t.Fatalf("message = %q", f.Message)
	}
}

// A week short of the goal gets a fix planning the missing posts for the
// rest of it; a past week is left alone, and a week well over is info.
func TestCheck_Goal(t *testing.T) {
	c := campaign() // Tue 1 to Thu 31 December
	c.EstimatedPostCount, c.GoalCadence = new(2), "week"
	posts := []models.Post{
		{ID: "w1", Status: models.PostStatusPublished, PlatformID: "li", ScheduledAt: at("2026-12-02T09:00")},
		{ID: "w2", Status: models.PostStatusDraft, PlatformID: "li", ScheduledAt: at("2026-12-09T09:00")},
	}
	for _, d := range []string{"15", "16", "17", "18"} {
		posts = append(posts, models.Post{ID: "w3-" + d, Status: models.PostStatusDraft, PlatformID: "x", ScheduledAt: at("2026-12-" + d + "T09:00")})
	}
	got := Check(c, posts, nil, *at("2026-12-09T08:00"))
	var short []Finding
	for _, f := range got {
		if f.Code == CodeGoalShort {
			short = append(short, f)
		}
	}
	// Week 1 is over; week 2 (8–14) has 1 of 2; weeks 4 (22–28) and 5 (29–31,
	// three days, so one post) have none.
	if len(short) != 3 {
		t.Fatalf("short = %+v", short)
	}
	if fx := short[0].Fixes[0]; fx.Kind != FixPlanWindow || fx.From != "2026-12-09" || fx.To != "2026-12-14" || fx.Count != 1 {
		t.Fatalf("week 2 fix = %+v", fx)
	}
	if fx := short[2].Fixes[0]; fx.From != "2026-12-29" || fx.To != "2026-12-31" || fx.Count != 1 {
		t.Fatalf("week 5 fix = %+v", fx)
	}
	if f := find(got, CodeGoalOver); f == nil || len(f.Fixes) != 0 {
		t.Fatalf("over = %+v", f)
	}
	if short[0].Key == short[1].Key {
		t.Fatal("two periods share a key")
	}
}

// Posts left on a platform the campaign stopped targeting get a finding,
// with the platform added back, taking them off the calendar and deleting
// them as fixes; published ones are history.
func TestCheck_PlatformNotTargeted(t *testing.T) {
	c := campaign()
	c.TargetPlatforms = models.CampaignPlatforms{{ID: "li"}}
	posts := []models.Post{
		{ID: "t1", Status: models.PostStatusDraft, PlatformID: "th", Platform: &models.Platform{ID: "th", Name: "Threads"}},
		{ID: "t2", Status: models.PostStatusScheduled, PlatformID: "th", ScheduledAt: at("2026-12-08T09:00")},
		{ID: "t3", Status: models.PostStatusPublished, PlatformID: "th", ScheduledAt: at("2026-11-02T09:00")},
		{ID: "l1", Status: models.PostStatusDraft, PlatformID: "li"},
	}
	f := find(Check(c, posts, nil, *at("2026-11-01T00:00")), CodePlatformNotTargeted)
	if f == nil || !slices.Equal(f.PostIDs, []string{"t1", "t2"}) || !strings.Contains(f.Message, "Threads") {
		t.Fatalf("finding = %+v", f)
	}
	if !slices.Equal(kinds(f.Fixes), []FixKind{FixEnablePlatform, FixUnschedule, FixDelete}) || f.Fixes[0].PlatformID != "th" || !slices.Equal(f.Fixes[1].PostIDs, []string{"t2"}) {
		t.Fatalf("fixes = %+v", f.Fixes)
	}
}
