-- scheduled_tasks: durable "fire X at scheduled_time" records (scheduled
-- transitions today; the type column keeps the shape generic for future
-- ScheduledTask variants). See docs/superpowers/specs/2026-07-16-scheduled-transition-runtime-design.md.
CREATE TABLE scheduled_tasks (
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
    PRIMARY KEY (id)
) STRICT;

-- ScanDue is cross-tenant and orders by scheduled_time.
CREATE INDEX idx_scheduled_tasks_due
    ON scheduled_tasks (scheduled_time);

-- ReconcileForEntity looks up an entity's other-state tasks per tenant.
CREATE INDEX idx_scheduled_tasks_entity
    ON scheduled_tasks (tenant_id, entity_id);
