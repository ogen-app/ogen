-- Intentionally a no-op. This migration only repairs a half-applied
-- 20260903000001; the published_url column's lifecycle is owned by that
-- migration's down. Dropping the column here would destroy data on a rollback
-- of this fixup alone, so we leave it in place.
SELECT 1;
