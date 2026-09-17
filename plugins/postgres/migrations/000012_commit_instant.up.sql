-- Temporal values move out of the JSONB document and into columns, which the
-- original storage design already names as the source of truth. The commit
-- phase then stamps narrow columns instead of rewriting whole documents.
--
-- Each new date column below gets DEFAULT CURRENT_TIMESTAMP. That default is
-- only a provisional value for the moment between a row's INSERT and its
-- transaction's commit — the later commit-stamping task overwrites it with
-- the real commit instant before any reader can observe it. Do not read the
-- default as the intended semantic: without it, Save's existing INSERT
-- statements (which do not yet mention these columns) would violate NOT NULL
-- on the very next write after this migration.
ALTER TABLE entity_versions ADD COLUMN IF NOT EXISTS transaction_id TEXT NOT NULL DEFAULT '';
ALTER TABLE entity_versions ADD COLUMN IF NOT EXISTS creation_date TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS creation_date TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS last_modified TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- Backfill from the documents before writes stop populating them. Existing
-- rows predate the column and carry their real creation/modification instant
-- only in the JSONB document; the ADD COLUMN default above only covers rows
-- inserted after this migration runs.
UPDATE entity_versions
   SET transaction_id = COALESCE(doc->'_meta'->>'transaction_id', ''),
       creation_date  = NULLIF(doc->'_meta'->>'creation_date', '')::timestamptz
 WHERE doc->'_meta'->>'creation_date' IS NOT NULL;

UPDATE entities
   SET creation_date = NULLIF(doc->'_meta'->>'creation_date', '')::timestamptz,
       last_modified = NULLIF(doc->'_meta'->>'last_modified_date', '')::timestamptz
 WHERE doc->'_meta'->>'creation_date' IS NOT NULL;

-- The commit phase finds a transaction's own rows by this column, and
-- GetVersionByTransaction moves onto it from its unindexed JSON probe.
CREATE INDEX IF NOT EXISTS idx_ev_transaction
    ON entity_versions (tenant_id, transaction_id, entity_id, version);

-- Every entity_versions row must have an entities row. The point-in-time read
-- enumerates entities from `entities` and probes each one's revision, so a
-- version row without its entity row is invisible to every point-in-time read
-- — silently, with no error. Nothing enforced this before: it held only
-- because the write paths happened to insert the entities row first. A test
-- fixture in this repository already violated it and the violation was
-- undetectable until the read changed shape.
--
-- ON DELETE CASCADE rather than RESTRICT: nothing deletes entities rows today
-- (Delete and DeleteAll set deleted = true), so the clause is unreachable in
-- current code. Should a retention or erasure feature ever remove an entity
-- row, taking its history with it is the honest outcome — the alternative is
-- history that no point-in-time read can reach.
ALTER TABLE entity_versions
    ADD CONSTRAINT entity_versions_entity_fk
    FOREIGN KEY (tenant_id, entity_id) REFERENCES entities (tenant_id, entity_id)
    ON DELETE CASCADE;

-- Durable submit times: an in-process map answers only on the node that
-- committed, and only until a restart.
CREATE TABLE IF NOT EXISTS submit_times (
    tenant_id   TEXT        NOT NULL,
    tx_id       TEXT        NOT NULL,
    submit_time TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (tenant_id, tx_id)
);

CREATE INDEX IF NOT EXISTS idx_submit_times_pruning ON submit_times (submit_time);

ALTER TABLE submit_times ENABLE ROW LEVEL SECURITY;
CREATE POLICY submit_times_tenant_isolation ON submit_times
    USING (tenant_id = current_setting('app.current_tenant', true));
