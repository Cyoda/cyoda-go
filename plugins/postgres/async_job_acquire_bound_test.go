package postgres_test

// async_job_acquire_bound_test.go — the connection-acquire bound on the async
// search job record, which is hold-and-wait for the same reason a point-in-time
// read is (pit_acquire_bound_test.go), through a different door.
//
// The job record must NOT join the transaction the submitter may be holding: the
// goroutine that fills it in runs on a context of its own, and a job bound to
// the caller's transaction would be invisible to that goroutine and gone if the
// caller rolled back. So the job store pins the pool — and inside a transaction
// that means the request holds TWO connections at once, its transaction's and
// this one. An unbounded acquire there does not fail, it BLOCKS, until the
// caller's own context expires.
//
// The submit path really can be inside a transaction: the TxJoin middleware
// wraps the whole API mux (so POST /search/async/... is covered) and the gRPC
// tx-route interceptor covers EntitySearch, which carries the snapshot submit.
// The status, results and cancel endpoints are request-scoped in the same way,
// so every job-record statement reachable from a request context is in scope —
// which is all three Querier methods.
//
// Three properties:
//
//  1. In a transaction, on a saturated pool, the job statement fails fast and
//     carries the storage-unavailable marker — the same answer every other
//     acquire in this plugin gives for the same condition.
//  2. Outside a transaction nothing changes: no second connection is held, so
//     it is not hold-and-wait, and bounding it would turn ordinary pool
//     contention into spurious failures.
//  3. The record still does not join the caller's transaction — the property
//     the pool pinning exists for, which the bound must not trade away.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

const asyncJobAcquireTenant = spi.TenantID("async-job-acquire-tenant")

// newAsyncJobFixture builds a pool with exactly maxConns connections and a
// factory and TransactionManager that share one acquire deadline. maxConns is
// the whole point: connection scarcity has to be reachable deterministically,
// not raced for.
func newAsyncJobFixture(t *testing.T, maxConns int32, acquire time.Duration) (*postgres.StoreFactory, *postgres.TransactionManager, context.Context) {
	t.Helper()
	tm, pool := newTestTxManager(t, withMaxConns(maxConns), withAcquireTimeout(acquire))
	factory := postgres.NewStoreFactoryWithTMAndAcquireTimeoutForTest(pool, tm, acquire)
	return factory, tm, ctxWithTenant(asyncJobAcquireTenant)
}

func newAsyncJobRecord(id string) *spi.SearchJob {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &spi.SearchJob{
		ID:         id,
		TenantID:   asyncJobAcquireTenant,
		Status:     "RUNNING",
		ModelRef:   spi.ModelRef{EntityName: "Widget", ModelVersion: "1"},
		Condition:  json.RawMessage(`{}`),
		SearchOpts: json.RawMessage(`{}`),
		CreateTime: now,
	}
}

// TestAsyncJobAcquire_InTxOnSaturatedPool_FailsFastAsStorageUnavailable covers
// one job statement per Querier method (Exec, QueryRow, Query), because they all
// take the same second connection and a caller must be told the same thing
// whichever one it reached.
//
// What would silently disarm this test:
//
//   - A pool that is not actually saturated (maxConns > 1, or the holding
//     transaction released early) — the `err == nil` fatal catches that: every
//     one of these statements succeeds against a free connection.
//   - A failure raised BEFORE the pool is ever touched (a missing tenant on the
//     context, an argument the store rejects up front) would fail fast for a
//     reason that has nothing to do with the bound. The `elapsed >= acquire`
//     lower bound is what rules that out: only a real wait on a real acquire
//     deadline can take that long.
//
// The pgx "small result set is served from client memory" trap that constrains
// the point-in-time streaming test does not apply here: nothing in this file
// iterates a result set across a pause. Every statement is a single round trip,
// and the bound under test is on GETTING the connection, not on using it.
//
// The caller's own deadline is deliberately generous: an unbounded acquire does
// not fail, it BLOCKS, so the regression signature is "elapsed ≈ the caller's
// whole budget, and the error is the caller's own context expiring rather than
// the storage-unavailable marker". Both are asserted.
func TestAsyncJobAcquire_InTxOnSaturatedPool_FailsFastAsStorageUnavailable(t *testing.T) {
	const acquire = 300 * time.Millisecond
	const callerBudget = 8 * time.Second

	factory, tm, ctx := newAsyncJobFixture(t, 1, acquire)
	holdCtx := beginHoldingTx(t, tm, ctx)

	store, err := factory.AsyncSearchStore(holdCtx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}

	calls := map[string]func(context.Context) error{
		// Exec — the submit path itself.
		"CreateJob": func(c context.Context) error {
			return store.CreateJob(c, newAsyncJobRecord("async-acquire-create"))
		},
		// QueryRow — the status endpoint.
		"GetJob": func(c context.Context) error {
			_, err := store.GetJob(c, "async-acquire-get")
			return err
		},
		// Exec — the terminal write.
		"UpdateJobStatus": func(c context.Context) error {
			return store.UpdateJobStatus(c, "async-acquire-update", 1, "SUCCESSFUL", 0, "", time.Now().UTC(), 1)
		},
		// Query — the results endpoint.
		"GetResultIDs": func(c context.Context) error {
			_, _, err := store.GetResultIDs(c, "async-acquire-results", 0, 10)
			return err
		},
		// Exec then QueryRow — the cancel endpoint.
		"Cancel": func(c context.Context) error {
			return store.Cancel(c, "async-acquire-cancel", time.Now().UTC())
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			callerCtx, cancel := context.WithTimeout(holdCtx, callerBudget)
			defer cancel()

			start := time.Now()
			err := call(callerCtx)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("%s succeeded on a one-connection pool whose only connection the caller's own "+
					"transaction is holding", name)
			}
			if elapsed < acquire {
				t.Fatalf("%s failed after %v, before the %v acquire deadline had even elapsed: %v — it did "+
					"not fail on the pool, so this test proves nothing about the bound", name, elapsed, acquire, err)
			}
			if !storageUnavailable(err) {
				t.Fatalf("%s: pool exhaustion not marked storage-unavailable after %v: %v", name, elapsed, err)
			}
			if elapsed > callerBudget/2 {
				t.Fatalf("%s waited %v of an %v caller budget; an in-transaction job-record statement must "+
					"fail on its own acquire deadline rather than block until the caller gives up",
					name, elapsed, callerBudget)
			}
			t.Logf("%s reported storage-unavailable after %v (acquire deadline %v)", name, elapsed, acquire)
		})
	}
}

