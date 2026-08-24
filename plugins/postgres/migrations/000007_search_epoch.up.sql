-- Async-search claim/heartbeat fencing (spi.AsyncSearchStore.Epoch/HeartbeatTime).
-- heartbeat_time is NULL until the first Heartbeat/ClaimStale stamps it; staleness
-- then falls back to created_at as the baseline (see search_store.go ClaimStale).
-- epoch starts at 1 (CreateJob's contract) and is bumped by each successful claim.
ALTER TABLE search_jobs ADD COLUMN heartbeat_time TIMESTAMPTZ;
ALTER TABLE search_jobs ADD COLUMN epoch BIGINT NOT NULL DEFAULT 1;
