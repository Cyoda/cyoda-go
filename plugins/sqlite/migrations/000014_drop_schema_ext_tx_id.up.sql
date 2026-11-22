-- tx_id was a diagnostic stamp of the ambient transaction; nothing reads it
-- and an extension is not part of the caller's transaction.
ALTER TABLE model_schema_extensions DROP COLUMN tx_id;
