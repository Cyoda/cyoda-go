-- Every search job has a point in time. spi.SearchJob.PointInTime is a plain
-- time.Time and CreateJob always writes it; the database now refuses a row
-- without one. SQLite cannot add NOT NULL to an existing column, so the table
-- is rebuilt (create, copy, drop, rename) with the one change; every other
-- column, default and constraint is as 000001, 000006 and 000007 left it.
-- search_jobs has no index or trigger of its own.
--
-- Explicit BEGIN/COMMIT: see 000009's comment on why a multi-statement
-- rebuild supplies its own transaction.
BEGIN;

CREATE TABLE search_jobs_v10 (
    tenant_id      TEXT    NOT NULL,
    job_id         TEXT    NOT NULL,
    status         TEXT    NOT NULL DEFAULT 'RUNNING',
    model_name     TEXT    NOT NULL,
    model_version  TEXT    NOT NULL,
    condition      BLOB,
    point_in_time  INTEGER NOT NULL,
    search_opts    BLOB,
    result_count   INTEGER NOT NULL DEFAULT 0,
    error          TEXT    NOT NULL DEFAULT '',
    create_time    INTEGER NOT NULL,
    finish_time    INTEGER,
    calc_time_ms   INTEGER NOT NULL DEFAULT 0,
    heartbeat_time INTEGER,
    epoch          INTEGER NOT NULL DEFAULT 1,
    released       INTEGER NOT NULL DEFAULT 0,
    stale_claims   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, job_id)
) STRICT, WITHOUT ROWID;

INSERT INTO search_jobs_v10
    (tenant_id, job_id, status, model_name, model_version, condition,
     point_in_time, search_opts, result_count, error, create_time, finish_time,
     calc_time_ms, heartbeat_time, epoch, released, stale_claims)
SELECT tenant_id, job_id, status, model_name, model_version, condition,
       point_in_time, search_opts, result_count, error, create_time, finish_time,
       calc_time_ms, heartbeat_time, epoch, released, stale_claims
FROM search_jobs;

DROP TABLE search_jobs;
ALTER TABLE search_jobs_v10 RENAME TO search_jobs;

COMMIT;
