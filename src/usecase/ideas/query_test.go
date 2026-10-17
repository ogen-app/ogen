package ideas

import (
	"errors"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
)

func TestParseQueryDefaults(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	q, err := ParseQuery(url.Values{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Sort != models.IdeaSortCreated || q.Limit != UnpagedLimit || q.Group != "" || q.Location != time.UTC {
		t.Errorf("defaults = %+v", q)
	}
}

func TestParseQueryReadsEveryFilter(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	v, _ := url.ParseQuery("pile=waiting,yes&pile=yes&origin_not=manual&source_asset=a1,none&asset_id=a2" +
		"&kind=pdf&campaign_id=c1&fit=high&phase=Launch&author=u1&text=+bees+&created=7d&tz=Europe/Kyiv" +
		"&created_before=2026-10-20&scope_campaign=c1&sort=fit&group=source&limit=50&cursor=abc&facets=origin,kind")
	q, err := ParseQuery(v, now)
	if err != nil {
		t.Fatal(err)
	}
	kyiv, _ := time.LoadLocation("Europe/Kyiv")
	switch {
	case !slices.Equal(q.Pile.Any, []string{"waiting", "yes"}):
		t.Errorf("pile = %v, want repeated values merged without duplicates", q.Pile)
	case !slices.Equal(q.Origin.Not, []string{"manual"}):
		t.Errorf("origin = %v", q.Origin)
	case !slices.Equal(q.SourceAsset.Any, []string{"a1", "none", "a2"}):
		t.Errorf("source_asset = %v, want asset_id folded in", q.SourceAsset)
	case q.Text != "bees":
		t.Errorf("text = %q", q.Text)
	case q.CreatedAfter == nil || !q.CreatedAfter.Equal(time.Date(2026, 10, 11, 0, 0, 0, 0, kyiv)):
		t.Errorf("created after = %v, want the start of six days ago in Kyiv", q.CreatedAfter)
	case q.CreatedBefore == nil || !q.CreatedBefore.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, kyiv)):
		t.Errorf("created before = %v, want the date's start in tz", q.CreatedBefore)
	case q.ScopeCampaign != "c1" || q.Sort != models.IdeaSortFit || q.Group != models.IdeaGroupSource:
		t.Errorf("scope/sort/group = %q %q %q", q.ScopeCampaign, q.Sort, q.Group)
	case q.Limit != 50 || q.Cursor != "abc" || !slices.Equal(q.Facets, []string{"origin", "kind"}):
		t.Errorf("paging = %d %q %v", q.Limit, q.Cursor, q.Facets)
	}
}

func TestParseQueryRejectsNamingTheParameter(t *testing.T) {
	for raw, param := range map[string]string{
		"pile=someday":                        "pile",
		"kind_not=pdf,film":                   "kind",
		"fit=great":                           "fit",
		"origin=robot":                        "origin",
		"sort=random":                         "sort",
		"group=tag":                           "group",
		"limit=0":                             "limit",
		"limit=201":                           "limit",
		"facets=origin,verdict":               "facets",
		"created=yesterday":                   "created",
		"created=7d&created_after=2026-10-01": "created",
		"created_before=soon":                 "created_before",
		"tz=Mars/Base":                        "tz",
	} {
		v, _ := url.ParseQuery(raw)
		_, err := ParseQuery(v, time.Now())
		qe, ok := errors.AsType[*listquery.Error](err)
		if !ok || qe.Param != param || qe.Code != listquery.CodeInvalidFilter {
			t.Errorf("%q: err = %v, want invalid_filter on %s", raw, err, param)
		}
	}
}
