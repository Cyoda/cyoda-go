-- Epoch fencing and liveness tracking for async search jobs. epoch starts
-- at 1 (CreateJob's persisted value) and is bumped by ClaimStale on each
-- successful reclaim; heartbeat_time is NULL until the owning executor's
-- first Heartbeat call, in which case staleness falls back to create_time.
ALTER TABLE search_jobs ADD COLUMN heartbeat_time INTEGER;
ALTER TABLE search_jobs ADD COLUMN epoch INTEGER NOT NULL DEFAULT 1;

-- GetPage's canonical entity-ID order over a model's current-state rows:
-- covers the tenant/model/model-version equality filter AND the trailing
-- entity_id ORDER BY in one index, so the planner can satisfy both the
-- WHERE and the ORDER BY without a separate sort step.
CREATE INDEX idx_entities_model_id ON entities (tenant_id, model_name, model_version, entity_id);
