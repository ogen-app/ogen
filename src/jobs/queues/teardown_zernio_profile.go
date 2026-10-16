package queues

// TeardownZernioProfileQueue tears down a tenant's Zernio profile when its
// workspace is deleted (CON-203, follow-up to CON-147 PR4). WorkspacesHandler.Delete
// enqueues one task inside its soft-delete transaction (transactional outbox),
// so the teardown exists iff the workspace was actually deleted and the delete
// HTTP response never blocks on Zernio. It is the mirror image of
// BootstrapZernioProfileQueue: bootstrap provisions the per-tenant profile at
// signup, teardown removes it at deletion.
//
// Deferring the actual Zernio calls into a retryable job is what keeps the
// delete request fast and resilient: Zernio being unreachable delays cleanup,
// it never fails the delete.
const TeardownZernioProfileQueue = "teardown_zernio_profile"

// TeardownZernioProfileTask carries the tenant whose profile to delete.
type TeardownZernioProfileTask struct {
	TenantID string `json:"tenant_id"`
}

// Kind implements JobArgs.
func (TeardownZernioProfileTask) Kind() string { return TeardownZernioProfileQueue }
