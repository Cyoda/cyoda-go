-- Explicit BEGIN/COMMIT: see the up migration's comment.
BEGIN;

CREATE TABLE search_jobs_v9 (
    tenant_id      TEXT    NOT NULL,
    job_id         TEXT    NOT NULL,
    status         TEXT    NOT NULL DEFAULT 'RUNNING',
    model_name     TEXT    NOT NULL,
    model_version  TEXT    NOT NULL,
    condition      BLOB,
    point_in_time  INTEGER,
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

INSERT INTO search_jobs_v9
    (tenant_id, job_id, status, model_name, model_version, condition,
     point_in_time, search_opts, result_count, error, create_time, finish_time,
     calc_time_ms, heartbeat_time, epoch, released, stale_claims)
SELECT tenant_id, job_id, status, model_name, model_version, condition,
       point_in_time, search_opts, result_count, error, create_time, finish_time,
       calc_time_ms, heartbeat_time, epoch, released, stale_claims
FROM search_jobs;

DROP TABLE search_jobs;
ALTER TABLE search_jobs_v9 RENAME TO search_jobs;

COMMIT;
