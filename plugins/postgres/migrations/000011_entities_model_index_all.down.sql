-- Mirrors up.sql's build-under-a-temporary-name-then-swap sequencing, for
-- the same reason: a naive DROP-then-CREATE would hold AccessExclusiveLock
-- (taken by the DROP, and never released until this file's implicit
-- transaction commits) across the whole index build that follows it,
-- blocking every reader as well as every writer against `entities` for the
-- build's duration. Building the partial index under a new name first keeps
-- that statement at ShareLock (blocks writers, not readers); the DROP and
-- RENAME that follow are fast, catalog-only AccessExclusiveLock operations,
-- so readers are blocked for a moment rather than for the build's length.
CREATE INDEX IF NOT EXISTS idx_entities_model_entity_id_rebuild
    ON entities (tenant_id, model_name, model_version, entity_id COLLATE "C")
    WHERE NOT deleted;

DROP INDEX IF EXISTS idx_entities_model_entity_id;

ALTER INDEX idx_entities_model_entity_id_rebuild RENAME TO idx_entities_model_entity_id;
