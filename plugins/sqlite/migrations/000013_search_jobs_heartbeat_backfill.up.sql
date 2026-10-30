-- A search job written before CreateJob stamped heartbeat_time has none.
-- Stale reclaim measures from heartbeat_time alone, so such a RUNNING job
-- could never be reclaimed, and GetJob returned a nil heartbeat. Give it its
-- creation time, the baseline it was measured from until now (the same
-- backfill as the postgres plugin).
UPDATE search_jobs SET heartbeat_time = create_time WHERE heartbeat_time IS NULL;
