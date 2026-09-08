-- CON-165 fixup: repair environments where posts.published_url is missing.
--
-- The original 20260903000001 migration bundled `ALTER TABLE posts ADD COLUMN
-- published_url` with a pg_input_is_valid-guarded backfill in a single file.
-- bun sends a marker-less .up.sql as ONE pgx Exec, which the simple query
-- protocol runs as one implicit transaction — so when the backfill errored
-- (pg_input_is_valid is PG16+; and the CTE's published_results::jsonb cast can
-- still throw on a malformed row before the WHERE guard is applied) the
-- ADD COLUMN rolled back with it. bun's old default recorded 20260903000001 as
-- applied *before* running it, so the column stayed missing while the row was
-- marked done — every full `posts` SELECT (notably the reconcile sweep) then
-- failed with `column po.published_url does not exist`.
--
-- This migration re-adds the column idempotently and runs the backfill in a way
-- that can never abort. It is a no-op where the column already exists
-- (fresh/dev/PG16+ installs).
ALTER TABLE posts
    ADD COLUMN IF NOT EXISTS published_url TEXT;

--bun:split

-- Best-effort backfill of the auto-publish tail from published_results (element
-- 0's platformPostUrl — a post maps to a single platform). Done row-by-row so a
-- single malformed historical published_results value can't abort the backfill
-- for every other row: each row's ::jsonb cast runs in its own subtransaction
-- (the inner BEGIN/EXCEPTION), so a bad row is skipped, not fatal. This needs no
-- PG16-only pg_input_is_valid and cannot roll the migration back — the column
-- existing is what matters, and any un-backfilled row self-heals on its next
-- verify/refresh. The candidate set is only the auto-publish tail (non-empty,
-- array-shaped published_results with a still-NULL published_url), so the
-- per-row overhead is bounded and one-time.
DO $$
DECLARE
    r   RECORD;
    url TEXT;
BEGIN
    FOR r IN
        SELECT id, published_results
        FROM posts
        WHERE published_url IS NULL
          AND published_results <> ''
          AND left(btrim(published_results), 1) = '['
    LOOP
        BEGIN
            IF jsonb_typeof(r.published_results::jsonb) = 'array' THEN
                url := NULLIF(r.published_results::jsonb -> 0 ->> 'platformPostUrl', '');
                IF url IS NOT NULL THEN
                    -- Re-check published_url IS NULL: a concurrently-serving
                    -- instance (rolling deploy) may have set it since the cursor
                    -- snapshot — don't clobber a live value.
                    UPDATE posts SET published_url = url
                    WHERE id = r.id AND published_url IS NULL;
                END IF;
            END IF;
        EXCEPTION WHEN others THEN
            -- malformed row: skip it (self-heals on next verify/refresh).
            NULL;
        END;
    END LOOP;
END $$;
