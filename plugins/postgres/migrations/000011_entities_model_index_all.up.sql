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
-- Sequencing — build under a temporary name, then swap — is deliberate, not
-- decorative. The naive DROP-then-CREATE (drop the old index, then CREATE
-- INDEX the replacement in its place) was tried and rejected: DROP INDEX
-- takes AccessExclusiveLock on `entities`, and because the whole migration
-- file runs as one implicit transaction (see migration_index_guard_test.go's
-- clause (b) comment), that lock is held from the DROP all the way to COMMIT
-- — i.e. for the ENTIRE index build that follows it. AccessExclusiveLock
-- conflicts with AccessShareLock, so every plain SELECT against `entities`
-- blocks for as long as the build takes, cluster-wide — not just writers.
--
-- This sequencing instead runs the slow part (the index build) as a plain
-- CREATE INDEX against a NEW name while the old index is still live: that
-- statement takes only ShareLock, which conflicts with writers (INSERT/
-- UPDATE/DELETE take RowExclusiveLock) but not with a reader's
-- AccessShareLock. Only once the build has finished do the fast catalog-only
-- steps run — DROP the old index, RENAME the new one into its place — and
-- each of those takes AccessExclusiveLock only for the moment it takes to
-- update the catalog, not for a data scan or rewrite. Net effect: readers
-- are blocked for a moment, not for the build's duration; writers are
-- blocked for the build's duration either way (a plain, non-CONCURRENTLY
-- build cannot avoid that on a table it can't take CONCURRENTLY's own build
-- strategy against — see below for why CONCURRENTLY itself is not an option
-- here).
--
-- Not CONCURRENTLY: see the grandfathered entry in
-- migration_index_guard_test.go, which explains why the initial build below
-- still uses a plain CREATE INDEX rather than CONCURRENTLY.
CREATE INDEX IF NOT EXISTS idx_entities_model_entity_id_rebuild
    ON entities (tenant_id, model_name, model_version, entity_id COLLATE "C");

DROP INDEX IF EXISTS idx_entities_model_entity_id;

ALTER INDEX idx_entities_model_entity_id_rebuild RENAME TO idx_entities_model_entity_id;
