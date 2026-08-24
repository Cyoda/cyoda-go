package postgres_test

// search_store_fencing_test.go — the fencing, cancellation and read-consistency
// contracts of the PostgreSQL AsyncSearchStore that search_store_test.go's
// happy-path coverage does not reach.

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// newRunningJob is the minimal RUNNING job every test in this file starts
// from: the fields SaveResults/ClaimStale fence on, and nothing else.
func newRunningJob(id string, tenant spi.TenantID, createTime time.Time) *spi.SearchJob {
	return &spi.SearchJob{
		ID:         id,
		TenantID:   tenant,
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "Fenced", ModelVersion: "1"},
		Condition:  json.RawMessage(`{}`),
		CreateTime: createTime,
	}
}

// emptySeq is an iter.Seq[string] that yields nothing — the ordinary outcome of
// a search matching zero rows.
func emptySeq() iter.Seq[string] { return func(func(string) bool) {} }

// ---------------------------------------------------------------------------
// SaveResults fences on an empty sequence
// ---------------------------------------------------------------------------

// TestPGSearchStore_SaveResults_EmptySequence_FencesOnStaleEpoch pins the
// epoch fence for the zero-result case. A search matching nothing is an
// ordinary outcome, so an executor whose claim was reclaimed by ClaimStale
// (epoch bumped out from under it) must be refused with ErrStaleClaim whether
// its result set is empty or not — the memory plugin's guardAndAppend(nil)
// does exactly this, and spi.AsyncSearchStore's godoc makes the fence
// unconditional ("Fences on epoch (ErrStaleClaim) and terminal status").
//
// Before the fix, postgres' flush() returned nil for an empty batch without
// ever touching the database, so a reclaimed executor got nil here and
// ErrStaleClaim on memory — a backend divergence on one contract.
func TestPGSearchStore_SaveResults_EmptySequence_FencesOnStaleEpoch(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "empty-fence-tenant")
	ctx := ctxWithTenant("empty-fence-tenant")

	// created_at well in the past so ClaimStale's staleness window covers it.
	stale := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	if err := store.CreateJob(ctx, newRunningJob("job-empty-stale", "empty-fence-tenant", stale)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	claimed, err := store.ClaimStale(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	if len(claimed) != 1 || claimed[0].Epoch != 2 {
		t.Fatalf("ClaimStale: got %d jobs (epoch of first, if any, %v), want 1 job at epoch 2",
			len(claimed), epochOfFirst(claimed))
	}

	// The dispossessed executor still holds epoch 1 and its scan matched
	// nothing. The write must still be fenced off.
	err = store.SaveResults(ctx, "job-empty-stale", 1, emptySeq())
	if !errors.Is(err, spi.ErrStaleClaim) {
		t.Fatalf("SaveResults(empty sequence, stale epoch) = %v, want ErrStaleClaim", err)
	}
}

// TestPGSearchStore_SaveResults_EmptySequence_FencesOnTerminal is the terminal
// half of the same contract: a job that has already reached a write-once
// terminal status must refuse an empty SaveResults with ErrAlreadyTerminal,
// not silently succeed.
func TestPGSearchStore_SaveResults_EmptySequence_FencesOnTerminal(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "empty-terminal-tenant")
	ctx := ctxWithTenant("empty-terminal-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, status := range []string{"SUCCESSFUL", "FAILED", "CANCELLED"} {
		jobID := "job-empty-" + status
		job := newRunningJob(jobID, "empty-terminal-tenant", now)
		job.Status = status
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob(%s): %v", status, err)
		}

		err := store.SaveResults(ctx, jobID, 1, emptySeq())
		if !errors.Is(err, spi.ErrAlreadyTerminal) {
			t.Errorf("SaveResults(empty sequence, %s) = %v, want ErrAlreadyTerminal", status, err)
		}
	}
}

// TestPGSearchStore_SaveResults_EmptySequence_MissingJob completes the
// probeFenced classification triple for the empty-sequence path: no row at all
// is ErrNotFound, exactly as it is for a non-empty one.
func TestPGSearchStore_SaveResults_EmptySequence_MissingJob(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "empty-missing-tenant")
	ctx := ctxWithTenant("empty-missing-tenant")

	err := store.SaveResults(ctx, "no-such-job", 1, emptySeq())
	if !errors.Is(err, spi.ErrNotFound) {
		t.Fatalf("SaveResults(empty sequence, missing job) = %v, want ErrNotFound", err)
	}
}

