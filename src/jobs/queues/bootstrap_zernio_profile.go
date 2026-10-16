package queues

// BootstrapZernioProfileQueue eagerly provisions a tenant's Zernio profile at
// registration time. Signup enqueues one task per new tenant
// inside its transaction (transactional outbox), so the profile is created in
// the background without ever blocking the signup request on Zernio. The lazy
// on-connect bootstrap (handlers/zernio.go) remains the guaranteed fallback, so
// this job is best-effort: when Zernio is unreachable or unconfigured it gives
// up cleanly rather than holding the tenant hostage.
const BootstrapZernioProfileQueue = "bootstrap_zernio_profile"

// BootstrapZernioProfileTask carries the tenant whose profile to provision.
type BootstrapZernioProfileTask struct {
	TenantID string `json:"tenant_id"`
}

// Kind implements JobArgs.
func (BootstrapZernioProfileTask) Kind() string { return BootstrapZernioProfileQueue }
