-- The campaign and post assistants are packs of the one assistant now, so
-- their conversations join its store, keyed by thread: each campaign's and
-- each post's history becomes that thread's, oldest first as before. Model
-- turns were stored as the JSON of their "complete" event, which the
-- assistant replays by its explanation and action, the fields both shapes
-- share. The old tables go.
--
-- A campaign turn's two messages were written in one transaction, so they
-- share created_at; the model's moves a microsecond later, as the assistant
-- writes its own, so it replays after the user's.
INSERT INTO assistant_messages (id, tenant_id, scope_kind, scope_id, role, content, created_at)
SELECT id, tenant_id, 'campaign', campaign_id, role, content,
       CASE WHEN role = 'model' THEN created_at + interval '1 microsecond' ELSE created_at END
FROM campaign_assistant_messages
ON CONFLICT (id) DO NOTHING;

INSERT INTO assistant_messages (id, tenant_id, scope_kind, scope_id, role, content, created_at)
SELECT id, tenant_id, 'post', post_id, role, content, created_at
FROM post_assistant_messages
ON CONFLICT (id) DO NOTHING;

DROP TABLE campaign_assistant_messages;
DROP TABLE post_assistant_messages;

-- The old table's rows went with their post; a thread now goes when the
-- post's delete removes it. The threads and offers of posts deleted before
-- this go now.
DELETE FROM assistant_messages m
WHERE m.scope_kind = 'post'
  AND NOT EXISTS (SELECT 1 FROM posts p WHERE p.id = m.scope_id);

DELETE FROM assistant_proposals ap
WHERE ap.scope_kind = 'post'
  AND NOT EXISTS (SELECT 1 FROM posts p WHERE p.id = ap.scope_id);

-- One model config: the post assistant's writer becomes the assistant's,
-- per scope, unless the assistant already has one there. The old flows'
-- rows go; their orchestrators fell to the assistant's own.
INSERT INTO flow_model_config (id, tier_id, flow_key, slot_key, model_id, updated_at, updated_by)
SELECT 'mc-' || md5(COALESCE(w.tier_id, '') || ':assistant:writer'), w.tier_id, 'assistant', 'writer', w.model_id, w.updated_at, w.updated_by
FROM flow_model_config w
WHERE w.flow_key = 'post_assistant' AND w.slot_key = 'writer'
  AND NOT EXISTS (
      SELECT 1 FROM flow_model_config a
      WHERE a.flow_key = 'assistant' AND a.slot_key = 'writer'
        AND COALESCE(a.tier_id, '') = COALESCE(w.tier_id, '')
  );

DELETE FROM flow_model_config WHERE flow_key IN ('post_assistant', 'campaign_assistant');
