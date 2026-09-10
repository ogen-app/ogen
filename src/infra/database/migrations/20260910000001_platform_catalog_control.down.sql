-- CON-292 rollback: drop the operator-catalog config and columns.

DROP TABLE IF EXISTS platform_global_limits;

DROP INDEX IF EXISTS ux_platforms_zernio_id;

ALTER TABLE platforms
    DROP COLUMN IF EXISTS sort_order,
    DROP COLUMN IF EXISTS supported_post_types,
    DROP COLUMN IF EXISTS connect_supported,
    DROP COLUMN IF EXISTS enabled,
    DROP COLUMN IF EXISTS zernio_id;
