package assets

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
)

func TestParseQueryDefaults(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	q, err := ParseQuery(url.Values{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if q.Sort != models.AssetSortUpdatedDesc || q.Limit != DefaultPageSize || q.Group != "" || q.Location != time.UTC || !q.Now.Equal(now) {
		t.Errorf("defaults = %+v", q)
	}
}

func TestParseQueryReadsEveryFilter(t *testing.T) {
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	sum := strings.Repeat("Ab", 32)
	v, _ := url.ParseQuery("title=bee,hive&title=bee&title_not=draft&kind=pdf,web&kind_not=audio" +
		"&status=ready&status_not=failed&campaign=c1,none&campaign_not=c2&tag=t1&tag_not=none" +
		"&ideas=undecided&ideas_not=unread&checksum=" + sum + "&text=+bees+&tz=Europe/Kyiv" +
		"&updated=7d&updated_before=2026-10-20&created_after=2026-10-01T00:00:00Z&created_before=2026-10-08" +
		"&scope_campaign=c1&sort=relevance&group=tag&limit=20&cursor=abc&facets=kind,tag")
	q, err := ParseQuery(v, now)
	if err != nil {
		t.Fatal(err)
	}
	kyiv, _ := time.LoadLocation("Europe/Kyiv")
	switch {
	case !slices.Equal(q.Title.Any, []string{"bee", "hive"}) || !slices.Equal(q.Title.Not, []string{"draft"}):
		t.Errorf("title = %v, want repeated values merged without duplicates", q.Title)
	case !slices.Equal(q.Kind.Any, []string{"pdf", "web"}) || !slices.Equal(q.Kind.Not, []string{"audio"}):
		t.Errorf("kind = %v", q.Kind)
	case !slices.Equal(q.Status.Any, []string{"ready"}) || !slices.Equal(q.Status.Not, []string{"failed"}):
		t.Errorf("status = %v", q.Status)
	case !slices.Equal(q.Campaign.Any, []string{"c1", "none"}) || !slices.Equal(q.Campaign.Not, []string{"c2"}):
		t.Errorf("campaign = %v", q.Campaign)
	case !slices.Equal(q.Tag.Any, []string{"t1"}) || !slices.Equal(q.Tag.Not, []string{"none"}):
		t.Errorf("tag = %v", q.Tag)
	case !slices.Equal(q.Ideas.Any, []string{"undecided"}) || !slices.Equal(q.Ideas.Not, []string{"unread"}):
		t.Errorf("ideas = %v", q.Ideas)
	case !slices.Equal(q.Checksums, []string{strings.ToLower(sum)}):
		t.Errorf("checksums = %v, want lower-cased", q.Checksums)
	case q.Text != "bees":
		t.Errorf("text = %q", q.Text)
	case q.Location.String() != "Europe/Kyiv":
		t.Errorf("location = %v", q.Location)
	case q.UpdatedAfter == nil || !q.UpdatedAfter.Equal(time.Date(2026, 10, 13, 0, 0, 0, 0, kyiv)):
		t.Errorf("updated after = %v, want the start of six days ago in Kyiv", q.UpdatedAfter)
	case q.UpdatedBefore == nil || !q.UpdatedBefore.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, kyiv)):
		t.Errorf("updated before = %v, want the day's start in tz", q.UpdatedBefore)
	case q.CreatedAfter == nil || !q.CreatedAfter.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)):
		t.Errorf("created after = %v, want the instant as given", q.CreatedAfter)
	case q.CreatedBefore == nil || !q.CreatedBefore.Equal(time.Date(2026, 10, 8, 0, 0, 0, 0, kyiv)):
		t.Errorf("created before = %v", q.CreatedBefore)
	case q.ScopeCampaign != "c1" || q.Sort != models.AssetSortRelevance || q.Group != models.AssetGroupTag:
		t.Errorf("scope/sort/group = %q %q %q", q.ScopeCampaign, q.Sort, q.Group)
	case q.Limit != 20 || q.Cursor != "abc" || !slices.Equal(q.Facets, []string{"kind", "tag"}):
		t.Errorf("paging = %d %q %v", q.Limit, q.Cursor, q.Facets)
	}
}

// A day bound means its start in tz, so across a DST change the two bounds of
// a week sit at different UTC offsets.
func TestParseQueryDaysAcrossDST(t *testing.T) {
	v, _ := url.ParseQuery("updated_after=2026-10-24&updated_before=2026-10-26&tz=Europe/Kyiv")
	q, err := ParseQuery(v, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC); !q.UpdatedAfter.Equal(want) {
		t.Errorf("after = %v, want %v (UTC+3)", q.UpdatedAfter.UTC(), want)
	}
	if want := time.Date(2026, 10, 25, 22, 0, 0, 0, time.UTC); !q.UpdatedBefore.Equal(want) {
		t.Errorf("before = %v, want %v (UTC+2)", q.UpdatedBefore.UTC(), want)
	}
}

func TestParseQueryRejectsNamingTheParameter(t *testing.T) {
	for raw, param := range map[string]string{
		"kind=film":                           "kind",
		"kind_not=pdf,film":                   "kind",
		"status=archived":                     "status",
		"ideas=maybe":                         "ideas",
		"checksum=abc":                        "checksum",
		"sort=random":                         "sort",
		"sort=relevance":                      "sort",
		"group=status":                        "group",
		"limit=0":                             "limit",
		"limit=201":                           "limit",
		"facets=kind,verdict":                 "facets",
		"updated=yesterday":                   "updated",
		"updated=7d&updated_after=2026-10-01": "updated",
		"updated_before=soon":                 "updated_before",
		"created_after=2026-13-01":            "created_after",
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

func TestParseQueryTextBounds(t *testing.T) {
	for raw, code := range map[string]string{
		"text=a":                           models.AssetCodeQueryTooShort,
		"text=" + strings.Repeat("ж", 201): models.AssetCodeQueryTooLong,
	} {
		v, _ := url.ParseQuery(raw)
		_, err := ParseQuery(v, time.Now())
		qe, ok := errors.AsType[*listquery.Error](err)
		if !ok || qe.Param != "text" || qe.Code != code {
			t.Errorf("%.20q: err = %v, want %s on text", raw, err, code)
		}
	}
	v, _ := url.ParseQuery("text=" + strings.Repeat("ж", 200))
	if _, err := ParseQuery(v, time.Now()); err != nil {
		t.Errorf("200 characters: %v", err)
	}
}

func TestIDsOnly(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "ids_only=true": true, "ids_only=0": false} {
		v, _ := url.ParseQuery(raw)
		if got, err := IDsOnly(v); err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", raw, got, err, want)
		}
	}
	v, _ := url.ParseQuery("ids_only=maybe")
	if _, err := IDsOnly(v); err == nil {
		t.Error("ids_only=maybe: want an error")
	}
}
