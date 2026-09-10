-- CON-292 rollback: drop the operator-catalog config, seeded rows, and columns.

-- Remove the platforms seeded by this migration (§19.2).
DELETE FROM platforms WHERE id IN ('Tk7nQ2xLpR9a', 'Pn4vK8mWz1Bc', 'Rd5hJ3yTq6Ne');

DROP TABLE IF EXISTS platform_global_limits;

DROP INDEX IF EXISTS ux_platforms_zernio_id;

ALTER TABLE platforms
    DROP COLUMN IF EXISTS sort_order,
    DROP COLUMN IF EXISTS supported_post_types,
    DROP COLUMN IF EXISTS connect_supported,
    DROP COLUMN IF EXISTS enabled,
    DROP COLUMN IF EXISTS zernio_id;
