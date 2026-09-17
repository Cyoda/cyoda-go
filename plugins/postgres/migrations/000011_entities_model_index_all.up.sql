-- A point-in-time read enumerates entities from `entities` and probes each
-- one's revision at the instant. It must see entities deleted SINCE that
-- instant, whose current row carries deleted = true, so the model index can
-- no longer be partial.
--
-- Replacing the partial index rather than adding a second one: two
-- near-identical indexes on the hot write table cost every insert twice for
-- no gain. Current-state reads keep their `AND NOT deleted` predicate in the
-- query and simply filter after the index lookup.
--
-- Plain CREATE INDEX, not CONCURRENTLY: see the grandfathered entry in
-- migration_index_guard_test.go — CONCURRENTLY deterministically deadlocks
-- this project's concurrent multi-node boot.
DROP INDEX IF EXISTS idx_entities_model_entity_id;

CREATE INDEX IF NOT EXISTS idx_entities_model_entity_id
    ON entities (tenant_id, model_name, model_version, entity_id COLLATE "C");
