package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
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

	// epoch is deliberately not in the column list: the schema's
	// `DEFAULT 1` supplies it, so CreateJob always persists 1 regardless
	// of the value the caller set on job.Epoch. heartbeat_time is
	// likewise omitted — it defaults to NULL (never stamped).
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

// searchJobColumns is the column list shared by every full-row read of
// search_jobs (GetJob and ClaimStale's post-claim re-read), so the scan
// order in scanSearchJob only has to be kept in step with one place.
const searchJobColumns = `job_id, tenant_id, status, model_name, model_version,
	        condition, point_in_time, search_opts, result_count,
	        error, create_time, finish_time, calc_time_ms,
	        heartbeat_time, epoch`

func (s *asyncSearchStore) GetJob(ctx context.Context, jobID string) (*spi.SearchJob, error) {
	tid, err := s.tenant(ctx)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+searchJobColumns+`
		 FROM search_jobs WHERE tenant_id = ? AND job_id = ?`,
		string(tid), jobID)
	return scanSearchJob(row)
}

func (s *asyncSearchStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	var finishMicro *int64
	if !finishTime.IsZero() {
		v := finishTime.UnixMicro()
		finishMicro = &v
	}

	return fencedUpdate(ctx, s.db, tid, jobID, epoch,
		"status = ?, result_count = ?, error = ?, finish_time = ?, calc_time_ms = ?",
		status, resultCount, errMsg, finishMicro, calcTimeMs)
}

// searchResultsChunkSize bounds each SaveResults write-tx so a large result
// set is streamed in bounded-size commits rather than one unbounded
// transaction.
const searchResultsChunkSize = 500

