package postgres

import (
	"context"
	"fmt"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// asyncSearchStore implements spi.AsyncSearchStore backed by PostgreSQL.
// Unlike other stores, this is a long-lived singleton — tenant is resolved
// per method call from the context, not at construction time. This allows
// the store to be obtained at app startup with context.Background().
// ReapExpired operates cross-tenant (no tenant context required).
type asyncSearchStore struct {
	// q is the pool-pinned funnel: every statement here is classified, and none
	// of them joins a transaction the caller may be holding. See poolQuerier for
	// why the job record must stay independent of the submitting transaction.
	q Querier

	// pool is kept for the one operation Querier does not carry, SaveResults'
	// CopyFrom.
	pool *pgxpool.Pool

	// searchStatementTimeout is the ceiling the SCAN an async job runs is
	// bounded by — not any statement in this file, which are the ordinary
	// job-record reads and writes and belong under the interactive ceiling like
	// every other short statement.
	searchStatementTimeout time.Duration
}

// AsyncScanContext marks ctx as belonging to an async-search scan, so the entity
// store bounds that scan by this backend's own search ceiling instead of the
// interactive one the pool carries.
//
// The domain calls this on the background context its job goroutine runs under,
// discovering the method with a type assertion — async search is the one
// workload whose purpose is to run long, and a single shared ceiling would force
// operators to choose between fast-failing interactive writes and long
// analytical scans.
func (s *asyncSearchStore) AsyncScanContext(ctx context.Context) context.Context {
	return withSearchScanCeiling(ctx, s.searchStatementTimeout)
}

func (s *asyncSearchStore) tenant(ctx context.Context) (spi.TenantID, error) {
	uc := spi.GetUserContext(ctx)
	if uc == nil {
		return "", fmt.Errorf("no user context — tenant cannot be resolved")
	}
	if uc.Tenant.ID == "" {
		return "", fmt.Errorf("user context has no tenant")
	}
	return uc.Tenant.ID, nil
}

func (s *asyncSearchStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}
	// Enforce context tenant — never trust the struct field.
	_, err = s.q.Exec(ctx,
		`INSERT INTO search_jobs (id, tenant_id, status, model_name, model_ver, condition, point_in_time, search_opts, result_count, error, created_at, finished_at, calc_ms)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		job.ID, string(tid), job.Status,
		job.ModelRef.EntityName, job.ModelRef.ModelVersion,
		job.Condition, job.PointInTime,
		job.SearchOpts, job.ResultCount,
		job.Error, job.CreateTime, job.FinishTime, job.CalcTimeMs)
	if err != nil {
		return fmt.Errorf("failed to create search job %s: %w", job.ID, err)
	}
	return nil
}

func (s *asyncSearchStore) GetJob(ctx context.Context, jobID string) (*spi.SearchJob, error) {
	tid, err := s.tenant(ctx)
	if err != nil {
		return nil, err
	}
	row := s.q.QueryRow(ctx,
		`SELECT id, tenant_id, status, model_name, model_ver, condition, point_in_time, search_opts, result_count, error, created_at, finished_at, calc_ms
		 FROM search_jobs WHERE id = $1 AND tenant_id = $2`,
		jobID, string(tid))
	return scanSearchJob(row)
}

func (s *asyncSearchStore) UpdateJobStatus(ctx context.Context, jobID string, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}
	tag, err := s.q.Exec(ctx,
		`UPDATE search_jobs SET status = $1, result_count = $2, error = $3, finished_at = $4, calc_ms = $5
		 WHERE id = $6 AND tenant_id = $7`,
		status, resultCount, errMsg, finishTime, calcTimeMs,
		jobID, string(tid))
	if err != nil {
		return fmt.Errorf("failed to update search job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("search job %q not found", jobID)
	}
	return nil
}

func (s *asyncSearchStore) SaveResults(ctx context.Context, jobID string, entityIDs []string) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	// Verify job belongs to this tenant.
	var exists bool
	err = s.q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_jobs WHERE id = $1 AND tenant_id = $2)`,
		jobID, string(tid)).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to verify job ownership: %w", err)
	}
	if !exists {
		return fmt.Errorf("search job %q not found", jobID)
	}

	if len(entityIDs) == 0 {
		return nil
	}

	// Build rows for CopyFrom.
	rows := make([][]any, len(entityIDs))
	for i, eid := range entityIDs {
		rows[i] = []any{jobID, string(tid), i, eid}
	}

	// CopyFrom is not on Querier, so this is the one statement in the file that
	// classifies at the call site rather than inside the funnel. Same
	// classification, applied by hand.
	_, err = s.pool.CopyFrom(ctx,
		pgx.Identifier{"search_job_results"},
		[]string{"job_id", "tenant_id", "seq", "entity_id"},
		pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("failed to save results for job %s: %w", jobID, classifyError(err))
	}
	return nil
}

