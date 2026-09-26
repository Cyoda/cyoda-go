-- Cyoda-Go PostgreSQL Storage Backend — Initial Schema
-- See: docs/plans/2026-03-27-postgresql-storage-design.md
--
-- This file is the consolidated, net-result schema for a fresh install.
-- Pre-release greenfield: there are no deployments to carry forward, so the
-- original 000001..000005 migration chain has been collapsed into this one
-- file. Application-level WHERE tenant_id = $1 is the primary isolation
-- mechanism; RLS is enabled but NOT forced (activates automatically when the
-- application connects as a non-owner role or sets app.current_tenant).

-- entities: current state, one row per entity
CREATE TABLE IF NOT EXISTS entities (
    tenant_id       TEXT        NOT NULL,
    entity_id       TEXT        NOT NULL,
    model_name      TEXT        NOT NULL,
    model_version   TEXT        NOT NULL,
    version         BIGINT      NOT NULL,
    deleted         BOOLEAN     NOT NULL DEFAULT false,
    doc             JSONB       NOT NULL,
    PRIMARY KEY (tenant_id, entity_id)
);

CREATE INDEX IF NOT EXISTS idx_entities_model
    ON entities (tenant_id, model_name, model_version)
    WHERE NOT deleted;

-- entity_versions: append-only bi-temporal history
CREATE TABLE IF NOT EXISTS entity_versions (
    tenant_id        TEXT        NOT NULL,
    entity_id        TEXT        NOT NULL,
    model_name       TEXT        NOT NULL,
    model_version    TEXT        NOT NULL,
    version          BIGINT      NOT NULL,
    valid_time       TIMESTAMPTZ NOT NULL,
    transaction_time TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    wall_clock_time  TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    doc              JSONB       NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, version)
);

CREATE INDEX IF NOT EXISTS idx_ev_bitemporal
    ON entity_versions (tenant_id, entity_id, valid_time DESC, transaction_time DESC);

CREATE INDEX IF NOT EXISTS idx_ev_model
    ON entity_versions (tenant_id, model_name, model_version);

-- sm_audit_events: StateMachine audit trail
CREATE TABLE IF NOT EXISTS sm_audit_events (
    tenant_id       TEXT        NOT NULL,
    entity_id       TEXT        NOT NULL,
    event_id        TEXT        NOT NULL,
    transaction_id  TEXT,
    timestamp       TIMESTAMPTZ NOT NULL,
    doc             JSONB       NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, event_id)
);

CREATE INDEX IF NOT EXISTS idx_sm_events_tx
    ON sm_audit_events (tenant_id, entity_id, transaction_id);

-- models: model descriptors
CREATE TABLE IF NOT EXISTS models (
    tenant_id       TEXT        NOT NULL,
    model_name      TEXT        NOT NULL,
    model_version   TEXT        NOT NULL,
    doc             JSONB       NOT NULL,
    PRIMARY KEY (tenant_id, model_name, model_version)
);

-- kv_store: key-value store (workflows, configs)
CREATE TABLE IF NOT EXISTS kv_store (
    tenant_id       TEXT        NOT NULL,
    namespace       TEXT        NOT NULL,
    key             TEXT        NOT NULL,
    value           BYTEA       NOT NULL,
    PRIMARY KEY (tenant_id, namespace, key)
);

-- messages: edge messages with binary payload
CREATE TABLE IF NOT EXISTS messages (
    tenant_id       TEXT        NOT NULL,
    message_id      TEXT        NOT NULL,
    header          JSONB       NOT NULL,
    metadata        JSONB       NOT NULL,
    payload         BYTEA       NOT NULL,
    PRIMARY KEY (tenant_id, message_id)
);

