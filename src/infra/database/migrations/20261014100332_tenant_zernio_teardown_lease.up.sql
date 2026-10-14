-- While river-service tears down a deleted workspace's Zernio profile, this
-- lease holds off a restore: SetStatus refuses to reactivate the tenant until
-- the lease ends. Each teardown step renews it, so a stalled teardown frees the
-- tenant within one lease.
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS zernio_teardown_lease_until timestamptz;
