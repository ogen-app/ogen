// Package savedviews stores named addresses of filterable lists: smart
// groups. It knows nothing about any one list. Each list registers a Surface,
// which owns its parameters, its parser and its counts, so a new list is one
// registration with no migration and no new endpoint.
package savedviews

import (
	"context"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Surface is one filterable list a view can be saved for.
type Surface interface {
	// Name is the surface's id in the API and the saved_views table.
	Name() string
	// Version is the current format of the surface's query strings. Bump it
	// when a parameter is renamed or retired, and teach Upgrade the change.
	Version() int
	// Params lists the parameters of a view's address, each with whether it
	// takes a comma-separated list.
	Params() map[string]bool
	// Volatile lists the parameters a list request carries that a view drops
	// silently: paging, display options, the viewer's time zone.
	Volatile() []string
	// Upgrade rewrites a query of an older version into the current one.
	Upgrade(from int, v url.Values) url.Values
	// Validate parses v as the list's endpoint does; its error names the
	// rejected parameter as a *listquery.Error.
	Validate(v url.Values) error
	// Counts lists the names of the counts Count returns.
	Counts() []string
	// Count runs each view's query and returns every count of Counts for
	// each, in one round trip. The date presets read loc.
	Count(ctx context.Context, views []url.Values, now time.Time, loc *time.Location) ([]map[string]int, error)
}

// canonical turns a query string into the surface's canonical form: volatile
// parameters dropped, multi-valued ones split on commas, trimmed, sorted and
// de-duplicated, empty ones dropped, keys sorted. Commas between values stay
// literal so the string reads like the address it came from.
func canonical(s Surface, raw string) (url.Values, string, error) {
	in, err := url.ParseQuery(raw)
	if err != nil {
		return nil, "", &QueryError{Msg: "query is not a valid query string"}
	}
	params, volatile := s.Params(), s.Volatile()
	out := url.Values{}
	for key, vals := range in {
		if slices.Contains(volatile, key) {
			continue
		}
		multi, ok := params[key]
		if !ok {
			return nil, "", &QueryError{Param: key, Msg: key + ": unknown parameter"}
		}
		if multi {
			out[key] = splitValues(vals)
		} else if v := strings.TrimSpace(vals[len(vals)-1]); v != "" {
			out[key] = []string{v}
		}
		if len(out[key]) == 0 {
			delete(out, key)
		}
	}
	return out, encode(out), nil
}

func splitValues(vals []string) []string {
	var out []string
	for _, raw := range vals {
		for v := range strings.SplitSeq(raw, ",") {
			if v = strings.TrimSpace(v); v != "" {
				out = append(out, v)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// encode writes v with keys in order and each key's values comma-joined.
func encode(v url.Values) string {
	var b strings.Builder
	for _, key := range slices.Sorted(maps.Keys(v)) {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(key))
		b.WriteByte('=')
		for i, val := range v[key] {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(url.QueryEscape(val))
		}
	}
	return b.String()
}
