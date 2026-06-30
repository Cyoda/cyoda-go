CREATE TABLE IF NOT EXISTS unique_claims (
    tenant_id TEXT NOT NULL, model_name TEXT NOT NULL, model_version TEXT NOT NULL,
    key_id TEXT NOT NULL, signature TEXT NOT NULL, entity_id TEXT NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, key_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS unique_claims_uq
    ON unique_claims (tenant_id, model_name, model_version, key_id, signature);
ALTER TABLE unique_claims ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_unique_claims ON unique_claims
    USING (tenant_id = current_setting('app.current_tenant', true));
