DROP TABLE IF EXISTS submit_times;
ALTER TABLE entity_versions DROP CONSTRAINT IF EXISTS entity_versions_entity_fk;
DROP INDEX IF EXISTS idx_ev_transaction;
ALTER TABLE entities DROP COLUMN IF EXISTS last_modified;
ALTER TABLE entities DROP COLUMN IF EXISTS creation_date;
ALTER TABLE entity_versions DROP COLUMN IF EXISTS creation_date;
ALTER TABLE entity_versions DROP COLUMN IF EXISTS transaction_id;
