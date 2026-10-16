-- Background jobs run in river-service, on its own schema. ogen's in-process
-- River and its tables go. A job still waiting here would be lost, so the
-- migration refuses to run until river_job holds no live job.
-- The lookup is dynamic SQL: a database that never had River has no
-- river_job for a static query to name.
DO $$
DECLARE
    live boolean;
BEGIN
    IF to_regclass('river_job') IS NOT NULL THEN
        EXECUTE $q$
            SELECT EXISTS (
                SELECT 1 FROM river_job
                WHERE state::text IN ('available', 'pending', 'retryable', 'running', 'scheduled'))
        $q$ INTO live;
        IF live THEN
            RAISE EXCEPTION 'river_job still holds live jobs; let them finish before dropping River';
        END IF;
    END IF;
END
$$;

DROP TABLE IF EXISTS river_client_queue;
DROP TABLE IF EXISTS river_client;
DROP TABLE IF EXISTS river_queue;
DROP TABLE IF EXISTS river_leader;
DROP TABLE IF EXISTS river_job;
DROP TABLE IF EXISTS river_migration;
DROP FUNCTION IF EXISTS river_job_notify() CASCADE;
DROP FUNCTION IF EXISTS river_job_state_in_bitmask CASCADE;
DROP TYPE IF EXISTS river_job_state CASCADE;
