package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/entitlements"
	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/secrets"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// New wires the API server and returns the Fiber app, ready to Listen. It
// starts background work (River workers, the Zernio sync worker, catalog
// refresh loops) on ctx, which must outlive the request lifecycle; those are
// stopped by the app's shutdown hooks. cipher is the secret store's KEK
// cipher, reused to seal Zernio connect tokens at rest.
func New(ctx context.Context, db, analyticsDB *bun.DB, cfg *config.Config, secretStore secrets.Store, cipher *envelope.Cipher, hub eventhub.Hub) (*fiber.App, error) {
	// Container-internal diagnostics only.
	if cfg.EnablePprof {
		startPprof("localhost:6060")
	}

	plan := &shutdownPlan{}
	plan.add(stageDrainHandlers, drainHandlerBackground)

	d := newDeps(db, analyticsDB, cfg, secretStore, cipher, hub, plan)
	loadOperatorCatalogs(ctx, cfg, d.r)
	for _, phase := range d.phases() {
		if err := phase(ctx); err != nil {
			return nil, err
		}
	}

	app := newFiberApp(cfg)
	registerRoutes(app, d)
	plan.register(app)
	return app, nil
}

func defaultErrorHandler(c *fiber.Ctx, err error) error {
	// Render entitlement denials as structured bodies.
	if qe, ok := errors.AsType[*entitlements.QuotaExceededError](err); ok {
		return c.Status(fiber.StatusPaymentRequired).JSON(fiber.Map{
			"error": "entitlement_exceeded", "feature": qe.Key, "limit": qe.Limit, "current": qe.Current,
		})
	}
	if fe, ok := errors.AsType[*entitlements.FeatureNotAvailableError](err); ok {
		return c.Status(fiber.StatusForbidden).JSON(fiber.Map{"error": "feature_not_available", "feature": fe.Key})
	}
	code := fiber.StatusInternalServerError
	if e, ok := errors.AsType[*fiber.Error](err); ok {
		code = e.Code
	}
	// Server faults are logged and reported so they are never hidden behind
	// the JSON body; 4xx is the client's problem and shows only in the access log.
	if code >= 500 {
		slog.ErrorContext(c.Context(), "request failed",
			logging.AttrComponent, "http",
			"method", c.Method(),
			"path", c.Path(),
			"status", code,
			logging.AttrError, err)
		// Skips panics already captured at recovery time.
		reportServerError(c, err, code)
	}
	return c.Status(code).JSON(fiber.Map{"error": err.Error()})
}

// accessLog emits exactly one structured line per request, replacing Fiber's
// default text logger. It runs after the handler so the request,
// tenant, and user ids set by downstream middleware are attached by the slog
// ContextHandler via c.Context(). Like Fiber's own logger middleware it invokes
// the app ErrorHandler when the chain returns an error, so the logged status
// reflects the final response (e.g. 5xx) rather than the pre-handler default.
func accessLog() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		chainErr := c.Next()
		if chainErr != nil {
			if herr := c.App().ErrorHandler(c, chainErr); herr != nil {
				_ = c.SendStatus(fiber.StatusInternalServerError)
			}
		}

		status := c.Response().StatusCode()
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}
		// Streaming responses (SetBodyStreamWriter — e.g. the SSE event stream)
		// have no materialised body. Calling c.Response().Body() on one drains
		// the stream to EOF to buffer it; for a long-lived/infinite stream that
		// never returns, blocking the response from ever being flushed (the SSE
		// client then hangs forever in "connecting"). Skip the byte count there.
		respBytes := 0
		if !c.Response().IsBodyStream() {
			respBytes = len(c.Response().Body())
		}
		slog.Default().LogAttrs(c.Context(), level, "request",
			slog.String(logging.AttrComponent, "http"),
			slog.String("method", c.Method()),
			slog.String("path", c.Path()),
			slog.Int("status", status),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
			slog.Int("bytes", respBytes),
			slog.String("ip", c.IP()),
		)
		return nil
	}
}