func (s *asyncSearchStore) GetResultIDs(ctx context.Context, jobID string, offset, limit int) ([]string, int, error) {
	tid, err := s.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}

	// Verify job exists and belongs to this tenant.
	var exists bool
	err = s.q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_jobs WHERE id = $1 AND tenant_id = $2)`,
		jobID, string(tid)).Scan(&exists)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to check job existence: %w", err)
	}
	if !exists {
		return nil, 0, fmt.Errorf("search job %q not found", jobID)
	}

	// Get total count.
	var total int
	err = s.q.QueryRow(ctx,
		`SELECT COUNT(*) FROM search_job_results WHERE job_id = $1 AND tenant_id = $2`,
		jobID, string(tid)).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count results for job %s: %w", jobID, err)
	}

	// Get paginated results.
	rows, err := s.q.Query(ctx,
		`SELECT entity_id FROM search_job_results WHERE job_id = $1 AND tenant_id = $2 ORDER BY seq OFFSET $3 LIMIT $4`,
		jobID, string(tid), offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to query results for job %s: %w", jobID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, 0, fmt.Errorf("failed to scan result row: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("row iteration error: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}

	return ids, total, nil
}

func (s *asyncSearchStore) DeleteJob(ctx context.Context, jobID string) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}
	// CASCADE on search_job_results handles cleanup.
	_, err = s.q.Exec(ctx,
		`DELETE FROM search_jobs WHERE id = $1 AND tenant_id = $2`,
		jobID, string(tid))
	if err != nil {
		return fmt.Errorf("failed to delete search job %s: %w", jobID, err)
	}
	return nil
}

// Cancel marks the job as CANCELLED. Idempotent: cancelling a job already in
// a terminal state returns nil. Cancelling a non-existent job returns
// spi.ErrNotFound.
func (s *asyncSearchStore) Cancel(ctx context.Context, jobID string) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	// Conditionally update: only transition to CANCELLED if currently RUNNING.
	tag, err := s.q.Exec(ctx,
		`UPDATE search_jobs SET status = 'CANCELLED'
		 WHERE id = $1 AND tenant_id = $2 AND status NOT IN ('SUCCESSFUL', 'FAILED', 'CANCELLED')`,
		jobID, string(tid))
	if err != nil {
		return fmt.Errorf("failed to cancel search job %s: %w", jobID, err)
	}

	if tag.RowsAffected() > 0 {
		// Successfully transitioned to CANCELLED.
		return nil
	}

	// No rows affected: either the job is already terminal (idempotent → nil)
	// or the job does not exist (→ ErrNotFound). Check existence.
	var exists bool
	err = s.q.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_jobs WHERE id = $1 AND tenant_id = $2)`,
		jobID, string(tid)).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check existence of search job %s: %w", jobID, err)
	}
	if !exists {
		return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
	}
	// Job exists but is already terminal — idempotent.
	return nil
}

func (s *asyncSearchStore) ReapExpired(ctx context.Context, ttl time.Duration) (int, error) {
	// ReapExpired runs cross-tenant — no tenant context required.
	cutoff := time.Now().UTC().Add(-ttl)
	tag, err := s.q.Exec(ctx,
		`DELETE FROM search_jobs WHERE status != 'RUNNING' AND finished_at IS NOT NULL AND finished_at < $1`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to reap expired search jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// scanSearchJob reads a single SearchJob from a pgx.Row.
func scanSearchJob(row pgx.Row) (*spi.SearchJob, error) {
	var job spi.SearchJob
	var modelName, modelVer string
	err := row.Scan(
		&job.ID, &job.TenantID, &job.Status,
		&modelName, &modelVer,
		&job.Condition, &job.PointInTime,
		&job.SearchOpts, &job.ResultCount,
		&job.Error, &job.CreateTime, &job.FinishTime, &job.CalcTimeMs)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("search job not found: %w", spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to scan search job: %w", err)
	}
	job.ModelRef = spi.ModelRef{EntityName: modelName, ModelVersion: modelVer}
	return &job, nil
}
