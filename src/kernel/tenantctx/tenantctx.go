// Package tenantctx carries the active tenant id through a request- or
// job-scoped context.Context. It is the single mechanism the tenant-scoped
// query layer (CON-97 §6) uses to discover which tenant a query belongs to.
//
// On the request path the Fiber auth middleware stores the tenant via
// c.Locals(tenantctx.Key, id); fasthttp exposes user values through
// (*RequestCtx).Value, so From() reads it back via ctx.Value without having to
// thread c.UserContext() through all ~150 handler→repo call sites. On the
// background-job path there is no request context, so workers rebuild the
// context with With() from the tenant_id carried in the job args.
package tenantctx

import (
	"context"
	"errors"
)

// ctxKey is an unexported type so the context value cannot collide with keys
// set by other packages. It is shared with Fiber's c.Locals on the request
// path (see package doc).
type ctxKey struct{}

// systemKey marks a context as a system (intentionally cross-tenant) context.
type systemKey struct{}

// tierKey carries the caller's tenant tier id (CON-308) for per-tier model
// resolution. Optional: absent means "resolve the global default".
type tierKey struct{}

// Key is the context/locals key under which the tenant id is stored. It is
// exported (as a value of an unexported type) so the Fiber auth middleware can
// call c.Locals(tenantctx.Key, id) and have From() read it back, while callers
// still cannot forge the key type.
var Key = ctxKey{}

// ErrNoTenant is returned by the tenant-scoping query hooks (CON-97 §6/§12.1)
// when a tenant-owned query runs without a tenant in context and the context
// is not a system context. The query fails closed rather than touching another
// tenant's rows.
var ErrNoTenant = errors.New("tenantctx: no tenant in context (refusing to run an unscoped tenant query)")

// WithSystem marks ctx as a system context: tenant-scoping hooks skip filtering
// and trust the caller-set tenant_id on writes. Use ONLY for genuinely
// cross-tenant work — backfills, migrations, seeds, and (interim, until they
// become per-tenant) background jobs. Auditable by grepping for WithSystem.
func WithSystem(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemKey{}, true)
}

// IsSystem reports whether ctx was marked by WithSystem.
func IsSystem(ctx context.Context) bool {
	v, _ := ctx.Value(systemKey{}).(bool)
	return v
}

// With returns a copy of ctx carrying the given tenant id.
func With(ctx context.Context, tenantID string) context.Context {
	return context.WithValue(ctx, Key, tenantID)
}

// From returns the tenant id carried by ctx and whether a non-empty one was
// present. Callers that enforce isolation must treat (_, false) as fail-closed
// (CON-97 §6) — never run an unscoped query.
func From(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(Key).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// WithTier returns a copy of ctx carrying the caller's tenant tier id, for
// per-tier model resolution (CON-308). Optional — the model resolver falls back
// to the global default when no tier is present.
func WithTier(ctx context.Context, tierID string) context.Context {
	return context.WithValue(ctx, tierKey{}, tierID)
}

// TierFrom returns the tier id carried by ctx and whether a non-empty one was
// present. Its signature matches modelconfig.TierFunc so it can be injected
// directly.
func TierFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(tierKey{}).(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}
