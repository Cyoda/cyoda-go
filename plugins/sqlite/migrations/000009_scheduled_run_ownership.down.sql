DROP TABLE IF EXISTS scheduler_owners;
DROP TABLE IF EXISTS scheduled_task_marks;

CREATE TABLE scheduled_tasks_v8 (
    id                TEXT    NOT NULL,
    tenant_id         TEXT    NOT NULL,
    type              TEXT    NOT NULL,
    scheduled_time    INTEGER NOT NULL,
    timeout_ms        INTEGER,
    redispatch_after  INTEGER,
    entity_id         TEXT    NOT NULL,
    model_name        TEXT    NOT NULL,
    model_version     INTEGER NOT NULL,
    transition        TEXT    NOT NULL,
    source_state      TEXT    NOT NULL,
    armed_at          INTEGER NOT NULL,
    attempt_count     INTEGER NOT NULL DEFAULT 0,
    armed_by_id       TEXT    NOT NULL DEFAULT '',
    armed_by_kind     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (id)
) STRICT;

INSERT INTO scheduled_tasks_v8
    (id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
     model_version, transition, source_state, armed_at, armed_by_id, armed_by_kind)
SELECT id, tenant_id, type, scheduled_time, timeout_ms, entity_id, model_name,
       model_version, transition, source_state, armed_at, armed_by_id, armed_by_kind
FROM scheduled_tasks;

DROP TABLE scheduled_tasks;
ALTER TABLE scheduled_tasks_v8 RENAME TO scheduled_tasks;

CREATE INDEX idx_scheduled_tasks_due ON scheduled_tasks (scheduled_time);
CREATE INDEX idx_scheduled_tasks_entity ON scheduled_tasks (tenant_id, entity_id);
