-- submit_times gains a tenant_id column so the persistent-fallback path in
-- GetSubmitTime can enforce the same tenant gate as the in-memory cache.
-- Drop-and-recreate instead of ALTER: rows carry a 1h TTL, so anything
-- present at migration time was about to expire anyway, and a NOT NULL
-- column cannot be backfilled meaningfully for rows whose tenant is unknown.
DROP TABLE IF EXISTS submit_times;

CREATE TABLE submit_times (
    tx_id       TEXT    NOT NULL PRIMARY KEY,
    tenant_id   TEXT    NOT NULL,
    submit_time INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_submit_times_ttl ON submit_times (submit_time);
