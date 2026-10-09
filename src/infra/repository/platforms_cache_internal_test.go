package repository

import (
	"context"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// countingPlatforms serves a fixed platform list and counts List calls.
type countingPlatforms struct {
	PlatformRepository
	rows  []models.Platform
	lists int
}

func (c *countingPlatforms) List(context.Context) ([]models.Platform, error) {
	c.lists++
	return c.rows, nil
}

func (c *countingPlatforms) SetEnabled(_ context.Context, id string, enabled bool) (*models.Platform, error) {
	for i := range c.rows {
		if c.rows[i].ID == id {
			c.rows[i].Enabled = enabled
			return &c.rows[i], nil
		}
	}
	return nil, nil
}

func TestCachedPlatformRepository(t *testing.T) {
	ctx := t.Context()
	inner := &countingPlatforms{rows: []models.Platform{{ID: "a", Enabled: true}, {ID: "b"}}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repo := NewCachedPlatformRepository(inner, time.Minute).(*cachedPlatformRepository)
	repo.now = func() time.Time { return now }

	for range 3 {
		if all, err := repo.List(ctx); err != nil || len(all) != 2 {
			t.Fatalf("List = (%d rows, %v), want 2", len(all), err)
		}
	}
	if enabled, err := repo.ListEnabled(ctx); err != nil || len(enabled) != 1 || enabled[0].ID != "a" {
		t.Fatalf("ListEnabled = (%v, %v), want [a]", enabled, err)
	}
	if inner.lists != 1 {
		t.Fatalf("inner List calls = %d, want 1 while the snapshot is fresh", inner.lists)
	}

	if _, err := repo.SetEnabled(ctx, "b", true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if enabled, _ := repo.ListEnabled(ctx); len(enabled) != 2 {
		t.Fatalf("ListEnabled after a write = %d rows, want 2 (write must drop the snapshot)", len(enabled))
	}

	now = now.Add(2 * time.Minute)
	if _, err := repo.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if inner.lists != 3 {
		t.Fatalf("inner List calls = %d, want 3 (initial, after write, after expiry)", inner.lists)
	}

	all, _ := repo.List(ctx)
	all[0].ID = "mutated"
	if again, _ := repo.List(ctx); again[0].ID != "a" {
		t.Fatal("a caller's edit to the returned slice reached the snapshot")
	}
}
