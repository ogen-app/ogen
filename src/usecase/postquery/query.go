// Package postquery reads the posts list's query string: the filter of a
// campaign's posts list, the workspace posts list and the calendar, the same
// way for the endpoints that serve them and for the assistant.
package postquery

import (
	"net/url"
	"time"

	"github.com/ogen-app/ogen/src/domain/listquery"
	"github.com/ogen-app/ogen/src/domain/models"
)

// ParseQuery reads the posts list parameters. It rejects an unknown value with
// a *listquery.Error; parameters it does not know are ignored.
func ParseQuery(v url.Values) (models.PostQuery, error) {
	q := models.PostQuery{Location: time.UTC}
	var err error
	if q.Location, err = listquery.Location(v); err != nil {
		return q, err
	}
	for _, f := range []struct {
		dst     *models.AssetValueFilter
		key     string
		allowed []string
	}{
		{&q.Status, "status", models.PostStatuses},
		{&q.Platform, "platform", nil},
		{&q.PostType, "post_type", nil},
		{&q.Campaign, "campaign_id", nil},
	} {
		if *f.dst, err = listquery.Filter(v, f.key, f.allowed); err != nil {
			return q, err
		}
	}
	if q.Scheduled, err = listquery.OneOf(v, "scheduled", models.PostScheduledValues, ""); err != nil {
		return q, err
	}
	if q.ScheduledAfter, err = listquery.Time(v, "scheduled_after", q.Location); err != nil {
		return q, err
	}
	if q.ScheduledBefore, err = listquery.Time(v, "scheduled_before", q.Location); err != nil {
		return q, err
	}
	if q.Scheduled == models.PostScheduledNo && (q.ScheduledAfter != nil || q.ScheduledBefore != nil) {
		return q, listquery.Invalid("scheduled", "no cannot be combined with scheduled_after or scheduled_before")
	}
	return q, nil
}
