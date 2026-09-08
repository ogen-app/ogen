-- CON-165: persist the platform permalink as a first-class field on the post.
-- Until now the published URL survived only inside published_results JSON (the
-- auto-publish path) or in platform_analytics[].platform_post_url in the
-- isolated analytics DB (the verify-external path). Neither is a clean field the
-- front-end can read off a post it already has, forcing an N+1 of analytics
-- calls just to render a "View post" link (CON-149).
--
-- Nullable and backfill-tolerant: a row carries NULL until a publish/verify sets
-- it, or the user pastes one via PUT /api/posts/:id (the Zernio skip path). The
-- "verified" distinction is not a new column — publisher_post_id <> '' already
-- signals a publisher-confirmed post; a URL present with an empty
-- publisher_post_id is a user-supplied (unverified) link.
ALTER TABLE posts
    ADD COLUMN IF NOT EXISTS published_url TEXT;

--bun:split

-- Backfill the auto-publish tail from published_results — the first platform
-- outcome's platformPostUrl (posts map to a single platform, so element 0 is the
-- one). Done row-by-row in a PL/pgSQL loop so this stays portable to PostgreSQL
-- 15: the original guard used pg_input_is_valid, which is PG16+, so under a
-- mark-applied-on-success migrator a clean PG15 install would otherwise wedge on
-- a permanently-failing migration. Each row's ::jsonb cast runs in its own
-- subtransaction (the inner BEGIN/EXCEPTION), so a malformed historical
-- published_results value is skipped rather than aborting the migration. The
-- `published_url IS NULL` guard on the UPDATE keeps a concurrent writer (rolling
-- deploy) from being clobbered; the verify-external tail (URL only in the
-- separate analytics DB) self-heals on the next verify/refresh.
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
