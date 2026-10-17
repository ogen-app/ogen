package ideas

import (
	"context"
	"maps"
	"net/url"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// SurfaceName is the ideas list's saved-views surface.
const SurfaceName = "ideas"

// Counts a saved view of the ideas list can carry.
const (
	CountTotal = "total"
	// CountUndecided is the view's waiting pile.
	CountUndecided = "undecided"
)

// Surface is the ideas list as a saved-views surface.
type Surface struct {
	repo repository.IdeaRepository
}

// NewSurface returns the ideas surface over repo.
func NewSurface(repo repository.IdeaRepository) *Surface {
	return &Surface{repo: repo}
}

func (*Surface) Name() string { return SurfaceName }

func (*Surface) Version() int { return 1 }

func (*Surface) Params() map[string]bool { return Params }

func (*Surface) Volatile() []string { return VolatileParams }

func (*Surface) Upgrade(_ int, v url.Values) url.Values { return v }

func (*Surface) Validate(v url.Values) error {
	_, err := ParseQuery(v, time.Now().UTC())
	return err
}

func (*Surface) Counts() []string { return []string{CountTotal, CountUndecided} }

func (s *Surface) Count(ctx context.Context, views []url.Values, now time.Time, loc *time.Location) ([]map[string]int, error) {
	qs := make([]models.IdeaQuery, len(views))
	for i, v := range views {
		v = maps.Clone(v)
		v.Set("tz", loc.String())
		q, err := ParseQuery(v, now)
		if err != nil {
			return nil, err
		}
		qs[i] = q
	}
	got, err := s.repo.CountViews(ctx, qs)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]int, len(got))
	for i, c := range got {
		out[i] = map[string]int{CountTotal: c.Total, CountUndecided: c.Waiting}
	}
	return out, nil
}
