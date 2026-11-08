package campaign_assistant

import (
	"slices"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/scheduling"
)

// Redistributed posts land at the campaign's publishing time in its
// timezone, never on an instant another post already holds; a post already
// on its date there stays put.
func TestRedistributeMoves(t *testing.T) {
	c := &models.Campaign{
		StartDate:      new(day("2026-01-01")),
		EndDate:        new(day("2026-01-01")),
		Timezone:       "Europe/Kyiv", // UTC+2 in January
		PublishingTime: "10:00",
		PublishingDays: scheduling.DefaultPublishingDays(),
	}
	published := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	onDate := time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC) // 01:30 on 1 January in Kyiv
	posts := []models.Post{
		{ID: "pub", Status: models.PostStatusPublished, ScheduledAt: &published},
		{ID: "d1", Status: models.PostStatusDraft},
		{ID: "d2", Status: models.PostStatusDraft},
		{ID: "kept", Status: models.PostStatusDraft, ScheduledAt: &onDate},
	}

	batch, phaseOf := redistributeMoves(c, posts)
	var ids []string
	var at []time.Time
	for _, m := range batch {
		if m.From != nil {
			t.Errorf("%s: from = %v, want unscheduled", m.PostID, m.From)
		}
		ids, at = append(ids, m.PostID), append(at, m.To)
	}
	slices.Sort(ids)
	slices.SortFunc(at, time.Time.Compare)
	if !slices.Equal(ids, []string{"d1", "d2"}) {
		t.Fatalf("moved = %v, want d1 and d2 (kept is on its date in Kyiv)", ids)
	}
	want := []time.Time{published.Add(time.Minute), published.Add(2 * time.Minute)}
	if !slices.EqualFunc(at, want, time.Time.Equal) {
		t.Fatalf("instants = %v, want %v: 10:00 Kyiv, off the published post's minute", at, want)
	}
	if phaseOf["d1"] != "" || len(phaseOf) != 2 {
		t.Fatalf("phases = %v", phaseOf)
	}
}

func TestFreeInstant(t *testing.T) {
	at := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	if got := freeInstant(at, map[time.Time]bool{}); !got.Equal(at) {
		t.Fatalf("free = %v, want %v", got, at)
	}
	full := map[time.Time]bool{}
	for i := range maxSameTimeNudge {
		full[at.Add(time.Duration(i)*time.Minute)] = true
	}
	if got := freeInstant(at, full); !got.Equal(at) {
		t.Fatalf("all taken = %v, want the instant itself", got)
	}
}
