// Package listquery reads the query strings of the filterable lists: a list's
// address is its query string, so the endpoint that serves the list and a
// saved view of it parse the same url.Values the same way.
//
// The conventions are the asset list's: a parameter takes a comma-separated
// list meaning "any of these", its `_not` twin excludes values, and
// repeating a parameter adds to its list.
package listquery

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// Error is a rejected parameter. Param names it; Code is the machine-readable
// code of the 400.
type Error struct {
	Code  string
	Param string
	Msg   string
}

func (e *Error) Error() string { return e.Msg }

// Codes of an Error.
const (
	CodeInvalidFilter = models.AssetCodeInvalidFilter
	CodeInvalidCursor = models.AssetCodeInvalidCursor
)

// Invalid is an invalid_filter Error on param.
func Invalid(param, format string, args ...any) *Error {
	return &Error{Code: CodeInvalidFilter, Param: param, Msg: param + ": " + fmt.Sprintf(format, args...)}
}

// NotSuffix is the suffix of a parameter's excluding twin.
const NotSuffix = "_not"

// Values returns a parameter's values: every occurrence split on commas,
// trimmed, without blanks or duplicates.
func Values(v url.Values, key string) []string {
	var out []string
	for _, raw := range v[key] {
		for s := range strings.SplitSeq(raw, ",") {
			if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// Single returns a parameter's last value, trimmed.
func Single(v url.Values, key string) string {
	vals := v[key]
	if len(vals) == 0 {
		return ""
	}
	return strings.TrimSpace(vals[len(vals)-1])
}

// Filter reads key and key_not, rejecting values outside allowed when it is
// non-nil.
func Filter(v url.Values, key string, allowed []string) (models.AssetValueFilter, error) {
	f := models.AssetValueFilter{Any: Values(v, key), Not: Values(v, key+NotSuffix)}
	if allowed != nil {
		for _, s := range slices.Concat(f.Any, f.Not) {
			if !slices.Contains(allowed, s) {
				return f, Invalid(key, "unknown value %q (allowed: %s)", s, strings.Join(allowed, ", "))
			}
		}
	}
	return f, nil
}

// OneOf reads a single-valued parameter, rejecting a value outside allowed;
// def when absent.
func OneOf(v url.Values, key string, allowed []string, def string) (string, error) {
	s := Single(v, key)
	if s == "" {
		return def, nil
	}
	if !slices.Contains(allowed, s) {
		return "", Invalid(key, "unknown value %q (allowed: %s)", s, strings.Join(allowed, ", "))
	}
	return s, nil
}

// Location reads tz, UTC when absent.
func Location(v url.Values) (*time.Location, error) {
	tz := Single(v, "tz")
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, Invalid("tz", "unknown time zone %q", tz)
	}
	return loc, nil
}

// Time reads an RFC 3339 timestamp, or a YYYY-MM-DD date meaning its start in
// loc; nil when absent.
func Time(v url.Values, key string, loc *time.Location) (*time.Time, error) {
	raw := Single(v, key)
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return &t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, raw, loc); err == nil {
		return &t, nil
	}
	return nil, Invalid(key, "must be an RFC 3339 timestamp or a YYYY-MM-DD date")
}

// DateWindow reads key_after and key_before, or the preset key, which starts
// the window at the preset in loc.
func DateWindow(v url.Values, key string, now time.Time, loc *time.Location) (after, before *time.Time, err error) {
	if after, err = Time(v, key+"_after", loc); err != nil {
		return nil, nil, err
	}
	if before, err = Time(v, key+"_before", loc); err != nil {
		return nil, nil, err
	}
	preset := Single(v, key)
	if preset == "" {
		return after, before, nil
	}
	if after != nil {
		return nil, nil, Invalid(key, "cannot be combined with %s_after", key)
	}
	since, ok := models.AssetDateSince(preset, now, loc)
	if !ok {
		return nil, nil, Invalid(key, "unknown value %q (allowed: %s)", preset, strings.Join(models.AssetDatePresets, ", "))
	}
	return &since, before, nil
}
