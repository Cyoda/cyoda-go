-- Restores the column; existing rows get ''.
ALTER TABLE model_schema_extensions ADD COLUMN tx_id TEXT NOT NULL DEFAULT '';