// TestPGSearchStore_SaveResults_EmptySequence_LiveClaimSucceeds guards the
// other side of the fix: an empty result set against a live claim at the right
// epoch is a perfectly ordinary success, and must stay one.
func TestPGSearchStore_SaveResults_EmptySequence_LiveClaimSucceeds(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "empty-ok-tenant")
	ctx := ctxWithTenant("empty-ok-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.CreateJob(ctx, newRunningJob("job-empty-ok", "empty-ok-tenant", now)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := store.SaveResults(ctx, "job-empty-ok", 1, emptySeq()); err != nil {
		t.Fatalf("SaveResults(empty sequence, live claim) = %v, want nil", err)
	}
	ids, total, err := store.GetResultIDs(ctx, "job-empty-ok", 0, 10)
	if err != nil {
		t.Fatalf("GetResultIDs: %v", err)
	}
	if total != 0 || len(ids) != 0 {
		t.Fatalf("GetResultIDs after empty save: total=%d ids=%v, want 0/[]", total, ids)
	}
}

func epochOfFirst(jobs []*spi.SearchJob) any {
	if len(jobs) == 0 {
		return nil
	}
	return jobs[0].Epoch
}

// ---------------------------------------------------------------------------
// Cancellation must not churn pooled connections
// ---------------------------------------------------------------------------

// TestPGSearchStore_SaveResults_CancelledMidChunk_DoesNotChurnPool pins the
// rollback-context contract on SaveResults' per-chunk transaction.
//
// CancelAsync is a first-class feature, so cancelling mid-stream is the normal
// path, not an edge case: the context SaveResults runs under is precisely the
// thing that gets cancelled. When the cancel lands BETWEEN two of the chunk
// transaction's statements, pgx leaves the connection healthy and the
// transaction open on the server (the next statement early-returns with
// "context already done" without touching the wire). The deferred rollback is
// then the only thing that can close that transaction — and issued on the same
// cancelled context it, too, early-returns without sending ROLLBACK. The
// connection goes back to pgxpool with a non-idle transaction status, so
// Release DESTROYS it, and every submit-then-cancel cycle burns one of the
// pool's connections plus a fresh TCP+auth handshake to replace it. searcher.go
// and grouped_stats.go already roll back on context.WithoutCancel(ctx) for
// exactly this reason.
//
// The interleaving is forced with a pgx QueryTracer rather than a sleep: the
// tracer fires the cancel the instant probeFenced's `SELECT ... FOR UPDATE`
// completes, which is a point where pgx holds no in-flight statement. That
// makes "the cancel lands between statements, with the transaction open and the
// connection healthy" a fact of the test's construction, not a timing gamble.
//
// (A cancel that lands *inside* a statement is a different story: pgx tears the
// connection down at the driver layer regardless of what the rollback does, so
// no rollback context can save it. That shape is therefore not what this test
// measures.)
func TestPGSearchStore_SaveResults_CancelledMidChunk_DoesNotChurnPool(t *testing.T) {
	tracer := &cancelAfterFenceTracer{}
	factory, pool := setupSearchTestWithTracer(t, tracer)
	ctxTenant := ctxWithTenant("cancel-pool-tenant")
	store, err := factory.AsyncSearchStore(ctxTenant)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.CreateJob(ctxTenant, newRunningJob("job-cancel-pool", "cancel-pool-tenant", now)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Warm the pool so the baseline reflects a steady state rather than the
	// lazily-opened first connections.
	if err := store.SaveResults(ctxTenant, "job-cancel-pool", 1, slices.Values([]string{"warm"})); err != nil {
		t.Fatalf("warm-up SaveResults: %v", err)
	}

	const cycles = 8
	before := pool.Stat().NewConnsCount()

	for i := 0; i < cycles; i++ {
		ctx, cancel := context.WithCancel(ctxTenant)
		tracer.arm(cancel)
		err := store.SaveResults(ctx, "job-cancel-pool", 1, slices.Values([]string{"e1", "e2"}))
		tracer.disarm()
		cancel()
		// The error itself is not the contract under test — a cancelled write
		// legitimately fails — only that the connection it borrowed comes back.
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cycle %d: SaveResults = %v, want context.Canceled (the tracer cancels between the "+
				"fence and the CopyFrom, so the chunk must abort there)", i, err)
		}
	}

	opened := pool.Stat().NewConnsCount() - before
	// One replacement connection is slack for anything the pool does on its
	// own. Per-cycle churn is what this test rejects: the pre-fix shape
	// destroys the chunk connection on every cancelled cycle, so `opened`
	// tracks `cycles`.
	if opened > 1 {
		t.Fatalf("cancelled SaveResults churned pooled connections: %d new connections opened across %d "+
			"cancelled cycles, want <= 1 (the chunk rollback must run on context.WithoutCancel so the "+
			"connection returns to the pool instead of being destroyed)", opened, cycles)
	}
}

