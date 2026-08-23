-- Epoch fencing and liveness tracking for async search jobs. epoch starts
-- at 1 (CreateJob's persisted value) and is bumped by ClaimStale on each
-- successful reclaim; heartbeat_time is NULL until the owning executor's
-- first Heartbeat call, in which case staleness falls back to create_time.
ALTER TABLE search_jobs ADD COLUMN heartbeat_time INTEGER;
ALTER TABLE search_jobs ADD COLUMN epoch INTEGER NOT NULL DEFAULT 1;
