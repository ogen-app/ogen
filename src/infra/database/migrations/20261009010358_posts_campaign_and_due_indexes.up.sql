-- Indexes for the posts lookups that only had idx_posts_tenant_id to use.
--
-- idx_posts_campaign serves every per-campaign read (campaign post lists,
-- phase counts, unschedule-by-campaign, the Figma campaign tree) and the
-- posts_touch_campaign trigger path. campaign_id leads because campaign ids
-- are globally unique, so the index also serves the tree's raw-SQL ranking
-- subquery, which has no tenant predicate. scheduled_at matches their order.
CREATE INDEX IF NOT EXISTS idx_posts_campaign ON posts (campaign_id, scheduled_at);

-- idx_posts_due serves the cross-tenant sweeps for stuck scheduled posts and
-- for manual-publish reminders, which filter one status and a scheduled_at
-- cutoff. Partial, so it holds only the few posts waiting on a schedule.
CREATE INDEX IF NOT EXISTS idx_posts_due ON posts (status, scheduled_at)
    WHERE status IN ('scheduled', 'scheduled_for_manual_publishing');
