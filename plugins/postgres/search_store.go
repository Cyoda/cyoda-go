package postgres

import (
	"context"
	"fmt"
	"iter"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// asyncSearchStore implements spi.AsyncSearchStore backed by PostgreSQL.
// Unlike other stores, this is a long-lived singleton — tenant is resolved
// per method call from the context, not at construction time. This allows
// the store to be obtained at app startup with context.Background().
// ReapExpired and ClaimStale operate cross-tenant (no tenant context
// required).
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

// searchJobColumns and qualifiedSearchJobColumns are the SELECT/RETURNING
// column list every job read uses, in the order scanSearchJobRow expects.
// The qualified variant is for statements that join search_jobs under an
// alias (ClaimStale's UPDATE ... FROM).
const searchJobColumns = `id, tenant_id, status, model_name, model_ver, condition, point_in_time, search_opts, result_count, error, created_at, finished_at, calc_ms, heartbeat_time, epoch`
const qualifiedSearchJobColumns = `j.id, j.tenant_id, j.status, j.model_name, j.model_ver, j.condition, j.point_in_time, j.search_opts, j.result_count, j.error, j.created_at, j.finished_at, j.calc_ms, j.heartbeat_time, j.epoch`

// isTerminalSearchStatus reports whether status is one of the write-once
// terminal statuses (spi.AsyncSearchStore godoc).
func isTerminalSearchStatus(status string) bool {
	switch status {
	case "SUCCESSFUL", "FAILED", "CANCELLED":
		return true
	default:
		return false
	}
}

func (s *asyncSearchStore) CreateJob(ctx context.Context, job *spi.SearchJob) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}
	// Enforce context tenant — never trust the struct field. Epoch is always
	// persisted as 1 regardless of job.Epoch (spi.AsyncSearchStore.CreateJob
	// contract) — heartbeat_time is left NULL (never stamped).
	_, err = s.q.Exec(ctx,
		`INSERT INTO search_jobs (id, tenant_id, status, model_name, model_ver, condition, point_in_time, search_opts, result_count, error, created_at, finished_at, calc_ms, epoch)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 1)`,
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
		`SELECT `+searchJobColumns+` FROM search_jobs WHERE id = $1 AND tenant_id = $2`,
		jobID, string(tid))
	return scanSearchJob(row)
}

// probeFenced classifies why a fenced write against jobID did not apply: no
// row -> spi.ErrNotFound, a terminal status -> spi.ErrAlreadyTerminal,
// otherwise the epoch does not match -> spi.ErrStaleClaim (these are the only
// three ways the fenced UPDATEs below can affect zero rows). SaveResults also
// calls this directly before each batch, since its writes land in
// search_job_results rather than search_jobs — there is no single UPDATE to
// fence there.
func (s *asyncSearchStore) probeFenced(ctx context.Context, jobID string, tid spi.TenantID, epoch int64) error {
	var status string
	var actualEpoch int64
	err := s.q.QueryRow(ctx,
		`SELECT status, epoch FROM search_jobs WHERE id = $1 AND tenant_id = $2`,
		jobID, string(tid)).Scan(&status, &actualEpoch)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
		}
		return fmt.Errorf("failed to probe search job %s: %w", jobID, err)
	}
	if isTerminalSearchStatus(status) {
		return fmt.Errorf("search job %q is terminal: %w", jobID, spi.ErrAlreadyTerminal)
	}
	if actualEpoch != epoch {
		return fmt.Errorf("search job %q epoch mismatch (have %d, want %d): %w", jobID, actualEpoch, epoch, spi.ErrStaleClaim)
	}
	return nil
}

// UpdateJobStatus is a single fenced conditional UPDATE: it only applies
// against a RUNNING job at the given epoch. A zero-finishTime is stored as
// NULL rather than a real zero-value timestamp (spi.AsyncSearchStore
// contract).
func (s *asyncSearchStore) UpdateJobStatus(ctx context.Context, jobID string, epoch int64, status string, resultCount int, errMsg string, finishTime time.Time, calcTimeMs int64) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	var finishArg *time.Time
	if !finishTime.IsZero() {
		finishArg = &finishTime
	}

	tag, err := s.q.Exec(ctx,
		`UPDATE search_jobs SET status = $1, result_count = $2, error = $3, finished_at = $4, calc_ms = $5
		 WHERE id = $6 AND tenant_id = $7 AND epoch = $8
		   AND status NOT IN ('SUCCESSFUL', 'FAILED', 'CANCELLED')`,
		status, resultCount, errMsg, finishArg, calcTimeMs,
		jobID, string(tid), epoch)
	if err != nil {
		return fmt.Errorf("failed to update search job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.probeFenced(ctx, jobID, tid, epoch)
	}
	return nil
}

