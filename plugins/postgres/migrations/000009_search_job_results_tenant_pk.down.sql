-- Best-effort rollback. Narrowing the PK back to (job_id, seq) FAILS on any
-- database where two tenants share a job ID with overlapping seq values —
-- which is precisely the collision the up-migration exists to permit, so a
-- database that used the widened key is the one most likely to reject this.
-- Migrations here are forward-only in practice (see migrate.go); restore from
-- a backup rather than rolling this one back on a populated database.
ALTER TABLE search_job_results DROP CONSTRAINT search_job_results_pkey;
ALTER TABLE search_job_results ADD CONSTRAINT search_job_results_pkey
    PRIMARY KEY (job_id, seq);

-- Restore 000001's tenant index, which the up-migration drops as a redundant
-- prefix of the widened PK. Under the narrow (job_id, seq) PK it is load-bearing
-- again: nothing else indexes tenant_id.
CREATE INDEX IF NOT EXISTS idx_search_job_results_tenant ON search_job_results(tenant_id, job_id);
