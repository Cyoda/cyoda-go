-- Revert to the tenant-less shape from 000001. Rows are discarded, matching
-- the up direction's rationale (1h TTL — nothing durable lives here).
DROP TABLE IF EXISTS submit_times;

CREATE TABLE submit_times (
    tx_id       TEXT    NOT NULL PRIMARY KEY,
    submit_time INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX idx_submit_times_ttl ON submit_times (submit_time);
