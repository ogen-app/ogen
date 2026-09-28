// Package settings holds small helpers over the generic key-value
// `settings` table that encode workspace-level configuration semantics
// (defaults, validation) the raw repository deliberately stays out of.
//
// The workspace timezone lives here: a single instance-level
// IANA zone name used to resolve relative scheduling expressions, echo
// times back to the user, and stamp the Zernio submit. The instant
// persisted on a post is always absolute UTC — the timezone only ever
// affects presentation and resolution, never storage.
package settings

import (
	"context"
	"time"

	"github.com/ogen-app/ogen/src/infra/repository"
)

// TimezoneKey is the `settings` row key holding the workspace IANA
// timezone name (e.g. "Europe/Kyiv"). Absent → UTC.
const TimezoneKey = "timezone"

// ResolveTimezone validates an IANA timezone name and returns the
// corresponding location. An empty name resolves to UTC (the documented
// default). Unknown zones return the error from time.LoadLocation so the
// settings handler can reject them with a 400.
func ResolveTimezone(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(name)
}

// WorkspaceTimezone reads the configured workspace timezone, falling
// back to UTC whenever the setting is unset, unreadable, empty, or
// invalid. It returns both the loaded location and its canonical name
// so callers can echo the zone to the user. It never errors — a missing
// or broken timezone setting must not block scheduling; it just means
// "treat times as UTC".
func WorkspaceTimezone(ctx context.Context, repo repository.SettingRepository) (*time.Location, string) {
	if repo == nil {
		return time.UTC, "UTC"
	}
	s, err := repo.GetByKey(ctx, TimezoneKey)
	if err != nil || s == nil || s.Value == "" {
		return time.UTC, "UTC"
	}
	loc, err := time.LoadLocation(s.Value)
	if err != nil {
		return time.UTC, "UTC"
	}
	return loc, s.Value
}
