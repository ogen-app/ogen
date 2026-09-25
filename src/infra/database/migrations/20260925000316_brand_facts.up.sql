-- CON-316: Facts ledger — per-fact records + the guardrails stance.
--
-- Facts move out of brand_guardrails.facts (a jsonb string list) into a table of
-- their own: each fact gets a stable id, a subject, a kind, a source, three
-- calendar dates and an author, and is written one row at a time. The generator
-- (brandresolve) reads this table and drops expired rows. brand_guardrails.facts
-- is kept for now but is only a projection of this table (CON-316 FR6).
--
-- Both tables cascade from tenants, so tenant teardown needs no table list.

CREATE TABLE brand_facts (
    id              TEXT        PRIMARY KEY,
    tenant_id       TEXT        NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    statement       TEXT        NOT NULL,
    subject         TEXT        NOT NULL DEFAULT 'us'
                      CHECK (subject IN ('us', 'problem', 'opportunity')),
    kind            TEXT        NOT NULL DEFAULT 'documented'
                      CHECK (kind IN ('measured', 'documented', 'commitment', 'judgement')),
    source          TEXT        NOT NULL DEFAULT '',
    added_on        DATE        NULL,   -- NULL = "never recorded" (backfilled rows)
    checked_on      DATE        NULL,
    expires_on      DATE        NULL,   -- NULL = does not go off
    created_by      TEXT        NULL REFERENCES users (id) ON DELETE SET NULL,
    created_by_name TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The statement is a fact's identity within a workspace; the guardrails
-- compatibility path reconciles on it.
CREATE UNIQUE INDEX idx_brand_facts_tenant_statement ON brand_facts (tenant_id, statement);
CREATE INDEX idx_brand_facts_tenant_expires ON brand_facts (tenant_id, expires_on);

-- A row means the workspace has decided it needs no guardrails. Absent means
-- undecided. Any guardrails write deletes it.
CREATE TABLE brand_guardrails_stance (
    tenant_id       TEXT        PRIMARY KEY REFERENCES tenants (id) ON DELETE CASCADE,
    decided_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by      TEXT        NULL REFERENCES users (id) ON DELETE SET NULL,
    decided_by_name TEXT        NOT NULL DEFAULT ''
);

-- Backfill: one fact per trimmed, non-blank, distinct statement, with the
-- defaults the ui sidecar renders for a statement it has no metadata for
-- (subject us, kind documented, no source, no dates, no author). created_at is
-- spaced a microsecond apart in list order so the ledger's created_at ordering
-- reproduces the original list.
INSERT INTO brand_facts (id, tenant_id, statement, created_at, updated_at)
SELECT 'bf' || substr(md5(random()::text || clock_timestamp()::text || f.tenant_id || f.statement), 1, 14),
       f.tenant_id,
       f.statement,
       f.created_at + (f.ord * INTERVAL '1 microsecond'),
       f.updated_at
FROM (
    SELECT DISTINCT ON (g.tenant_id, btrim(s.value))
           g.tenant_id,
           btrim(s.value) AS statement,
           s.ord,
           g.created_at,
           g.updated_at
    FROM brand_guardrails g
    CROSS JOIN LATERAL jsonb_array_elements_text(g.facts) WITH ORDINALITY AS s(value, ord)
    WHERE btrim(s.value) <> ''
    ORDER BY g.tenant_id, btrim(s.value), s.ord
) f;
