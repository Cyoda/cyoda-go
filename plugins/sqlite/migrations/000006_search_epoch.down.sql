DROP INDEX idx_entities_model_id;
ALTER TABLE search_jobs DROP COLUMN epoch;
ALTER TABLE search_jobs DROP COLUMN heartbeat_time;
