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
	// why the job record must stay independent of the submitting transaction,
	// and unjoinedQuerier for why a statement issued from INSIDE a caller's
	// transaction bounds its connection acquire — it holds two at once.
	q Querier

	// pool is kept for the operations Querier does not carry: SaveResults
	// opens a per-chunk transaction on it (Begin) and streams the chunk
	// through it (CopyFrom) so the fence check and the write are atomic.
	pool *pgxpool.Pool

	// acquireTimeout bounds the connection acquire for SaveResults' per-chunk
	// Begin — see newAcquireContext. It never reaches the transaction handle
	// itself, only the wait for a pooled connection.
	acquireTimeout time.Duration

	// searchStatementTimeout is the ceiling the SCAN an async job runs is
	// bounded by — not any statement in this file, which are the ordinary
	// job-record reads and writes and belong under the interactive ceiling like
	// every other short statement.
	searchStatementTimeout time.Duration
}

// acquireContext bounds SaveResults' per-chunk Begin. See newAcquireContext
// for why the deadline must never reach the transaction handle it returns.
func (s *asyncSearchStore) acquireContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return newAcquireContext(ctx, s.acquireTimeout)
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
//
// q is explicit rather than always s.q: UpdateJobStatus and Heartbeat probe
// on the pool-pinned querier after their own atomic UPDATE already found
// zero rows (probeFenced only classifies why), but SaveResults probes and
// writes as two separate statements, so it passes a querier bound to its own
// per-chunk transaction (see forUpdate below).
//
// forUpdate takes SELECT ... FOR UPDATE, locking the row for the rest of the
// caller's transaction. SaveResults sets it so the fence-check and the
// results write it gates are atomic against a concurrent ClaimStale or
// terminal write — both of which are themselves single UPDATEs against this
// row, so they block on the lock until the fenced transaction commits or
// rolls back, closing the window a plain SELECT would leave between "epoch
// confirmed valid" and "rows durably written". UpdateJobStatus and Heartbeat
// pass false: their probe runs standalone on the pool, and a lock held past
// the SELECT's return would outlive the statement for no purpose.
func (s *asyncSearchStore) probeFenced(ctx context.Context, q Querier, jobID string, tid spi.TenantID, epoch int64, forUpdate bool) error {
	query := `SELECT status, epoch FROM search_jobs WHERE id = $1 AND tenant_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var status string
	var actualEpoch int64
	err := q.QueryRow(ctx, query, jobID, string(tid)).Scan(&status, &actualEpoch)
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
		return s.probeFenced(ctx, s.q, jobID, tid, epoch, false)
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
		return s.probeFenced(ctx, s.q, jobID, tid, epoch, false)
	}
	return nil
}

// saveResultsBatchSize is the CopyFrom chunk size: large enough to amortise
// round trips, small enough that the transaction each chunk opens is held
// for one chunk, not the whole (potentially very large) result stream.
const saveResultsBatchSize = 1000

// SaveResults streams entityIDs into search_job_results in
// saveResultsBatchSize chunks. Each chunk starts with a ctx.Err() check, then
// opens its own transaction that fences (see probeFenced, forUpdate=true) and
// CopyFrom's the chunk before committing — SaveResults itself never writes to
// search_jobs, so this is the only fencing the write gets, and the row lock
// the fence takes makes the two atomic against a concurrent ClaimStale or
// terminal write: neither can land between "epoch confirmed valid" and "rows
// durably written" because both are themselves UPDATEs against the same
// locked row. Deliberately one transaction per chunk, not one for the whole
// call: a job can run for hours, and holding a pooled connection that long
// would starve the pool's small connection budget. seq is a running counter
// across the whole call, so chunk boundaries never repeat or skip a sequence
// position.
//
// The final flush runs even when it has nothing to write, so an empty
// iter.Seq — an ordinary search matching zero rows — is fenced like any other:
// a missing job is ErrNotFound, a terminal one ErrAlreadyTerminal, and a
// reclaimed claim ErrStaleClaim. spi.AsyncSearchStore states the fence
// unconditionally, and the memory plugin already reads it that way
// (guardAndAppend(nil)); returning nil here for a zero-result set would leave a
// dispossessed executor believing its write landed.
func (s *asyncSearchStore) SaveResults(ctx context.Context, jobID string, epoch int64, entityIDs iter.Seq[string]) error {
	tid, err := s.tenant(ctx)
	if err != nil {
		return err
	}

	seq := 0
	batch := make([]string, 0, saveResultsBatchSize)

	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Acquire-only deadline, cancelled the instant Begin returns — see
		// newAcquireContext — so the transaction handle that outlives this
		// call never inherits it.
		acquireCtx, cancelAcquire := s.acquireContext(ctx)
		tx, err := s.pool.BeginTx(acquireCtx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		cancelAcquire()
		if err != nil {
			return classifyAcquireErr(ctx, acquireCtx,
				fmt.Sprintf("failed to begin results-chunk tx for job %s", jobID), err)
		}
		// Rollback is idempotent in pgx: a no-op once Commit has landed, and
		// this is the only path a probeFenced failure or a CopyFrom failure
		// takes — releasing the row lock either way.
		//
		// On a context derived WithoutCancel, matching searcher.go and
		// grouped_stats.go. SaveResults IS the cancellation path — CancelAsync
		// cancels the very context the streaming write runs under — so the
		// caller's context is routinely the thing that expired by the time this
		// runs. A rollback issued on an expired context never reaches the
		// server, leaving the transaction status non-idle, and pgxpool.Release
		// then destroys the connection instead of returning it: every
		// submit-then-cancel cycle would burn one of the pool's connections and
		// pay a fresh TCP+auth handshake to replace it.
		defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

		// classifiedQuerier keeps this statement on the same error
		// classification every other statement in this store gets from the
		// pool-pinned s.q, even though it runs on our own private tx.
		//
		// The caller's own ctx, NOT WithoutCancel: this is work whose result the
		// caller is waiting for, so a cancellation must abort it. Only the
		// rollback above outlives the cancellation, because its whole job is to
		// clean up after one.
		if err := s.probeFenced(ctx, classifiedQuerier{inner: tx}, jobID, tid, epoch, true); err != nil {
			return err
		}

		if len(batch) > 0 {
			rows := make([][]any, len(batch))
			for i, eid := range batch {
				rows[i] = []any{jobID, string(tid), seq, eid}
				seq++
			}

			// CopyFrom is not on Querier, so this batch classifies at the call
			// site rather than inside the funnel. Same classification, applied
			// by hand.
			if _, err := tx.CopyFrom(ctx,
				pgx.Identifier{"search_job_results"},
				[]string{"job_id", "tenant_id", "seq", "entity_id"},
				pgx.CopyFromRows(rows)); err != nil {
				return fmt.Errorf("failed to save results batch for job %s: %w", jobID, classifyError(err))
			}
		}

		// Also the caller's own ctx, and deliberately so: commit semantics are
		// not rollback semantics. A cancelled call must not make its work
		// durable — the abandoned chunk fails here and the deferred rollback
		// (which does outlive the cancellation) both discards it and returns
		// the connection to the pool.
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("failed to commit results batch for job %s: %w", jobID, classifyError(err))
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

// getResultIDsQuery reads the job's existence, its total result count and one
// page of result IDs in ONE statement, so all three describe a single snapshot.
//
// Reading a non-terminal job is explicitly supported (spi.AsyncSearchStore:
// "reading a non-terminal job answers with the results saved so far"), so a
// concurrent SaveResults chunk landing between separate count and page
// statements is reachable — and would report a total that does not describe the
// page returned with it. The memory plugin takes both under one RLock; under
// READ COMMITTED a single statement is postgres's equivalent, since every row
// it reads comes from the same statement snapshot.
//
// Shape: `total` is a one-row CTE, so the LEFT JOIN LATERAL always yields at
// least one row even when the page is empty (offset past the end, or no results
// yet) — that row carries the count with a NULL entity_id. A plain
// `count(*) OVER ()` would lose the total exactly when the page is empty.
const getResultIDsQuery = `
WITH total AS (
    SELECT count(*) AS n FROM search_job_results WHERE job_id = $1 AND tenant_id = $2
)
SELECT EXISTS(SELECT 1 FROM search_jobs WHERE id = $1 AND tenant_id = $2) AS job_exists,
       total.n,
       page.entity_id
FROM total
LEFT JOIN LATERAL (
    SELECT entity_id FROM search_job_results
    WHERE job_id = $1 AND tenant_id = $2
    ORDER BY seq
    OFFSET $3 LIMIT $4
) page ON true`

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

	rows, err := s.q.Query(ctx, getResultIDsQuery, jobID, string(tid), offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to query results for job %s: %w", jobID, err)
	}
	defer rows.Close()

	ids := []string{}
	total := 0
	exists := false
	for rows.Next() {
		var id *string
		if err := rows.Scan(&exists, &total, &id); err != nil {
			return nil, 0, fmt.Errorf("failed to scan result row: %w", err)
		}
		// NULL entity_id is the LATERAL's no-page-row marker, not a result.
		if id != nil {
			ids = append(ids, *id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("row iteration error: %w", err)
	}
	if !exists {
		return nil, 0, fmt.Errorf("search job %q not found: %w", jobID, spi.ErrNotFound)
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

	// nil, not an empty slice, when nothing was claimed — the shape the memory
	// and sqlite plugins already return (spi.AsyncSearchStore.ClaimStale's
	// godoc specifies neither, so the other two decide it).
	var claimed []*spi.SearchJob
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
