-- Reverses 000014. FAILED tasks are removed first: the version-13 shape has no
-- status, so a kept FAILED task would be due again and could repeat work that
-- must not be repeated. The primary key goes back to id alone; it fails, and
-- the down migration with it, if two tenants hold a task of the same id.
DELETE FROM scheduled_tasks WHERE status = 'FAILED';
DROP TABLE IF EXISTS scheduler_owners;
DROP TABLE IF EXISTS scheduled_task_marks;
DROP INDEX IF EXISTS scheduled_tasks_model_idx;
DROP INDEX IF EXISTS scheduled_tasks_query_idx;
DROP INDEX IF EXISTS scheduled_tasks_one_running_per_entity_uq;
DROP INDEX IF EXISTS scheduled_tasks_running_owner_idx;
DROP INDEX IF EXISTS scheduled_tasks_waiting_due_idx;
ALTER TABLE scheduled_tasks
    DROP CONSTRAINT scheduled_tasks_pkey,
    ADD CONSTRAINT scheduled_tasks_pkey PRIMARY KEY (id),
    DROP CONSTRAINT scheduled_tasks_last_error_len_chk,
    DROP CONSTRAINT scheduled_tasks_failed_chk,
    DROP CONSTRAINT scheduled_tasks_claim_chk,
    DROP CONSTRAINT scheduled_tasks_status_chk,
    DROP COLUMN claim_owner,
    DROP COLUMN claim_token,
    DROP COLUMN partial_commit,
    DROP COLUMN failed_time,
    DROP COLUMN failure_reason,
    DROP COLUMN last_error,
    DROP COLUMN last_attempt_time,
    DROP COLUMN lost_owners,
    DROP COLUMN attempts,
    DROP COLUMN next_attempt_time,
    DROP COLUMN status,
    DROP COLUMN arm_token,
    ADD COLUMN redispatch_after BIGINT,
    ADD COLUMN attempt_count INT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS scheduled_tasks_due_idx ON scheduled_tasks (scheduled_time);
