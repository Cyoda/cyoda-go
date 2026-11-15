-- model_lifecycle: one row per model whose generation every change of the
-- model's state or unique keys bumps (Lock when it changes the state, Unlock,
-- Save; Delete removes the row), in the transaction that writes the models
-- row, after it. A schema extension and a change-level change do not touch
-- it. An entity commit locks the rows of its models FOR SHARE, so a change of
-- a model's state or keys and a commit into that model are ordered. No foreign
-- key to models: the commit's check inserts a row for a model that may not
-- exist, to tell a missing model from one created after its snapshot.
CREATE TABLE IF NOT EXISTS model_lifecycle (
    tenant_id       TEXT    NOT NULL,
    model_name      TEXT    NOT NULL,
    model_version   TEXT    NOT NULL,
    generation      BIGINT  NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, model_name, model_version)
);

-- Same tenant policy as models: enabled, not forced.
ALTER TABLE model_lifecycle ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_model_lifecycle ON model_lifecycle
    USING (tenant_id = current_setting('app.current_tenant', true));

INSERT INTO model_lifecycle (tenant_id, model_name, model_version)
SELECT tenant_id, model_name, model_version FROM models
ON CONFLICT DO NOTHING;
