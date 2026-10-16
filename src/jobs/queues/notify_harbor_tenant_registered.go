package queues

// NotifyHarborQueue delivers the "a new tenant registered" event to Harbor.
// The signup transaction enqueues one; river-service POSTs the signed body to
// Harbor's inbound webhook, so signup never blocks on Harbor reachability.
const NotifyHarborQueue = "notify_harbor_tenant_registered"

// NotifyHarborTenantRegisteredTask carries only the tenant id; Harbor re-derives
// everything else by calling back the send RPC (which re-loads the tenant).
type NotifyHarborTenantRegisteredTask struct {
	TenantID string `json:"tenant_id"`
}

// Kind implements JobArgs.
func (NotifyHarborTenantRegisteredTask) Kind() string { return NotifyHarborQueue }