// fencedProbeKey marks the trace context of SaveResults' own fence statement,
// so TraceQueryEnd can tell it apart from every other statement on the pool.
type fencedProbeKey struct{}

// cancelAfterFenceTracer cancels an armed context the moment SaveResults'
// fenced `SELECT ... FOR UPDATE` on search_jobs completes — the seam between
// two statements of the chunk transaction.
type cancelAfterFenceTracer struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (tr *cancelAfterFenceTracer) arm(cancel context.CancelFunc) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.cancel = cancel
}

func (tr *cancelAfterFenceTracer) disarm() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.cancel = nil
}

func (tr *cancelAfterFenceTracer) fire() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.cancel != nil {
		tr.cancel()
		tr.cancel = nil
	}
}

func (tr *cancelAfterFenceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM search_jobs") && strings.Contains(data.SQL, "FOR UPDATE") {
		return context.WithValue(ctx, fencedProbeKey{}, true)
	}
	return ctx
}

func (tr *cancelAfterFenceTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(fencedProbeKey{}) != nil {
		tr.fire()
	}
}

// setupSearchTestWithTracer is setupSearchTest over a pool whose connections
// carry the given pgx tracer, so a test can observe — and act at — statement
// boundaries inside the plugin's own transactions.
func setupSearchTestWithTracer(t *testing.T, tracer pgx.QueryTracer) (*postgres.StoreFactory, *pgxpool.Pool) {
	t.Helper()
	dbURL := skipIfNoPostgres(t)

	poolCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 0
	poolCfg.MaxConnIdleTime = 60 * time.Second
	poolCfg.HealthCheckPeriod = 24 * time.Hour
	poolCfg.ConnConfig.Tracer = tracer

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.DropSchemaForTest(pool); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := postgres.Migrate(pool); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	t.Cleanup(func() { _ = postgres.DropSchemaForTest(pool) })
	return postgres.NewStoreFactory(pool), pool
}

// ---------------------------------------------------------------------------
// GetResultIDs total and page must describe the same state
// ---------------------------------------------------------------------------

// TestPGSearchStore_GetResultIDs_TotalMatchesPage pins the read-consistency
// contract the SPI godoc's "reading a non-terminal job answers with the results
// saved so far" makes reachable: total and the returned page must describe ONE
// state of the result set.
//
// Before the fix the count and the page were two unsynchronised statements on
// the pool, so a SaveResults chunk landing between them produced a total that
// did not describe the page — reported as "N of M" to a caller paging a live
// job. The memory plugin takes both under one RLock. Here a writer appends
// concurrently while a reader loops; every observation must be internally
// consistent (total >= len(page), and a full page read from offset 0 must never
// be shorter than a total that claims more).
func TestPGSearchStore_GetResultIDs_TotalMatchesPage(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "consistent-tenant")
	ctx := ctxWithTenant("consistent-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.CreateJob(ctx, newRunningJob("job-consistent", "consistent-tenant", now)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Writer: append result rows to the live job while the reader pages it,
	// standing in for a SaveResults chunk landing mid-read. The rows go in
	// directly rather than through repeated SaveResults calls because
	// spi.AsyncSearchStore permits exactly one SaveResults call per claim
	// epoch (its seq counter restarts at 0 on every call); what this test
	// needs is the append itself, at an arbitrary instant.
	pool := factory.Pool()
	done := make(chan struct{})
	var writerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for seq := 0; seq < 200; seq++ {
			if _, err := pool.Exec(context.Background(),
				`INSERT INTO search_job_results (job_id, tenant_id, seq, entity_id) VALUES ($1, $2, $3, $4)`,
				"job-consistent", "consistent-tenant", seq, "e"+strconv.Itoa(seq)); err != nil {
				writerErr = err
				return
			}
		}
	}()

	for {
		select {
		case <-done:
			wg.Wait()
			if writerErr != nil {
				t.Fatalf("concurrent result append: %v", writerErr)
			}
			return
		default:
		}

		ids, total, err := store.GetResultIDs(ctx, "job-consistent", 0, 10000)
		if err != nil {
			t.Fatalf("GetResultIDs: %v", err)
		}
		// The page is unbounded relative to any total the writer can reach in
		// this test, so a consistent snapshot always has len(ids) == total.
		// A total read separately from the page does not.
		if len(ids) != total {
			t.Fatalf("GetResultIDs returned total=%d but a page of %d ids — total and page must "+
				"describe one state of the result set (read them in a single statement or one snapshot)",
				total, len(ids))
		}
	}
}

