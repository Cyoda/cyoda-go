-- One owner per scheduled run.
--
-- scheduled_tasks now carries each task's life (arm_token, drawn on every
-- arm), its status (WAITING, RUNNING, FAILED), the claim that owns a RUNNING
-- run (claim_token, claim_owner), and what the scheduler records about its
-- attempts (next_attempt_time, attempts, lost_owners, last_attempt_time,
-- last_error, failure_reason, failed_time, partial_commit). redispatch_after
-- and attempt_count are dropped. A task is keyed by (tenant_id, id), as on
-- the other backends: the primary key was id alone.
--
-- Existing rows become WAITING lives, due at their scheduled time, each with
-- its own arm token: gen_random_uuid() is volatile, so ADD COLUMN rewrites the
-- table and evaluates it per row.
--
-- scheduled_task_marks holds one row per life (tenant, task id, arm token)
-- whose owner is about to dispatch a processor that is not safe to repeat. It
-- is written outside the entity transaction, so it survives that
-- transaction's rollback. scheduler_owners holds one liveness row per pnode
-- incarnation, stamped by the database clock.
--
-- Tenant isolation. None of the three tables is under row-level security.
-- ClaimDue, GiveBackIdle, the owner methods and the sweeps are cross-tenant
-- and run with no tenant set; no API reaches them. Every tenant-facing
-- statement filters on tenant_id. This corrects the note in 000004, which said
-- every write carried a tenant predicate: its Upsert and Delete matched on id
-- alone. Applied migrations are not edited, so the correction is made here.
--
-- Lock profile. ALTER TABLE takes ACCESS EXCLUSIVE on scheduled_tasks, and the
-- whole file runs as one implicit transaction, so readers and writers of
-- scheduled_tasks wait until the file commits: across the rewrite, the
-- backfill, the primary-key rebuild and the five index builds. No other table
-- is locked. CONCURRENTLY
-- cannot run here (many statements), and a separate file would not help:
-- golang-migrate's advisory lock spans the whole Up() run (see 000013).
ALTER TABLE scheduled_tasks
    DROP COLUMN redispatch_after,
    DROP COLUMN attempt_count,
    ADD COLUMN arm_token         UUID    NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN status            TEXT    NOT NULL DEFAULT 'WAITING',
    ADD COLUMN next_attempt_time BIGINT,
    ADD COLUMN attempts          INT     NOT NULL DEFAULT 0,
    ADD COLUMN lost_owners       INT     NOT NULL DEFAULT 0,
    ADD COLUMN last_attempt_time BIGINT,
    ADD COLUMN last_error        TEXT    NOT NULL DEFAULT '',
    ADD COLUMN failure_reason    TEXT    NOT NULL DEFAULT '',
    ADD COLUMN failed_time       BIGINT,
    ADD COLUMN partial_commit    BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN claim_token       UUID,
    ADD COLUMN claim_owner       UUID;

UPDATE scheduled_tasks SET next_attempt_time = scheduled_time;

-- The store always writes arm_token and status itself; the defaults above
-- exist only to backfill.
ALTER TABLE scheduled_tasks
    ALTER COLUMN next_attempt_time SET NOT NULL,
    ALTER COLUMN arm_token DROP DEFAULT,
    ALTER COLUMN status DROP DEFAULT,
    DROP CONSTRAINT scheduled_tasks_pkey,
    ADD CONSTRAINT scheduled_tasks_pkey PRIMARY KEY (tenant_id, id),
    ADD CONSTRAINT scheduled_tasks_status_chk
        CHECK (status IN ('WAITING', 'RUNNING', 'FAILED')),
    ADD CONSTRAINT scheduled_tasks_claim_chk
        CHECK ((status = 'RUNNING') = (claim_token IS NOT NULL AND claim_owner IS NOT NULL)),
    ADD CONSTRAINT scheduled_tasks_failed_chk
        CHECK ((status = 'FAILED') = (failure_reason <> '')),
    -- A recorded error text is at most 1 024 bytes on every backend; the
    -- store refuses a longer one as a deterministic rejection (SQLSTATE 23514).
    ADD CONSTRAINT scheduled_tasks_last_error_len_chk
        CHECK (octet_length(last_error) <= 1024);

DROP INDEX IF EXISTS scheduled_tasks_due_idx;

-- ClaimDue: due WAITING tasks; RUNNING tasks by owner; at most one RUNNING
-- task per entity, against concurrent claimers too.
CREATE INDEX scheduled_tasks_waiting_due_idx
    ON scheduled_tasks (next_attempt_time) WHERE status = 'WAITING';
CREATE INDEX scheduled_tasks_running_owner_idx
    ON scheduled_tasks (claim_owner) WHERE status = 'RUNNING';
CREATE UNIQUE INDEX scheduled_tasks_one_running_per_entity_uq
    ON scheduled_tasks (tenant_id, entity_id) WHERE status = 'RUNNING';
-- Query pages in (scheduled_time, id) order; ids compare byte-wise.
CREATE INDEX scheduled_tasks_query_idx
    ON scheduled_tasks (tenant_id, scheduled_time, id COLLATE "C");
-- DeleteForModel. (tenant_id, entity_id) is scheduled_tasks_entity_idx (000004).
CREATE INDEX scheduled_tasks_model_idx
    ON scheduled_tasks (tenant_id, model_name, model_version);

CREATE TABLE scheduled_task_marks (
    tenant_id   TEXT NOT NULL,
    task_id     TEXT NOT NULL,
    arm_token   UUID NOT NULL,
    claim_token UUID NOT NULL,
    PRIMARY KEY (tenant_id, task_id, arm_token)
);

CREATE TABLE scheduler_owners (
    owner        UUID        PRIMARY KEY,
    heartbeat_at TIMESTAMPTZ NOT NULL
);
