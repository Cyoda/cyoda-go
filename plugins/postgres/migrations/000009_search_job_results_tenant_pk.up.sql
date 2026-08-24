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

-- idx_search_job_results_tenant (tenant_id, job_id) was created by 000001 to
-- serve tenant-scoped lookups back when the PK was (job_id, seq) and nothing
-- else could. It is now a strict PREFIX of the new PK, so every lookup it could
-- answer — the paged read, the count, the ClearResults DELETE, the ON DELETE
-- CASCADE probe from search_jobs, and the RLS tenant_id predicate — plans on
-- search_job_results_pkey instead (verified by EXPLAIN, see
-- search_job_results_index_test.go). All it still costs is a second B-tree
-- write per row on SaveResults' CopyFrom.
--
-- Plain DROP INDEX, not CONCURRENTLY, matching the deliberate choice recorded
-- in migration_index_guard_test.go: the concurrent forms deadlock this
-- project's multi-node boot path, and this table is small and per-job.
DROP INDEX IF EXISTS idx_search_job_results_tenant;