// ---------------------------------------------------------------------------
// ClaimStale's empty return shape
// ---------------------------------------------------------------------------

// TestPGSearchStore_ClaimStale_EmptyReturnsNil pins the empty-result shape on
// the value the memory and sqlite plugins already return (a nil slice; neither
// normalises to an empty one, and spi.AsyncSearchStore.ClaimStale's godoc
// specifies neither). A backend differing from the others on the same contract
// is a defect, however cosmetic — len() callers cannot tell them apart, but a
// reflect.DeepEqual or a JSON round-trip can.
func TestPGSearchStore_ClaimStale_EmptyReturnsNil(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "claim-empty-tenant")
	ctx := ctxWithTenant("claim-empty-tenant")

	now := time.Now().UTC().Truncate(time.Microsecond)
	// A fresh job: RUNNING, but nowhere near stale.
	if err := store.CreateJob(ctx, newRunningJob("job-fresh", "claim-empty-tenant", now)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	claimed, err := store.ClaimStale(context.Background(), time.Hour, 10)
	if err != nil {
		t.Fatalf("ClaimStale: %v", err)
	}
	if claimed != nil {
		t.Fatalf("ClaimStale with no stale jobs = %#v, want nil (memory and sqlite both return nil)", claimed)
	}
}

// TestPGSearchStore_GetResultIDs_PaginationCheckedBeforeTenantResolution pins
// the order the three backends share: offset/limit are pure argument
// validation, decidable without touching any state, so they are rejected before
// the tenant is resolved. memory checked the tenant first, so a request with
// both inputs bad reported a different error there than here.
func TestPGSearchStore_GetResultIDs_PaginationCheckedBeforeTenantResolution(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "validation-order-tenant")

	// No user context at all AND invalid pagination.
	_, _, err := store.GetResultIDs(context.Background(), "job-x", -1, 0)
	if err == nil {
		t.Fatal("GetResultIDs with no user context and invalid pagination returned no error")
	}
	if !strings.Contains(err.Error(), "offset") && !strings.Contains(err.Error(), "limit") {
		t.Errorf("GetResultIDs reported %q, want the pagination error", err)
	}
}

// TestPGSearchStore_ClaimStale_RejectsNonPositiveLimit: "up to limit jobs" has
// no meaning below 1, and the three backends disagreed about what it meant —
// memory silently claimed nothing, postgres raised a raw driver error
// ("LIMIT must not be negative"), and sqlite passed the value straight into
// `LIMIT ?`, where SQLite defines LIMIT -1 as UNBOUNDED. All three now reject
// it up front, matching GetResultIDs' documented "limit >= 1; a violation
// returns an error".
func TestPGSearchStore_ClaimStale_RejectsNonPositiveLimit(t *testing.T) {
	factory := setupSearchTest(t)
	store := getSearchStore(t, factory, "claim-limit-tenant")
	ctx := ctxWithTenant("claim-limit-tenant")

	stale := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := store.CreateJob(ctx, newRunningJob("job-live", "claim-limit-tenant", stale)); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	for _, limit := range []int{0, -1} {
		claimed, err := store.ClaimStale(context.Background(), time.Minute, limit)
		if err == nil {
			t.Errorf("ClaimStale(limit=%d) returned %d jobs and no error, want an error", limit, len(claimed))
		}
		if len(claimed) != 0 {
			t.Errorf("ClaimStale(limit=%d) claimed %d jobs, want none", limit, len(claimed))
		}
	}

	job, err := store.GetJob(ctx, "job-live")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Epoch != 1 {
		t.Errorf("a rejected ClaimStale bumped the epoch to %d, want 1", job.Epoch)
	}
	if job.HeartbeatTime != nil {
		t.Error("a rejected ClaimStale stamped HeartbeatTime")
	}
}
