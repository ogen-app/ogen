package assets

import (
	"context"
	"maps"
	"net/url"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// SurfaceName is the asset list's saved-views surface.
const SurfaceName = "assets"

// CountTotal is the one count a saved view of the asset list carries.
const CountTotal = "total"

// ViewCounter counts the matches of many asset list queries in one round
// trip; repository.AssetRepository is one.
type ViewCounter interface {
	CountViews(ctx context.Context, qs []models.AssetQuery) ([]models.AssetViewCounts, error)
}

// Surface is the asset list as a saved-views surface.
type Surface struct {
	repo ViewCounter
}

// NewSurface returns the assets surface over repo.
func NewSurface(repo ViewCounter) *Surface {
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

func (*Surface) Counts() []string { return []string{CountTotal} }

func (s *Surface) Count(ctx context.Context, views []url.Values, now time.Time, loc *time.Location) ([]map[string]int, error) {
	qs := make([]models.AssetQuery, len(views))
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
		out[i] = map[string]int{CountTotal: c.Total}
	}
	return out, nil
}