// Heartbeat is a fenced conditional UPDATE, stamping heartbeat_time from the
// database's own clock. ClaimStale's staleness comparison uses the same
// server-side now() — same clock domain, no host/DB skew between the stamp
// and the read that later judges it stale.
func (s *asyncSearchStore) Heartbeat(ctx context.Context, jobID string, epoch int64) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	tag, err := s.q.Exec(ctx,
		`UPDATE search_jobs SET heartbeat_time = now()
		 WHERE id = $1 AND tenant_id = $2 AND epoch = $3
		   AND status NOT IN ('SUCCESSFUL', 'FAILED', 'CANCELLED')`,
		jobID, string(tid), epoch)
	if err != nil {
		return fmt.Errorf("failed to heartbeat search job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.probeFenced(ctx, jobID, tid, epoch)
	}
	return nil
}

// saveResultsBatchSize is the CopyFrom chunk size: large enough to amortise
// round trips, small enough that the pool connection CopyFrom pins is held
// for one chunk, not the whole (potentially very large) result stream.
const saveResultsBatchSize = 1000

// SaveResults streams entityIDs into search_job_results in
// saveResultsBatchSize chunks. Each chunk starts with a ctx.Err() check and a
// fenced probe (see probeFenced) — SaveResults itself never writes to
// search_jobs, so this is the only fencing the write gets — then a single
// pool.CopyFrom for the chunk. seq is a running counter across the whole
// call, so chunk boundaries never repeat or skip a sequence position.
func (s *asyncSearchStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	seq := 0
	batch := make([]string, 0, saveResultsBatchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.probeFenced(ctx, jobID, tid, epoch); err != nil {
			return err
		}

		rows := make([][]any, len(batch))
		for i, eid := range batch {
			rows[i] = []any{jobID, string(tid), seq, eid}
			seq++
		}

		// CopyFrom is not on Querier, so this batch classifies at the call
		// site rather than inside the funnel. Same classification, applied by
		// hand — and only for the duration of this one batch's COPY, not the
		// whole (potentially many-batch) call.
		_, err := s.pool.CopyFrom(ctx,
			pgx.Identifier{"search_job_results"},
			[]string{"job_id", "tenant_id", "seq", "entity_id"},
			pgx.CopyFromRows(rows))
		if err != nil {
			return fmt.Errorf("failed to save results batch for job %s: %w", jobID, classifyError(err))
		}
		batch = batch[:0]
		return nil
	}

	for eid := range entityIDs {
		batch = append(batch, eid)
		if len(batch) == saveResultsBatchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

func (s *asyncSearchStore) GetResultIDs(ctx context.Context, jobID string, offset, limit int) ([]string, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("get result ids for job %s: offset must be >= 0, got %d", jobID, offset)
	}
	if limit < 1 {
		return nil, 0, fmt.Errorf("get result ids for job %s: limit must be >= 1, got %d", jobID, limit)
	}

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

// ClearResults deletes the job's persisted result IDs. Idempotent — deleting
// zero rows is not an error.
func (s *asyncSearchStore) ClearResults(ctx context.Context, jobID string) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}
	_, err = s.q.Exec(ctx,
		`DELETE FROM search_job_results WHERE job_id = $1 AND tenant_id = $2`,
		jobID, string(tid))
	if err != nil {
		return fmt.Errorf("failed to clear results for job %s: %w", jobID, err)
	}
	return nil
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

// Cancel marks the job CANCELLED and stamps finishTime on the transition.
// Idempotent: cancelling a job already in a terminal state returns nil and
// does not overwrite the existing finish time. Cancelling a non-existent
// job returns spi.ErrNotFound. Cancel is not epoch-fenced — it is the sole
// idempotent-nil exception to fencing (spi.AsyncSearchStore godoc), so any
// caller (not just the current claim holder) may cancel a RUNNING job.
func (s *asyncSearchStore) Cancel(ctx context.Context, jobID string, finishTime time.Time) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	// Conditionally update: only transition to CANCELLED if currently RUNNING.
	tag, err := s.q.Exec(ctx,
		`UPDATE search_jobs SET status = 'CANCELLED', finished_at = $3
		 WHERE id = $1 AND tenant_id = $2 AND status NOT IN ('SUCCESSFUL', 'FAILED', 'CANCELLED')`,
		jobID, string(tid), finishTime)
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

// ClaimStale atomically claims up to limit RUNNING jobs whose heartbeat (or
// created_at, when heartbeat_time is NULL) is older than staleAfter, bumping
// epoch and refreshing heartbeat_time on every winner in the same statement
// that selected them.
//
// The candidate selection and the claiming UPDATE are one statement (a CTE
// feeding an UPDATE ... FROM), so the FOR UPDATE SKIP LOCKED in the CTE and
// the UPDATE's row lock cover the same rows without a race window between
// them: two concurrent ClaimStale calls see disjoint candidate sets because
// SKIP LOCKED excludes whatever the other caller's CTE has already locked.
//
// staleAfter is passed as microseconds (an integer) rather than a formatted
// interval string, so there is no locale/format ambiguity in what postgres
// parses. The comparison and the heartbeat stamp both use the database's own
// now() — the same clock domain, per the Heartbeat doc comment — so a
// concurrent claim can never race the stamp it is judged against.
func (s *asyncSearchStore) ClaimStale(ctx context.Context, staleAfter time.Duration, limit int) ([]*spi.SearchJob, error) {
	rows, err := s.q.Query(ctx,
		`WITH claimed AS (
		   SELECT tenant_id, id FROM search_jobs
		   WHERE status = 'RUNNING'
		     AND COALESCE(heartbeat_time, created_at) < now() - ($1 * interval '1 microsecond')
		   ORDER BY created_at
		   LIMIT $2
		   FOR UPDATE SKIP LOCKED
		 )
		 UPDATE search_jobs j SET epoch = j.epoch + 1, heartbeat_time = now()
		 FROM claimed c
		 WHERE j.tenant_id = c.tenant_id AND j.id = c.id
		 RETURNING `+qualifiedSearchJobColumns,
		staleAfter.Microseconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to claim stale search jobs: %w", err)
	}
	defer rows.Close()

	claimed := []*spi.SearchJob{}
	for rows.Next() {
		job, err := scanSearchJobRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("failed to scan claimed search job: %w", err)
		}
		claimed = append(claimed, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error claiming stale search jobs: %w", err)
	}
	return claimed, nil
}

// scanSearchJobRow scans one row via scan, the shared signature of
// pgx.Row.Scan and pgx.Rows.Scan — so both GetJob (single row) and
// ClaimStale (a result set) can share one scan body.
func scanSearchJobRow(scan func(dest ...any) error) (*spi.SearchJob, error) {
	var job spi.SearchJob
	var modelName, modelVer string
	err := scan(
		&job.ID, &job.TenantID, &job.Status,
		&modelName, &modelVer,
		&job.Condition, &job.PointInTime,
		&job.SearchOpts, &job.ResultCount,
		&job.Error, &job.CreateTime, &job.FinishTime, &job.CalcTimeMs,
		&job.HeartbeatTime, &job.Epoch)
	if err != nil {
		return nil, err
	}
	job.ModelRef = spi.ModelRef{EntityName: modelName, ModelVersion: modelVer}
	return &job, nil
}

// scanSearchJob reads a single SearchJob from a pgx.Row, translating a
// no-rows result into spi.ErrNotFound.
func scanSearchJob(row pgx.Row) (*spi.SearchJob, error) {
	job, err := scanSearchJobRow(row.Scan)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, fmt.Errorf("search job not found: %w", spi.ErrNotFound)
		}
		return nil, fmt.Errorf("failed to scan search job: %w", err)
	}
	return job, nil
}
