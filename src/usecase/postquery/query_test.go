package postquery

import (
	"errors"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
)

func TestParseQuery(t *testing.T) {
	v, _ := url.ParseQuery("status=draft,failed&status_not=published&platform=linkedin&post_type_not=thread" +
		"&campaign_id=c1&scheduled=yes&scheduled_after=2026-11-01&scheduled_before=2026-11-08T12:00:00Z&tz=Europe/Kyiv&unknown=1")
	q, err := ParseQuery(v)
	if err != nil {
		t.Fatal(err)
	}
	kyiv, _ := time.LoadLocation("Europe/Kyiv")
	if !slices.Equal(q.Status.Any, []string{"draft", "failed"}) || !slices.Equal(q.Status.Not, []string{"published"}) ||
		!slices.Equal(q.Platform.Any, []string{"linkedin"}) || !slices.Equal(q.PostType.Not, []string{"thread"}) ||
		!slices.Equal(q.Campaign.Any, []string{"c1"}) || q.Scheduled != models.PostScheduledYes {
		t.Fatalf("filters = %+v", q)
	}
	if !q.ScheduledAfter.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, kyiv)) ||
		!q.ScheduledBefore.Equal(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("days = %v .. %v", q.ScheduledAfter, q.ScheduledBefore)
	}

	empty, err := ParseQuery(url.Values{})
	if err != nil || empty.Status.Active() || empty.Scheduled != "" || empty.ScheduledAfter != nil || empty.Location != time.UTC {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
}

func TestParseQuery_Rejects(t *testing.T) {
	for _, tc := range []struct{ raw, param string }{
		{"status=lost", "status"},
		{"status_not=lost", "status"},
		{"scheduled=maybe", "scheduled"},
		{"scheduled_after=yesterday", "scheduled_after"},
		{"tz=Mars/Base", "tz"},
		{"scheduled=no&scheduled_before=2026-11-01", "scheduled"},
	} {
		raw, param := tc.raw, tc.param
		v, _ := url.ParseQuery(raw)
		_, err := ParseQuery(v)
		if le, ok := errors.AsType[*listquery.Error](err); !ok || le.Param != param {
			t.Fatalf("%s: err = %v, want a rejected %s", raw, err, param)
		}
	}
}