-- search_jobs: async search job metadata
-- PK is composite (tenant_id, id) so the same job ID may be reused across
-- tenants (the conformance harness uses fixed IDs like "j1" in fresh tenants).
-- condition is nullable: CreateJob writes NULL for an empty spi.SearchJob.Condition.
-- point_in_time is NOT NULL since 000015; it was declared nullable here on the
-- mistaken note that it was optional on spi.SearchJob, which it never was.
CREATE TABLE IF NOT EXISTS search_jobs (
    id            TEXT        NOT NULL,
    tenant_id     TEXT        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'RUNNING',
    model_name    TEXT        NOT NULL,
    model_ver     TEXT        NOT NULL,
    condition     JSONB,
    point_in_time TIMESTAMPTZ,
    search_opts   JSONB,
    result_count  INTEGER              DEFAULT 0,
    error         TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ,
    calc_ms       BIGINT               DEFAULT 0,
    PRIMARY KEY (tenant_id, id)
);

CREATE INDEX IF NOT EXISTS idx_search_jobs_tenant ON search_jobs(tenant_id);
CREATE INDEX IF NOT EXISTS idx_search_jobs_created ON search_jobs(created_at);

-- search_job_results: entity IDs matched by a search job
-- FK references search_jobs on the composite (id, tenant_id) PK.
CREATE TABLE IF NOT EXISTS search_job_results (
    job_id    TEXT    NOT NULL,
    tenant_id TEXT    NOT NULL,
    seq       INTEGER NOT NULL,
    entity_id TEXT    NOT NULL,
    PRIMARY KEY (job_id, seq),
    CONSTRAINT search_job_results_job_id_tenant_fkey
        FOREIGN KEY (job_id, tenant_id) REFERENCES search_jobs(id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_search_job_results_tenant ON search_job_results(tenant_id, job_id);

-- Row-Level Security (defense-in-depth).
-- RLS is enabled but NOT forced — the table owner bypasses policies. Enforcement
-- activates when (a) the application connects as a non-owner role, or (b) the
-- transaction sets app.current_tenant via SET LOCAL. Application-level
-- WHERE tenant_id = $1 is the primary isolation mechanism.
ALTER TABLE entities ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_entities ON entities
    USING (tenant_id = current_setting('app.current_tenant', true));

ALTER TABLE entity_versions ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_entity_versions ON entity_versions
    USING (tenant_id = current_setting('app.current_tenant', true));

ALTER TABLE sm_audit_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_sm_audit ON sm_audit_events
    USING (tenant_id = current_setting('app.current_tenant', true));

ALTER TABLE models ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_models ON models
    USING (tenant_id = current_setting('app.current_tenant', true));

ALTER TABLE kv_store ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_kv ON kv_store
    USING (tenant_id = current_setting('app.current_tenant', true));

ALTER TABLE messages ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_messages ON messages
    USING (tenant_id = current_setting('app.current_tenant', true));

ALTER TABLE search_jobs ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_search_jobs ON search_jobs
    USING (tenant_id = current_setting('app.current_tenant', true));

ALTER TABLE search_job_results ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_search_job_results ON search_job_results
    USING (tenant_id = current_setting('app.current_tenant', true));

-- --------------------------------------------------------------------
-- Model schema extensions — append-only log of additive schema deltas.
-- See docs/superpowers/specs/2026-04-20-model-schema-extensions-design.md §4.4.
--
-- Each row is either a single delta or a periodic savepoint (kind =
-- 'savepoint') that the plugin emits every 64 deltas. Get folds the
-- log from the most recent savepoint forward.
-- --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS model_schema_extensions (
    tenant_id     TEXT     NOT NULL,
    model_name    TEXT     NOT NULL,
    model_version TEXT     NOT NULL,
    seq           BIGSERIAL,
    kind          TEXT     NOT NULL CHECK (kind IN ('delta', 'savepoint')),
    payload       JSONB    NOT NULL,
    tx_id         TEXT     NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (tenant_id, model_name, model_version, seq)
);

CREATE INDEX IF NOT EXISTS model_schema_extensions_lookup
    ON model_schema_extensions (tenant_id, model_name, model_version, seq DESC);

ALTER TABLE model_schema_extensions ENABLE ROW LEVEL SECURITY;

CREATE POLICY model_schema_extensions_tenant_isolation
    ON model_schema_extensions
    USING (tenant_id = current_setting('app.current_tenant', true));
