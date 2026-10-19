package assets

import (
	"context"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

type fakeCounter struct {
	got []models.AssetQuery
}

func (f *fakeCounter) CountViews(_ context.Context, qs []models.AssetQuery) ([]models.AssetViewCounts, error) {
	f.got = qs
	out := make([]models.AssetViewCounts, len(qs))
	for i := range qs {
		out[i].Total = 10 + i
	}
	return out, nil
}

// Every parameter of the address parses, and none of them is also volatile.
func TestSurfaceParamsParse(t *testing.T) {
	s := NewSurface(nil)
	for p, multi := range s.Params() {
		if slices.Contains(s.Volatile(), p) {
			t.Errorf("%s is both part of the address and volatile", p)
		}
		value := map[string]string{
			"kind": "pdf", "kind_not": "pdf", "status": "ready", "status_not": "ready",
			"ideas": "yes", "ideas_not": "yes", "sort": "title", "group": "kind", "text": "bees",
			"updated": "7d", "created": "7d", "updated_after": "2026-10-01", "updated_before": "2026-10-01",
			"created_after": "2026-10-01", "created_before": "2026-10-01",
		}[p]
		if value == "" {
			value = "x"
		}
		if multi {
			value += "," + value
		}
		if err := s.Validate(url.Values{p: {value}}); err != nil {
			t.Errorf("%s=%s: %v", p, value, err)
		}
	}
}

func TestSurfaceCountReadsTheViewersZone(t *testing.T) {
	repo := &fakeCounter{}
	s := NewSurface(repo)
	kyiv, _ := time.LoadLocation("Europe/Kyiv")
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	views := []url.Values{{"updated": {"1d"}}, {"kind": {"pdf"}, "tz": {"UTC"}}}
	got, err := s.Count(t.Context(), views, now, kyiv)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0][CountTotal] != 10 || got[1][CountTotal] != 11 {
		t.Errorf("counts = %v", got)
	}
	if after := repo.got[0].UpdatedAfter; after == nil || !after.Equal(time.Date(2026, 10, 19, 0, 0, 0, 0, kyiv)) {
		t.Errorf("updated=1d starts at %v, want the start of today in Kyiv", after)
	}
	if loc := repo.got[1].Location; loc.String() != "Europe/Kyiv" {
		t.Errorf("location = %v, want the viewer's zone over a stored tz", loc)
	}
	if views[1].Get("tz") != "UTC" {
		t.Error("Count changed the caller's view")
	}
}

func TestSurfaceCountRejectsAStaleView(t *testing.T) {
	s := NewSurface(&fakeCounter{})
	if _, err := s.Count(t.Context(), []url.Values{{"kind": {"film"}}}, time.Now(), time.UTC); err == nil {
		t.Error("an unparseable view counted")
	}
}
