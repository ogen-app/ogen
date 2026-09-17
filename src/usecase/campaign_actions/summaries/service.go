package summaries

import (
	"context"
	"fmt"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// Service builds the batched Campaigns-list summary from a tenant-scoped post
// repository (CON-152). The repository carries tenant scoping, so Summaries is
// safe to call in any tenant context.
type Service struct {
	posts repository.PostRepository
}

// New builds a Service.
func New(posts repository.PostRepository) *Service {
	return &Service{posts: posts}
}

// Summaries loads every post in the caller's tenant as a slim projection and
// groups it by campaign. One DB read replaces the N per-card
// GET /campaigns/:id/posts requests the Campaigns list used to fire.
func (s *Service) Summaries(ctx context.Context) (*Summaries, error) {
	posts, err := s.posts.ListSummaryProjections(ctx)
	if err != nil {
		return nil, fmt.Errorf("list post summaries: %w", err)
	}
	out := build(posts)
	out.GeneratedAt = time.Now().UTC()
	return out, nil
}

// build groups the projections by campaign, preserving first-seen order (the
// repository returns rows ordered by campaign_id, then created_at). Pure — no
// I/O — so it is unit-testable with hand-built input, like overview.buildOverview.
// GeneratedAt is stamped by the caller.
func build(posts []models.Post) *Summaries {
	order := make([]string, 0)
	byCampaign := make(map[string]*CampaignSummary)
	for _, p := range posts {
		cs, ok := byCampaign[p.CampaignID]
		if !ok {
			cs = &CampaignSummary{CampaignID: p.CampaignID, Posts: []PostSummary{}}
			byCampaign[p.CampaignID] = cs
			order = append(order, p.CampaignID)
		}
		cs.Posts = append(cs.Posts, projectPost(p))
	}
	out := &Summaries{Summaries: make([]CampaignSummary, 0, len(order))}
	for _, id := range order {
		out.Summaries = append(out.Summaries, *byCampaign[id])
	}
	return out
}

// projectPost narrows a Post to the fields the readiness rules read. media_urls
// is normalised to a non-nil slice so it serialises as [] rather than null,
// matching the REST Post contract the client expects.
func projectPost(p models.Post) PostSummary {
	media := []string(p.MediaURLs)
	if media == nil {
		media = []string{}
	}
	return PostSummary{
		ID:                  p.ID,
		CampaignID:          p.CampaignID,
		Status:              string(p.Status),
		ScheduledAt:         p.ScheduledAt,
		PublishedAt:         p.PublishedAt,
		PlatformID:          p.PlatformID,
		PlatformPostType:    p.PlatformPostType,
		CampaignTypePhaseID: p.CampaignTypePhaseID,
		MediaURLs:           media,
		CreatedBy:           p.CreatedBy,
		FailureReason:       p.FailureReason,
		CreatedAt:           p.CreatedAt,
		UpdatedAt:           p.UpdatedAt,
	}
}
