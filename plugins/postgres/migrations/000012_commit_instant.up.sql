-- Temporal values move out of the JSONB document and into columns, which the
-- original storage design already names as the source of truth. The commit
-- phase then stamps narrow columns instead of rewriting whole documents.
--
-- Each new date column below gets DEFAULT CURRENT_TIMESTAMP as a provisional
-- value. The later commit-stamping task overwrites it with the real commit
-- instant, but only for writes that reach TransactionManager.Commit — i.e.
-- writes made under an SPI transaction. A non-transactional Save/Delete/
-- CompareAndSave opens its own raw DB transaction with no SPI transaction and
-- no transaction ID, so it never reaches that stamping path: for that whole
-- class of writes this default is never overwritten and stays the
-- permanently recorded value — which is CURRENT_TIMESTAMP, i.e.
-- transaction-start time, the exact thing this change exists to eliminate.
-- Stamping the non-transactional path is owed by the commit-stamping task,
-- not solved here. This default solves exactly one narrower problem: without
-- it, Save's existing INSERT statements (which do not yet mention these
-- columns) would violate NOT NULL on the very next write after this
-- migration.
ALTER TABLE entity_versions ADD COLUMN IF NOT EXISTS transaction_id TEXT NOT NULL DEFAULT '';
ALTER TABLE entity_versions ADD COLUMN IF NOT EXISTS creation_date TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS creation_date TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;
ALTER TABLE entities ADD COLUMN IF NOT EXISTS last_modified TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP;

-- Backfill from the documents before writes stop populating them. Existing
-- rows predate the column and carry their real creation/modification instant
-- only in the JSONB document; the ADD COLUMN default above only covers rows
-- inserted after this migration runs (plus every pre-existing row, stamped
-- once at ALTER TABLE time with a single shared value, since
-- CURRENT_TIMESTAMP is non-volatile).
--
-- Each assignment falls back to the column's OWN current value via COALESCE
-- rather than assigning NULLIF's NULL straight into a NOT NULL column: a row
-- whose document carries one date but not the other, or an empty string for
-- one, must not abort the migration and leave schema_migrations dirty — it
-- must instead keep the ALTER TABLE default for the field it can't backfill.
-- Each column also gets its own WHERE guard keyed off its own document
-- field, rather than one column's presence gating another's assignment.
UPDATE entity_versions
   SET transaction_id = COALESCE(doc->'_meta'->>'transaction_id', transaction_id)
 WHERE doc->'_meta' ? 'transaction_id';

UPDATE entity_versions
   SET creation_date = COALESCE(NULLIF(doc->'_meta'->>'creation_date', '')::timestamptz, creation_date)
 WHERE doc->'_meta' ? 'creation_date';

UPDATE entities
   SET creation_date = COALESCE(NULLIF(doc->'_meta'->>'creation_date', '')::timestamptz, creation_date)
 WHERE doc->'_meta' ? 'creation_date';

UPDATE entities
   SET last_modified = COALESCE(NULLIF(doc->'_meta'->>'last_modified_date', '')::timestamptz, last_modified)
 WHERE doc->'_meta' ? 'last_modified_date';

-- The commit phase finds a transaction's own rows by this column, which is
-- what the index serves.
--
-- GetVersionByTransaction deliberately does NOT move onto it, and the
-- unindexed document probe it keeps is not an oversight: this column carries
-- only a transaction that actually committed the row, while a non-transactional
-- write stores the caller-supplied id in the document instead. The memory
-- backend indexes that document value verbatim (its recordTxIndex), so a
-- caller-supplied id IS findable there — moving this backend onto the column
-- would make it answer differently from the others for that class of write.
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
-- ON DELETE RESTRICT, not CASCADE: this constraint exists so a future
-- retention or erasure feature cannot silently take an entity's history with
-- it. CASCADE would guarantee exactly that outcome — one DELETE FROM
-- entities would silently remove the whole version chain with no chance to
-- object. RESTRICT instead refuses the deletion outright: any future
-- retention/erasure feature is forced to make an explicit decision about
-- entity_versions (archive it, delete it deliberately, or route around this
-- entity) rather than inheriting silent deletion as a side effect. It costs
-- nothing today: nothing deletes entities rows now (Delete and DeleteAll set
-- deleted = true), so the clause is unreachable in current code either way —
-- RESTRICT is simply the fail-closed choice between two currently-unreachable
-- options.
--
-- Lock profile, stated as it actually is: this migration BLOCKS every writer
-- to entities and entity_versions from the ADD CONSTRAINT below until the
-- whole file commits — across the VALIDATE CONSTRAINT scan of
-- entity_versions, the largest table in this schema, and the submit_times DDL
-- after it.
--
-- The NOT VALID / VALIDATE split does NOT avoid that here, and must not be
-- read as claiming it does. The split's usual non-blocking profile depends on
-- the two statements running in SEPARATE transactions, so that ADD CONSTRAINT
-- releases its SHARE ROW EXCLUSIVE before the historical scan begins and the
-- scan runs alone under SHARE UPDATE EXCLUSIVE. They do not run separately:
-- the driver sends this whole file through one Exec with MultiStatementEnabled
-- false, and PostgreSQL wraps a multi-statement simple query in an implicit
-- transaction (see migration_index_guard_test.go's clause (b), and 000011's
-- comment, which turns on the same property). Locks taken inside a transaction
-- are held to its end, so SHARE ROW EXCLUSIVE — which conflicts with the ROW
-- EXCLUSIVE every INSERT/UPDATE/DELETE takes — spans the rest of the file.
-- Net effect on writers: identical to a plain validating ADD CONSTRAINT.
-- Readers are unaffected either way (ACCESS SHARE does not conflict).
--
-- That is acceptable, and the non-blocking profile is deliberately not
-- pursued: there are no production instances to protect, and buying it would
-- mean moving VALIDATE CONSTRAINT into a migration file of its own, which
-- carries its own concurrent multi-node boot implications (the migrator holds
-- one session advisory lock across the entire Up() run, spanning every file in
-- the batch) for no benefit today. The split is kept because the end state is
-- identical and it costs nothing: NOT VALID records the constraint and starts
-- enforcing it on every new row immediately, leaving VALIDATE to account only
-- for history.
ALTER TABLE entity_versions
    ADD CONSTRAINT entity_versions_entity_fk
    FOREIGN KEY (tenant_id, entity_id) REFERENCES entities (tenant_id, entity_id)
    ON DELETE RESTRICT
    NOT VALID;

ALTER TABLE entity_versions VALIDATE CONSTRAINT entity_versions_entity_fk;

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
