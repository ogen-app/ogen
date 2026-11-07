package platforms

import (
	"errors"
	"slices"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

var catalog = []models.Platform{
	{ID: "pl1", Name: "LinkedIn", PostTypes: models.PostTypeMap{"article": "Article", "carousel": "Carousel", "text-post": "Text"}},
	{ID: "pl2", Name: "Threads", PostTypes: models.PostTypeMap{"text-post": "Text"}},
}

// The beta-test case: "add Threads".
func TestPlan_AddsAPlatform(t *testing.T) {
	cur := models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}}
	next, added, err := Plan(cur, catalog, []Request{{Platform: "threads"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 2 || next[1].ID != "pl2" || len(next[1].PostTypes) != 0 {
		t.Fatalf("next = %+v", next)
	}
	if Summary(added) != "Threads" {
		t.Fatalf("summary = %q", Summary(added))
	}
	if len(cur) != 1 {
		t.Fatal("Plan modified the current platforms")
	}
}

func TestPlan_AddsPostTypes(t *testing.T) {
	cur := models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}}
	next, added, err := Plan(cur, catalog, []Request{{Platform: "LinkedIn", PostTypes: []string{"carousel", "article"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(next[0].PostTypes, []string{"article", "carousel"}) {
		t.Fatalf("post types = %v", next[0].PostTypes)
	}
	if Summary(added) != "the carousel post type on LinkedIn" {
		t.Fatalf("summary = %q", Summary(added))
	}
	if !slices.Equal(cur[0].PostTypes, []string{"article"}) {
		t.Fatal("Plan modified the current post types")
	}
}

// A platform with no selection already allows every type, and asking again
// for a targeted platform changes nothing.
func TestPlan_AlreadySatisfied(t *testing.T) {
	cur := models.CampaignPlatforms{{ID: "pl2"}}
	_, added, err := Plan(cur, catalog, []Request{{Platform: "Threads", PostTypes: []string{"text-post"}}})
	if err != nil || len(added) != 0 {
		t.Fatalf("added = %+v, err = %v", added, err)
	}
}

func TestPlan_Invalid(t *testing.T) {
	cases := map[string]struct {
		req  Request
		alts []string
	}{
		"unknown platform": {Request{Platform: "MySpace"}, []string{"LinkedIn", "Threads"}},
		"unknown type":     {Request{Platform: "Threads", PostTypes: []string{"carousel"}}, []string{"text-post"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Plan(nil, catalog, []Request{tc.req})
			ie, ok := errors.AsType[*InvalidError](err)
			if !ok || ie.Reason == "" || !slices.Equal(ie.Alternatives, tc.alts) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// Omitting post types on a platform restricted to some of them allows all.
func TestPlan_OmittedTypesLiftARestriction(t *testing.T) {
	cur := models.CampaignPlatforms{{ID: "pl1", PostTypes: []string{"article"}}}
	next, added, err := Plan(cur, catalog, []Request{{Platform: "LinkedIn"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || len(next[0].PostTypes) != 0 {
		t.Fatalf("next = %+v, want LinkedIn with every type", next)
	}
	if Summary(added) != "every post type on LinkedIn" {
		t.Fatalf("summary = %q", Summary(added))
	}
	if !slices.Equal(cur[0].PostTypes, []string{"article"}) {
		t.Fatal("Plan modified the current post types")
	}
}
