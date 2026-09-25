-- Write the ledger's statements back into brand_guardrails.facts (in ledger
-- order) before dropping it. A workspace with facts but no guardrails row gets
-- one, so no statement is lost.
UPDATE brand_guardrails g
SET facts = COALESCE((
    SELECT jsonb_agg(f.statement ORDER BY f.created_at, f.id)
    FROM brand_facts f
    WHERE f.tenant_id = g.tenant_id
), '[]'::jsonb);

INSERT INTO brand_guardrails (id, tenant_id, facts)
SELECT 'bg' || substr(md5(random()::text || f.tenant_id), 1, 14),
       f.tenant_id,
       jsonb_agg(f.statement ORDER BY f.created_at, f.id)
FROM brand_facts f
WHERE NOT EXISTS (SELECT 1 FROM brand_guardrails g WHERE g.tenant_id = f.tenant_id)
GROUP BY f.tenant_id;

DROP TABLE IF EXISTS brand_guardrails_stance;
DROP TABLE IF EXISTS brand_facts;
