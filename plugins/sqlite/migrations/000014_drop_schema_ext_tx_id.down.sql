-- Restores the column. SQLite's ADD COLUMN requires a constant default for a
-- NOT NULL column, so existing rows get ''.
ALTER TABLE model_schema_extensions ADD COLUMN tx_id TEXT NOT NULL DEFAULT '';