func (s *asyncSearchStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	// seq is a running in-call counter, not read back from the DB: rows
	// are always empty at the start of a claim epoch (ClearResults runs
	// before a reclaimed job's writer resumes), so starting at 0 every
	// call never collides with a prior epoch's rows.
	seq := 0
	chunk := make([]string, 0, searchResultsChunkSize)

	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin tx for saving results of job %s: %w", jobID, err)
		}
		defer tx.Rollback()

		// Fence before writing anything: a no-op SET so the shared
		// conditional-write+probe shape stays uniform across every
		// fenced write, with no side effect of its own.
		if err := fencedUpdate(ctx, tx, tid, jobID, epoch, "epoch = epoch"); err != nil {
			return err
		}

		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO search_job_results (tenant_id, job_id, seq, entity_id) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("failed to prepare insert statement for job %s: %w", jobID, err)
		}
		defer stmt.Close()

		for _, eid := range chunk {
			if _, err := stmt.ExecContext(ctx, string(tid), jobID, seq, eid); err != nil {
				return fmt.Errorf("failed to save result %d for job %s: %w", seq, jobID, err)
			}
			seq++
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit results chunk for job %s: %w", jobID, err)
		}
		chunk = chunk[:0]
		return nil
	}

	for eid := range entityIDs {
		chunk = append(chunk, eid)
		if len(chunk) == searchResultsChunkSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (s *asyncSearchStore) GetResultIDs(ctx context.Context, jobID string, offset, limit int) ([]string, int, error) {
	if offset < 0 || limit < 1 {
		return nil, 0, fmt.Errorf("invalid pagination: offset=%d limit=%d (offset must be >= 0, limit must be >= 1)", offset, limit)
	}

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

func (s *asyncSearchStore) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	now := s.clock.Now().UnixMicro()
	return fencedUpdate(ctx, s.db, tid, jobID, epoch, "heartbeat_time = ?", now)
}

// staleClaimCandidate is the shape of one row scanned from the ClaimStale
// staleness scan, before the per-candidate CAS decides whether it was
// actually won.
type staleClaimCandidate struct {
	tenantID string
	jobID    string
	epoch    int64
}

func (s *asyncSearchStore) ClaimStale(ctx context.Context, staleAfter time.Duration, limit int) ([]*spi.SearchJob, error) {
	cutoffMicro := s.clock.Now().Add(-staleAfter).UnixMicro()

	// Cross-tenant by design (search_store.go interface doc): a reclaim
	// sweep runs on behalf of the whole cluster, not one tenant, so no
	// tenant filter here — see spi.AsyncSearchStore.ClaimStale.
	rows, err := s.db.QueryContext(ctx,
		`SELECT tenant_id, job_id, epoch FROM search_jobs
		 WHERE status = 'RUNNING' AND COALESCE(heartbeat_time, create_time) < ?
		 LIMIT ?`,
		cutoffMicro, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to scan for stale search jobs: %w", err)
	}

	var candidates []staleClaimCandidate
	for rows.Next() {
		var c staleClaimCandidate
		if err := rows.Scan(&c.tenantID, &c.jobID, &c.epoch); err != nil {
			rows.Close()
			return nil, fmt.Errorf("failed to scan stale search job row: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("row iteration error scanning stale search jobs: %w", err)
	}
	rows.Close()

	// Rows must be fully drained and closed above before any further
	// query runs against s.db: the sqlite factory pins the connection
	// pool to a single connection, so an open *Rows on this goroutine
	// would otherwise deadlock the CAS updates below.
	now := s.clock.Now().UnixMicro()
	var claimed []*spi.SearchJob
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		res, err := s.db.ExecContext(ctx,
			`UPDATE search_jobs SET epoch = epoch + 1, heartbeat_time = ?
			 WHERE tenant_id = ? AND job_id = ? AND epoch = ? AND status = 'RUNNING'`,
			now, c.tenantID, c.jobID, c.epoch)
		if err != nil {
			return nil, fmt.Errorf("failed to claim search job %s: %w", c.jobID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// Lost the CAS: another claimer (or a status change) got
			// there first. Not our job to claim.
			continue
		}

		row := s.db.QueryRowContext(ctx,
			`SELECT `+searchJobColumns+`
			 FROM search_jobs WHERE tenant_id = ? AND job_id = ?`,
			c.tenantID, c.jobID)
		job, err := scanSearchJob(row)
		if err != nil {
			return nil, fmt.Errorf("failed to re-read claimed search job %s: %w", c.jobID, err)
		}
		claimed = append(claimed, job)
	}

	return claimed, nil
}

func (s *asyncSearchStore) ClearResults(ctx context.Context, jobID string) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM search_job_results WHERE tenant_id = ? AND job_id = ?`,
		string(tid), jobID); err != nil {
		return fmt.Errorf("failed to clear results for job %s: %w", jobID, err)
	}
	return nil
}

// dbExecer is satisfied by both *sql.DB and *sql.Tx, letting fencedUpdate
// run inside an explicit transaction (SaveResults chunks) or directly against
// the pool (UpdateJobStatus, Heartbeat) with the same code path.
type dbExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// isTerminalSearchStatus reports whether status is a terminal SearchJob
// status (SUCCESSFUL/FAILED/CANCELLED) — write-once per the AsyncSearchStore
// contract.
func isTerminalSearchStatus(status string) bool {
	switch status {
	case "SUCCESSFUL", "FAILED", "CANCELLED":
		return true
	}
	return false
}

// fencedUpdate is the shared shape for every epoch-fenced write against
// search_jobs (UpdateJobStatus, Heartbeat, and each SaveResults chunk): a
// single conditional UPDATE guarded by tenant, job, epoch, and non-terminal
// status. Zero rows affected is classified by a follow-up probe: no row ->
// ErrNotFound, a terminal status -> ErrAlreadyTerminal, anything else ->
// ErrStaleClaim (the epoch didn't match).
func fencedUpdate(ctx context.Context, ex dbExecer, tid spi.TenantID, jobID string, epoch int64, setClause string, setArgs ...any) error {
	args := make([]any, 0, len(setArgs)+3)
	args = append(args, setArgs...)
	args = append(args, string(tid), jobID, epoch)

	res, err := ex.ExecContext(ctx,
		`UPDATE search_jobs SET `+setClause+`
		 WHERE tenant_id = ? AND job_id = ? AND epoch = ?
		   AND status NOT IN ('SUCCESSFUL', 'FAILED', 'CANCELLED')`,
		args...)
	if err != nil {
		return fmt.Errorf("failed to update search job %s: %w", jobID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}

	var status string
	var actualEpoch int64
	err = ex.QueryRowContext(ctx,
		`SELECT status, epoch FROM search_jobs WHERE tenant_id = ? AND job_id = ?`,
		string(tid), jobID).Scan(&status, &actualEpoch)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
		}
		return fmt.Errorf("failed to probe search job %s: %w", jobID, err)
	}
	if isTerminalSearchStatus(status) {
		return fmt.Errorf("search job %q is in terminal status %s: %w", jobID, status, spi.ErrAlreadyTerminal)
	}
	return fmt.Errorf("search job %q: caller epoch %d does not match current epoch %d: %w", jobID, epoch, actualEpoch, spi.ErrStaleClaim)
}

// scanSearchJob reads a single SearchJob from a *sql.Row, matching the
// searchJobColumns projection.
func scanSearchJob(row *sql.Row) (*spi.SearchJob, error) {
	var job spi.SearchJob
	var modelName, modelVer string
	var condition, searchOpts []byte
	var pitMicro, finishMicro, heartbeatMicro sql.NullInt64
	var createMicro, calcTimeMs, epoch int64

	err := row.Scan(
		&job.ID, &job.TenantID, &job.Status,
		&modelName, &modelVer,
		&condition, &pitMicro,
		&searchOpts, &job.ResultCount,
		&job.Error, &createMicro, &finishMicro, &calcTimeMs,
		&heartbeatMicro, &epoch)
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
	job.Epoch = epoch

	if pitMicro.Valid {
		job.PointInTime = time.UnixMicro(pitMicro.Int64)
	}
	if finishMicro.Valid {
		ft := time.UnixMicro(finishMicro.Int64)
		job.FinishTime = &ft
	}
	if heartbeatMicro.Valid {
		ht := time.UnixMicro(heartbeatMicro.Int64)
		job.HeartbeatTime = &ht
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
