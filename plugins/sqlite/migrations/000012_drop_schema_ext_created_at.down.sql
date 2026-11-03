-- SQLite's ADD COLUMN refuses a non-constant default, so the original
-- strftime default is restored by rebuilding the table. Explicit
-- BEGIN/COMMIT: the migration runs with NoTxWrap.
BEGIN;

CREATE TABLE model_schema_extensions_v11 (
    tenant_id     TEXT     NOT NULL,
    model_name    TEXT     NOT NULL,
    model_version TEXT     NOT NULL,
    seq           INTEGER  NOT NULL,
    kind          TEXT     NOT NULL CHECK (kind IN ('delta', 'savepoint')),
    payload       BLOB     NOT NULL,
    tx_id         TEXT     NOT NULL,
    created_at    TEXT     NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (tenant_id, model_name, model_version, seq)
);

INSERT INTO model_schema_extensions_v11
    (tenant_id, model_name, model_version, seq, kind, payload, tx_id)
SELECT tenant_id, model_name, model_version, seq, kind, payload, tx_id
FROM model_schema_extensions;

DROP TABLE model_schema_extensions;
ALTER TABLE model_schema_extensions_v11 RENAME TO model_schema_extensions;

CREATE INDEX IF NOT EXISTS model_schema_extensions_lookup
    ON model_schema_extensions (tenant_id, model_name, model_version, seq DESC);

COMMIT;
