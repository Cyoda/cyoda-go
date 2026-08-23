package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

type asyncSearchStore struct {
	db    *sql.DB
	clock Clock
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

	var pitMicro *int64
	if !job.PointInTime.IsZero() {
		v := job.PointInTime.UnixMicro()
		pitMicro = &v
	}

	var finishMicro *int64
	if job.FinishTime != nil {
		v := job.FinishTime.UnixMicro()
		finishMicro = &v
	}

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO search_jobs
		 (tenant_id, job_id, status, model_name, model_version, condition, point_in_time, search_opts, result_count, error, create_time, finish_time, calc_time_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(tid), job.ID, job.Status,
		job.ModelRef.EntityName, job.ModelRef.ModelVersion,
		nullableBlob(job.Condition), pitMicro,
		nullableBlob(job.SearchOpts), job.ResultCount,
		job.Error, job.CreateTime.UnixMicro(), finishMicro, job.CalcTimeMs)
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
	row := s.db.QueryRowContext(ctx,
		`SELECT job_id, tenant_id, status, model_name, model_version,
		        condition, point_in_time, search_opts, result_count,
		        error, create_time, finish_time, calc_time_ms
		 FROM search_jobs WHERE tenant_id = ? AND job_id = ?`,
		string(tid), jobID)
	return scanSearchJob(row)
}

func (s *asyncSearchStore) UpdateJobStatus(ctx context.Context, jobID string, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	var finishMicro *int64
	if !finishTime.IsZero() {
		v := finishTime.UnixMicro()
		finishMicro = &v
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE search_jobs SET status = ?, result_count = ?, error = ?, finish_time = ?, calc_time_ms = ?
		 WHERE tenant_id = ? AND job_id = ?`,
		status, resultCount, errMsg, finishMicro, calcTimeMs,
		string(tid), jobID)
	if err != nil {
		return fmt.Errorf("failed to update search job %s: %w", jobID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("search job %q not found", jobID)
	}
	return nil
}

func (s *asyncSearchStore) SaveResults(ctx context.Context, jobID string, entityIDs []string) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	var exists bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_jobs WHERE tenant_id = ? AND job_id = ?)`,
		string(tid), jobID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to verify job ownership: %w", err)
	}
	if !exists {
		return fmt.Errorf("search job %q not found", jobID)
	}

	if len(entityIDs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin tx for saving results: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO search_job_results (tenant_id, job_id, seq, entity_id) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("failed to prepare insert statement: %w", err)
	}
	defer stmt.Close()

	for i, eid := range entityIDs {
		if _, err := stmt.ExecContext(ctx, string(tid), jobID, i, eid); err != nil {
			return fmt.Errorf("failed to save result %d for job %s: %w", i, jobID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit results for job %s: %w", jobID, err)
	}
	return nil
}

func (s *asyncSearchStore) GetResultIDs(ctx context.Context, jobID string, offset, limit int) ([]string, int, error) {
	tid, err := s.tenant(ctx)
	if err != nil {
		return nil, 0, err
	}

	var exists bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_jobs WHERE tenant_id = ? AND job_id = ?)`,
		string(tid), jobID).Scan(&exists)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to check job existence: %w", err)
	}
	if !exists {
		return nil, 0, fmt.Errorf("search job %q not found", jobID)
	}

	var total int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM search_job_results WHERE tenant_id = ? AND job_id = ?`,
		string(tid), jobID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to count results for job %s: %w", jobID, err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT entity_id FROM search_job_results WHERE tenant_id = ? AND job_id = ? ORDER BY seq LIMIT ? OFFSET ?`,
		string(tid), jobID, limit, offset)
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

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin tx: %w", err)
	}
	defer tx.Rollback()

	// No CASCADE in SQLite STRICT tables without FK — delete results first.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM search_job_results WHERE tenant_id = ? AND job_id = ?`,
		string(tid), jobID); err != nil {
		return fmt.Errorf("failed to delete results for job %s: %w", jobID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM search_jobs WHERE tenant_id = ? AND job_id = ?`,
		string(tid), jobID); err != nil {
		return fmt.Errorf("failed to delete search job %s: %w", jobID, err)
	}

	return tx.Commit()
}

func (s *asyncSearchStore) Cancel(ctx context.Context, jobID string, finishTime time.Time) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	res, err := s.db.ExecContext(ctx,
		`UPDATE search_jobs SET status = 'CANCELLED', finish_time = ?
		 WHERE tenant_id = ? AND job_id = ? AND status NOT IN ('SUCCESSFUL', 'FAILED', 'CANCELLED')`,
		finishTime.UnixMicro(), string(tid), jobID)
	if err != nil {
		return fmt.Errorf("failed to cancel search job %s: %w", jobID, err)
	}

	n, _ := res.RowsAffected()
	if n > 0 {
		return nil
	}

	// No rows affected: either already terminal or does not exist.
	var exists bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM search_jobs WHERE tenant_id = ? AND job_id = ?)`,
		string(tid), jobID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check existence of search job %s: %w", jobID, err)
	}
	if !exists {
		return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
	}
	return nil
}

func (s *asyncSearchStore) ReapExpired(ctx context.Context, ttl time.Duration) (int, error) {
	cutoff := s.clock.Now().Add(-ttl)
	cutoffMicro := cutoff.UnixMicro()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to begin tx: %w", err)
	}
	defer tx.Rollback()

	// Delete results for expired jobs first.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM search_job_results WHERE (tenant_id, job_id) IN (
		   SELECT tenant_id, job_id FROM search_jobs
		   WHERE status != 'RUNNING' AND finish_time IS NOT NULL AND finish_time < ?
		 )`, cutoffMicro); err != nil {
		return 0, fmt.Errorf("failed to delete expired job results: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`DELETE FROM search_jobs WHERE status != 'RUNNING' AND finish_time IS NOT NULL AND finish_time < ?`,
		cutoffMicro)
	if err != nil {
		return 0, fmt.Errorf("failed to reap expired search jobs: %w", err)
	}

	n, _ := res.RowsAffected()

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit reap: %w", err)
	}
	return int(n), nil
}

// scanSearchJob reads a single SearchJob from a *sql.Row.
func scanSearchJob(row *sql.Row) (*spi.SearchJob, error) {
	var job spi.SearchJob
	var modelName, modelVer string
	var condition, searchOpts []byte
	var pitMicro, finishMicro sql.NullInt64
	var createMicro, calcTimeMs int64

	err := row.Scan(
		&job.ID, &job.TenantID, &job.Status,
		&modelName, &modelVer,
		&condition, &pitMicro,
		&searchOpts, &job.ResultCount,
		&job.Error, &createMicro, &finishMicro, &calcTimeMs)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("search job not found: %w", spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to scan search job: %w", err)
	}

	job.ModelRef = spi.ModelRef{EntityName: modelName, ModelVersion: modelVer}
	job.Condition = condition
	job.SearchOpts = searchOpts
	job.CreateTime = time.UnixMicro(createMicro)
	job.CalcTimeMs = calcTimeMs

	if pitMicro.Valid {
		job.PointInTime = time.UnixMicro(pitMicro.Int64)
	}
	if finishMicro.Valid {
		ft := time.UnixMicro(finishMicro.Int64)
		job.FinishTime = &ft
	}

	return &job, nil
}

// nullableBlob returns nil if the slice is empty, otherwise returns the slice.
// Prevents storing empty []byte as non-NULL in SQLite.
func nullableBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
