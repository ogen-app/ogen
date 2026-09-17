package summaries

import (
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

func ptr(s string) *string { return &s }

// TestBuild_GroupsByCampaignPreservingOrder checks that build folds a flat,
// campaign-ordered row set into per-campaign groups in first-seen order, and
// keeps each campaign's posts in input order.
func TestBuild_GroupsByCampaignPreservingOrder(t *testing.T) {
	posts := []models.Post{
		{ID: "a1", CampaignID: "camp-a", Status: models.PostStatusDraft, MediaURLs: models.StringSlice{}},
		{ID: "a2", CampaignID: "camp-a", Status: models.PostStatusScheduled, MediaURLs: models.StringSlice{}},
		{ID: "b1", CampaignID: "camp-b", Status: models.PostStatusFailed, MediaURLs: models.StringSlice{}},
	}

	out := build(posts)

	if len(out.Summaries) != 2 {
		t.Fatalf("want 2 campaigns, got %d (%+v)", len(out.Summaries), out.Summaries)
	}
	if out.Summaries[0].CampaignID != "camp-a" || out.Summaries[1].CampaignID != "camp-b" {
		t.Fatalf("first-seen order not preserved: %+v", out.Summaries)
	}
	if len(out.Summaries[0].Posts) != 2 || out.Summaries[0].Posts[0].ID != "a1" || out.Summaries[0].Posts[1].ID != "a2" {
		t.Fatalf("camp-a posts wrong: %+v", out.Summaries[0].Posts)
	}
	if len(out.Summaries[1].Posts) != 1 || out.Summaries[1].Posts[0].ID != "b1" {
		t.Fatalf("camp-b posts wrong: %+v", out.Summaries[1].Posts)
	}
	// build leaves GeneratedAt zero — the caller (Service.Summaries) stamps it.
	if !out.GeneratedAt.IsZero() {
		t.Fatalf("GeneratedAt should be stamped by the caller, got %v", out.GeneratedAt)
	}
}

// TestBuild_ProjectsReadinessFields checks every field the readiness rules read
// is carried through, and that a nil media slice becomes [] (not null).
func TestBuild_ProjectsReadinessFields(t *testing.T) {
	at := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	pub := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 7, 28, 8, 30, 0, 0, time.UTC)

	out := build([]models.Post{{
		ID:                  "po_1",
		CampaignID:          "camp-a",
		Status:              models.PostStatusScheduled,
		ScheduledAt:         &at,
		PublishedAt:         &pub,
		PlatformID:          "pl_li",
		PlatformPostType:    "feed",
		CampaignTypePhaseID: ptr("ph_2"),
		MediaURLs:           nil, // notnull column, but guard the nil→[] normalisation
		CreatedBy:           "usr_ana",
		FailureReason:       "zernio_rejected",
		CreatedAt:           created,
		UpdatedAt:           updated,
		// Heavy fields that must NOT influence the projection.
		Title:   "should be dropped",
		Content: "should be dropped",
	}})

	got := out.Summaries[0].Posts[0]
	if got.ID != "po_1" || got.CampaignID != "camp-a" || got.Status != "scheduled" {
		t.Fatalf("scalar fields wrong: %+v", got)
	}
	if got.ScheduledAt == nil || !got.ScheduledAt.Equal(at) || got.PublishedAt == nil || !got.PublishedAt.Equal(pub) {
		t.Fatalf("time fields wrong: %+v", got)
	}
	if got.PlatformID != "pl_li" || got.PlatformPostType != "feed" {
		t.Fatalf("platform fields wrong: %+v", got)
	}
	if got.CampaignTypePhaseID == nil || *got.CampaignTypePhaseID != "ph_2" {
		t.Fatalf("phase id wrong: %+v", got.CampaignTypePhaseID)
	}
	if got.MediaURLs == nil || len(got.MediaURLs) != 0 {
		t.Fatalf("media should normalise to non-nil empty slice, got %#v", got.MediaURLs)
	}
	if !got.CreatedAt.Equal(created) || !got.UpdatedAt.Equal(updated) {
		t.Fatalf("timestamps wrong: %+v", got)
	}
	if got.CreatedBy != "usr_ana" || got.FailureReason != "zernio_rejected" {
		t.Fatalf("authorship/failure fields wrong: %+v", got)
	}
}

// TestBuild_Empty returns an empty, non-nil summary list.
func TestBuild_Empty(t *testing.T) {
	out := build(nil)
	if out.Summaries == nil {
		t.Fatalf("Summaries should be a non-nil empty slice, got nil")
	}
	if len(out.Summaries) != 0 {
		t.Fatalf("want 0 campaigns, got %d", len(out.Summaries))
	}
}
