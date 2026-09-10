package platforms

import (
	"context"
	"expvar"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// GlobalLimitsSource supplies the single platform_global_limits row (CON-292).
// It is satisfied by repository.PlatformGlobalLimitsRepository; declaring the
// interface here keeps the domain layer free of an infra import while letting
// boot inject the repo.
type GlobalLimitsSource interface {
	Get(ctx context.Context) (*models.PlatformGlobalLimits, error)
}

// globalLimitsRefreshInterval bounds the staleness window after an operator
// edits the ceilings when the explicit RefreshGlobalLimits call is missed.
const globalLimitsRefreshInterval = 60 * time.Second

var (
	// activeLimits holds the current ceilings. Reads are lock-free. Seeded with
	// the built-in defaults so callers get a usable value before InitGlobalLimits
	// runs and if the DB read ever fails.
	activeLimits atomic.Pointer[models.PlatformGlobalLimits]

	// limitsSource is set once by InitGlobalLimits at boot (before the refresh
	// goroutine and any gRPC-driven refresh), so plain access is race-free.
	limitsSource GlobalLimitsSource

	limitsRefreshOK   = expvar.NewInt("ogen_platform_global_limits_refresh_ok")
	limitsRefreshFail = expvar.NewInt("ogen_platform_global_limits_refresh_fail")
)

func init() {
	def := models.DefaultPlatformGlobalLimits()
	activeLimits.Store(&def)
}

// GlobalLimits returns the current cross-platform ceilings. Before
// InitGlobalLimits runs (and whenever a DB refresh fails) it returns the
// built-in defaults, so every caller always has a usable value.
func GlobalLimits() models.PlatformGlobalLimits {
	return *activeLimits.Load()
}

// InitGlobalLimits loads the ceilings from src at boot and starts a periodic
// refresh bound to ctx. Non-fatal: a failed initial load logs and keeps the
// built-in defaults so the app still starts (CON-292 §10.2).
func InitGlobalLimits(ctx context.Context, src GlobalLimitsSource) {
	limitsSource = src
	if err := RefreshGlobalLimits(ctx); err != nil {
		slog.ErrorContext(ctx, "initial platform global limits load failed; using built-in defaults",
			logging.AttrComponent, "platforms.limits", logging.AttrError, err)
	}
	go func() {
		t := time.NewTicker(globalLimitsRefreshInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := RefreshGlobalLimits(ctx); err != nil {
					slog.WarnContext(ctx, "platform global limits refresh failed; keeping previous values",
						logging.AttrComponent, "platforms.limits", logging.AttrError, err)
				}
			}
		}
	}()
}

// RefreshGlobalLimits reloads the ceilings now. PlatformAdminService calls it
// after UpdateGlobalLimits so an operator edit takes effect without waiting for
// the periodic tick. A failed reload keeps the previous values.
func RefreshGlobalLimits(ctx context.Context) error {
	src := limitsSource
	if src == nil {
		return nil
	}
	lim, err := src.Get(ctx)
	if err != nil {
		limitsRefreshFail.Add(1)
		return err
	}
	activeLimits.Store(lim)
	limitsRefreshOK.Add(1)
	return nil
}
