-- tx_id recorded the caller's transaction; nothing reads it, and an
-- extension commits in a transaction of its own.
ALTER TABLE model_schema_extensions DROP COLUMN tx_id;
