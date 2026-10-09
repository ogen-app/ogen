package repository

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// cachedPlatformRepository serves List and ListEnabled from a snapshot of the
// platforms table. The table is a handful of rows edited only by operators,
// but flows and handlers list it several times per request. Writes through
// this repository drop the snapshot at once; another replica's edit shows
// after at most ttl, the same staleness window the Zernio platform catalog
// has.
type cachedPlatformRepository struct {
	PlatformRepository
	ttl time.Duration
	now func() time.Time

	mu        sync.Mutex
	all       []models.Platform
	expiresAt time.Time
}

// NewCachedPlatformRepository wraps inner so its listings are cached for ttl.
func NewCachedPlatformRepository(inner PlatformRepository, ttl time.Duration) PlatformRepository {
	return &cachedPlatformRepository{PlatformRepository: inner, ttl: ttl, now: time.Now}
}

func (r *cachedPlatformRepository) List(ctx context.Context) ([]models.Platform, error) {
	all, err := r.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Clone(all), nil
}

func (r *cachedPlatformRepository) ListEnabled(ctx context.Context) ([]models.Platform, error) {
	all, err := r.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	enabled := make([]models.Platform, 0, len(all))
	for i := range all {
		if all[i].Enabled {
			enabled = append(enabled, all[i])
		}
	}
	return enabled, nil
}

// snapshot returns the cached rows, reloading them once they expire. The lock
// is held across the reload so concurrent misses share one query.
func (r *cachedPlatformRepository) snapshot(ctx context.Context) ([]models.Platform, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.all != nil && r.now().Before(r.expiresAt) {
		return r.all, nil
	}
	all, err := r.PlatformRepository.List(ctx)
	if err != nil {
		return nil, err
	}
	if all == nil {
		all = []models.Platform{}
	}
	r.all, r.expiresAt = all, r.now().Add(r.ttl)
	return all, nil
}

func (r *cachedPlatformRepository) invalidate() {
	r.mu.Lock()
	r.all = nil
	r.mu.Unlock()
}

func (r *cachedPlatformRepository) Create(ctx context.Context, p *models.Platform) error {
	defer r.invalidate()
	return r.PlatformRepository.Create(ctx, p)
}

func (r *cachedPlatformRepository) Update(ctx context.Context, p *models.Platform) error {
	defer r.invalidate()
	return r.PlatformRepository.Update(ctx, p)
}

func (r *cachedPlatformRepository) SetEnabled(ctx context.Context, id string, enabled bool) (*models.Platform, error) {
	defer r.invalidate()
	return r.PlatformRepository.SetEnabled(ctx, id, enabled)
}

func (r *cachedPlatformRepository) Delete(ctx context.Context, id string) (bool, error) {
	defer r.invalidate()
	return r.PlatformRepository.Delete(ctx, id)
}
