-- scheduled_tasks stores durable "do something at ScheduledTime, with
-- TimeoutMs lateness tolerance" records for the scheduled-transition
-- runtime and future ScheduledTask variants (see spi.ScheduledTaskStore).
--
-- Deliberately NOT enrolled in row-level security, unlike every other
-- tenant table in this schema: ScanDue is a trusted, cross-tenant system
-- read driving the scheduler's due-task scan loop, invoked outside any
-- per-tenant request context (no app.current_tenant GUC set). Adding a
-- tenant_isolation policy here would silently zero out ScanDue's result
-- set once RLS enforcement is strengthened (FORCE + non-owner role) for
-- the rest of the schema. Upsert/Delete/ReconcileForEntity remain
-- tenant-safe via application-level tenant_id predicates, matching the
-- primary isolation mechanism already relied on for every table in this
-- schema (RLS here is not FORCE-enabled — see migration 000002's notes).
CREATE TABLE IF NOT EXISTS scheduled_tasks (
    id               TEXT   PRIMARY KEY,
    tenant_id        TEXT   NOT NULL,
    type             TEXT   NOT NULL,
    scheduled_time   BIGINT NOT NULL,
    timeout_ms       BIGINT,
    redispatch_after BIGINT,
    entity_id        TEXT   NOT NULL,
    model_name       TEXT   NOT NULL,
    model_version    INT    NOT NULL,
    transition       TEXT   NOT NULL,
    source_state     TEXT   NOT NULL,
    armed_at         BIGINT NOT NULL,
    attempt_count    INT    NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS scheduled_tasks_due_idx
    ON scheduled_tasks (scheduled_time);

CREATE INDEX IF NOT EXISTS scheduled_tasks_entity_idx
    ON scheduled_tasks (tenant_id, entity_id);
