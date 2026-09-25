-- One owner per scheduled run. A task row carries its life (arm_token), its
-- status and its run bookkeeping. Unsafe marks and owner liveness are tables
-- of their own, so a mark written before an unsafe dispatch survives a
-- process restart and the next claim sees it.
--
-- The table is rebuilt: the primary key becomes (tenant_id, id), and
-- redispatch_after and attempt_count go. A pending task is kept as a new
-- life: WAITING, due at its scheduled time, with a fresh random arm token.
--
-- Explicit BEGIN/COMMIT: this file rebuilds scheduled_tasks (create, copy,
-- drop, rename) and adds two more tables in several statements; run as one
-- transaction so a failure partway never leaves the rebuild half done. No
-- PRAGMA or VACUUM appears here, so a transaction is not refused (see
-- migrate.go's NoTxWrap doc, which is about migrate's own per-file wrapper,
-- not about SQLite rejecting BEGIN/COMMIT around this file's statements).
BEGIN;

CREATE TABLE scheduled_tasks_v9 (
    id                TEXT    NOT NULL,
    tenant_id         TEXT    NOT NULL,
    type              TEXT    NOT NULL,
    scheduled_time    INTEGER NOT NULL,
    timeout_ms        INTEGER,
    entity_id         TEXT    NOT NULL,
    model_name        TEXT    NOT NULL,
    model_version     INTEGER NOT NULL,
    transition        TEXT    NOT NULL,
    source_state      TEXT    NOT NULL,
    armed_at          INTEGER NOT NULL,
    armed_by_id       TEXT    NOT NULL DEFAULT '',
    armed_by_kind     TEXT    NOT NULL DEFAULT '',
    arm_token         TEXT    NOT NULL,
    status            TEXT    NOT NULL CHECK (status IN ('WAITING', 'RUNNING', 'FAILED')),
    next_attempt_time INTEGER NOT NULL,
    attempts          INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lost_owners       INTEGER NOT NULL DEFAULT 0 CHECK (lost_owners >= 0),
    last_attempt_time INTEGER,
    last_error        TEXT    NOT NULL DEFAULT '' CHECK (length(CAST(last_error AS BLOB)) <= 1024),
    failure_reason    TEXT    NOT NULL DEFAULT '' CHECK (failure_reason IN (
                          '', 'UNSAFE_WORK_NOT_COMPLETED', 'OWNER_LOST_REPEATEDLY',
                          'EXPIRED_AFTER_FAILED_ATTEMPTS', 'RUN_PANICKED',
                          'STOPPED_AFTER_PARTIAL_COMMIT')),
    failed_time       INTEGER,
    partial_commit    INTEGER NOT NULL DEFAULT 0 CHECK (partial_commit IN (0, 1)),
    claim_token       TEXT,
    claim_owner       TEXT,
    CHECK ((status = 'RUNNING') = (claim_token IS NOT NULL AND claim_owner IS NOT NULL)),
    CHECK ((status = 'FAILED') = (failure_reason <> '')),
    PRIMARY KEY (tenant_id, id)
) STRICT;

INSERT INTO scheduled_tasks_v9
    (id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
     model_version, transition, source_state, armed_at, armed_by_id,
     armed_by_kind, arm_token, status, next_attempt_time)
SELECT id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
       model_version, transition, source_state, armed_at, armed_by_id,
       armed_by_kind,
       lower(hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
             substr(hex(randomblob(2)), 2) || '-' ||
             substr('89ab', 1 + (abs(random()) % 4), 1) ||
             substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6))),
       'WAITING', scheduled_time
FROM scheduled_tasks;

DROP TABLE scheduled_tasks;
ALTER TABLE scheduled_tasks_v9 RENAME TO scheduled_tasks;

-- ClaimDue: due WAITING rows, and RUNNING rows by owner.
CREATE INDEX idx_scheduled_tasks_waiting ON scheduled_tasks (next_attempt_time) WHERE status = 'WAITING';
CREATE INDEX idx_scheduled_tasks_owner ON scheduled_tasks (claim_owner) WHERE status = 'RUNNING';
-- At most one RUNNING task per entity.
CREATE UNIQUE INDEX idx_scheduled_tasks_running_entity ON scheduled_tasks (tenant_id, entity_id) WHERE status = 'RUNNING';
-- Query pages in (scheduled_time, id) order per tenant.
CREATE INDEX idx_scheduled_tasks_query ON scheduled_tasks (tenant_id, scheduled_time, id);
-- DeleteForModel; ReconcileForEntity and DeleteForEntities.
CREATE INDEX idx_scheduled_tasks_model ON scheduled_tasks (tenant_id, model_name, model_version);
CREATE INDEX idx_scheduled_tasks_entity ON scheduled_tasks (tenant_id, entity_id);

-- One mark per life at most, naming the claim that wrote it. A mark outlives
-- the life it belongs to until SweepMarks removes it, so it has no foreign key.
CREATE TABLE scheduled_task_marks (
    tenant_id   TEXT NOT NULL,
    task_id     TEXT NOT NULL,
    arm_token   TEXT NOT NULL,
    claim_token TEXT NOT NULL,
    PRIMARY KEY (tenant_id, task_id, arm_token)
) STRICT, WITHOUT ROWID;

-- Owner liveness on the store clock, in microseconds.
CREATE TABLE scheduler_owners (
    owner        TEXT    NOT NULL PRIMARY KEY,
    heartbeat_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
COMMIT;
