-- search_job_results' PK was (job_id, seq), omitting tenant_id even though
-- search_jobs deliberately allows the same job ID to be reused across
-- tenants (see 000001's comment on search_jobs). SaveResults restarts seq
-- at 0 for every job, so two tenants both using job ID "j1" collide on
-- (job_id, seq) pairs. Widen the PK to (tenant_id, job_id, seq), matching
-- the tenant-scoped uniqueness search_jobs already has, and matching the
-- sqlite plugin's equivalent PK.
--
-- search_job_results is a small, per-job results table (never queried
-- across the whole table without a job_id filter), so the ACCESS EXCLUSIVE
-- lock this PK rebuild takes is not subject to the CONCURRENTLY convention
-- migration_index_guard_test.go enforces for entities/entity_versions-scale
-- tables.
ALTER TABLE search_job_results DROP CONSTRAINT search_job_results_pkey;
ALTER TABLE search_job_results ADD CONSTRAINT search_job_results_pkey
    PRIMARY KEY (tenant_id, job_id, seq);
