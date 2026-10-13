package models

import (
	"testing"
	"time"
)

func TestAssetUpdatedSince(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 02:30 UTC on 2026-10-03 is 22:30 on 2026-10-02 in New York.
	now := time.Date(2026, 10, 3, 2, 30, 0, 0, time.UTC)
	cases := map[string]time.Time{
		AssetUpdatedToday: time.Date(2026, 10, 2, 0, 0, 0, 0, loc),
		AssetUpdatedWeek:  time.Date(2026, 9, 26, 0, 0, 0, 0, loc),
		AssetUpdated30d:   time.Date(2026, 9, 3, 0, 0, 0, 0, loc),
		AssetUpdatedMonth: time.Date(2026, 10, 1, 0, 0, 0, 0, loc),
	}
	for preset, want := range cases {
		got, ok := AssetUpdatedSince(preset, now, loc)
		if !ok || !got.Equal(want) {
			t.Errorf("%s: got %v (%v), want %v", preset, got, ok, want)
		}
	}
	if _, ok := AssetUpdatedSince("2d", now, loc); ok {
		t.Error("an unknown preset is accepted")
	}
}