// TestAsyncJobAcquire_OutsideTx_NotBoundedByTheAcquireDeadline guards the other
// direction. A job statement issued outside a transaction — the reaper, the
// heartbeat, the job goroutine's own terminal write, all of which run on
// detached contexts — holds no other connection, so it is not hold-and-wait and
// there is nothing to break the deadlock on. Bounding it would convert ordinary
// pool contention on a busy server into spurious retryable failures.
//
// The only connection is held for five times the acquire deadline by a
// transaction belonging to someone else, then released. The write must wait it
// out and succeed.
func TestAsyncJobAcquire_OutsideTx_NotBoundedByTheAcquireDeadline(t *testing.T) {
	const acquire = 200 * time.Millisecond
	const hold = 5 * acquire

	factory, tm, ctx := newAsyncJobFixture(t, 1, acquire)

	holdID, holdCtx := beginGuarded(t, tm, ctx)
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(hold)
		_ = tm.Rollback(holdCtx, holdID)
	}()
	// Registered after the fixture's schema-drop cleanup, so it runs first: the
	// drop acquires from this same pool.
	t.Cleanup(func() { <-released })

	// ctx carries NO transaction — this is the ordinary detached write.
	store, err := factory.AsyncSearchStore(ctx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	callerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()
	err = store.CreateJob(callerCtx, newAsyncJobRecord("async-acquire-unbounded"))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("non-tx CreateJob failed after %v on a pool whose connection frees at %v: %v — a statement "+
			"that holds no other connection must not carry the acquire bound", elapsed, hold, err)
	}
	if elapsed < acquire {
		t.Fatalf("non-tx CreateJob returned after %v, before the %v acquire deadline had even elapsed; the "+
			"fixture did not actually make it wait, so it proves nothing", elapsed, acquire)
	}
}

// TestAsyncJob_CreatedInTx_SurvivesCallerRollback pins the property the pool
// pinning exists for, at the layer the bound changed. The record must outlive
// the submitting transaction: the goroutine that fills it in runs on a detached
// context and would never see a row bound to that transaction, and a rollback
// would take the job with it.
//
// internal/e2e/async_search_job_tx_independence_test.go pins the same property
// end to end through both doors. This one is the plugin-level guard, and it is
// here because the querier behind these statements is exactly what the acquire
// bound rewrites — a bound implemented by resolving the caller's transaction
// instead of taking a connection of its own would satisfy every timing
// assertion above and break only this.
//
// Two connections: one for the caller's transaction, one for the job record.
func TestAsyncJob_CreatedInTx_SurvivesCallerRollback(t *testing.T) {
	factory, tm, ctx := newAsyncJobFixture(t, 2, 5*time.Second)

	txID, txCtx := beginGuarded(t, tm, ctx)

	store, err := factory.AsyncSearchStore(txCtx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	const jobID = "async-job-survives-rollback"
	if err := store.CreateJob(txCtx, newAsyncJobRecord(jobID)); err != nil {
		t.Fatalf("CreateJob inside a transaction: %v", err)
	}

	if err := tm.Rollback(ctx, txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	got, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatalf("GetJob after the submitting transaction rolled back: %v — the job record must not join "+
			"that transaction", err)
	}
	if got.ID != jobID {
		t.Errorf("GetJob returned %q, want %q", got.ID, jobID)
	}
}
