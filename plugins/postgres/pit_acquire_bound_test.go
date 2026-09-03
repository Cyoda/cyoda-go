package postgres_test

// pit_acquire_bound_test.go — the connection-acquire bound on the ONE path that
// is hold-and-wait: a point-in-time read issued from inside a transaction.
//
// A point-in-time read is committed-only, so it runs off the caller's
// transaction on a pooled connection of its own (see committedQuerier in
// search_base.go). Inside a transaction that means the request holds TWO
// connections at once: the transaction's, and the snapshot read's. That is
// hold-and-wait, and on a saturated pool an unbounded acquire turns it into a
// stall that only ends when the caller's own context expires.
//
// Three properties, and the second is the one that constrains the design:
//
//  1. In a transaction, on a saturated pool, the read fails fast and carries the
//     storage-unavailable marker — the same answer every other acquire in this
//     plugin gives for the same condition.
//  2. The bound is on GETTING the connection, not on USING it. A streaming
//     Iterate that already holds its connection must run for as long as it
//     needs; a deadline covering the whole operation would kill it mid-scan.
//  3. Outside a transaction nothing changes. That read holds no other
//     connection, so it is not hold-and-wait, and bounding it would turn
//     ordinary pool contention into spurious failures.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

var pitAcquireModel = spi.ModelRef{EntityName: "pitacquire", ModelVersion: "1"}

const pitAcquireTenant = spi.TenantID("pit-acquire-tenant")

// pitAcquireID is the id of the nth seeded entity.
func pitAcquireID(n int) string { return fmt.Sprintf("pit-acquire-%d", n) }

// newPITAcquireFixture builds a pool with exactly maxConns connections, a
// TransactionManager and a factory that share one acquire deadline, and seeds
// `seed` committed entities before any connection is held.
//
// maxConns is the whole point of the fixture: connection scarcity has to be
// reachable deterministically, not raced for.
//
// padBytes inflates each seeded document. It matters only to the streaming test,
// and it matters a lot: a handful of small rows arrive in the client's very
// first read and are then served from memory, so an iteration that pauses can
// finish without ever touching the socket again — and would pass no matter what
// context the query ran under. Padding pushes the result set past anything the
// client or the kernel will hold, so resuming after the pause forces a real read
// on the connection. Zero means no padding.
func newPITAcquireFixture(t *testing.T, maxConns int32, acquire time.Duration, seed, padBytes int) (*postgres.StoreFactory, *postgres.TransactionManager, context.Context) {
	t.Helper()
	tm, pool := newTestTxManager(t, withMaxConns(maxConns), withAcquireTimeout(acquire))
	factory := postgres.NewStoreFactoryWithTMAndAcquireTimeoutForTest(pool, tm, acquire)
	ctx := ctxWithTenant(pitAcquireTenant)

	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore (seed): %v", err)
	}
	doc := []byte(`{"v":"committed"}`)
	if padBytes > 0 {
		doc = []byte(`{"v":"committed","pad":"` + strings.Repeat("x", padBytes) + `"}`)
	}
	for i := 0; i < seed; i++ {
		if _, err := store.Save(ctx, &spi.Entity{
			Meta: spi.EntityMeta{ID: pitAcquireID(i), ModelRef: pitAcquireModel, State: "NEW"},
			Data: doc,
		}); err != nil {
			t.Fatalf("seed Save %d: %v", i, err)
		}
	}
	return factory, tm, ctx
}

// beginHoldingTx begins a transaction — which checks a connection out of the
// pool and keeps it until commit — and returns its context. That held connection
// is what makes a point-in-time read from the same context hold-and-wait.
//
// The rollback is registered as a cleanup rather than deferred so it runs BEFORE
// the fixture's schema drop, which acquires from this same pool and would
// otherwise block forever.
func beginHoldingTx(t *testing.T, tm *postgres.TransactionManager, ctx context.Context) context.Context {
	t.Helper()
	holdID, holdCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("hold begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(holdCtx, holdID) })
	return holdCtx
}

// TestPITAcquire_InTxOnSaturatedPool_FailsFastAsStorageUnavailable covers every
// point-in-time entry point, because they all take the same second connection
// and a caller must be told the same thing whichever one it reached.
//
// The caller's own deadline is deliberately generous: an unbounded acquire does
// not fail, it BLOCKS, so the regression signature is "elapsed ≈ the caller's
// whole budget, and the error is the caller's own context expiring rather than
// the storage-unavailable marker". Both are asserted.
func TestPITAcquire_InTxOnSaturatedPool_FailsFastAsStorageUnavailable(t *testing.T) {
	const acquire = 300 * time.Millisecond
	const callerBudget = 8 * time.Second

	factory, tm, ctx := newPITAcquireFixture(t, 1, acquire, 1, 0)
	holdCtx := beginHoldingTx(t, tm, ctx)

	txStore, err := factory.EntityStore(holdCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}
	asAt := pitFuture()

	reads := map[string]func(context.Context) error{
		"GetAsAt": func(c context.Context) error {
			_, err := txStore.GetAsAt(c, pitAcquireID(0), asAt)
			return err
		},
		"GetPage(asAt)": func(c context.Context) error {
			_, err := txStore.GetPage(c, pitAcquireModel, 100, 0, &asAt)
			return err
		},
		"Search(PointInTime)": func(c context.Context) error {
			_, err := txStore.Search(c, spi.Filter{}, spi.SearchOptions{
				ModelName:    pitAcquireModel.EntityName,
				ModelVersion: pitAcquireModel.ModelVersion,
				Limit:        100,
				PointInTime:  &asAt,
			})
			return err
		},
		"Iterate(PointInTime)": func(c context.Context) error {
			it, err := txStore.Iterate(c, pitAcquireModel, spi.Filter{}, spi.IterateOptions{PointInTime: &asAt})
			if err != nil {
				return err
			}
			defer it.Close()
			for it.Next() {
			}
			return it.Err()
		},
	}

	for name, read := range reads {
		t.Run(name, func(t *testing.T) {
			callerCtx, cancel := context.WithTimeout(holdCtx, callerBudget)
			defer cancel()

			start := time.Now()
			err := read(callerCtx)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatalf("%s succeeded on a one-connection pool whose only connection the caller's own "+
					"transaction is holding", name)
			}
			if !storageUnavailable(err) {
				t.Fatalf("%s: pool exhaustion not marked storage-unavailable after %v: %v", name, elapsed, err)
			}
			if elapsed > callerBudget/2 {
				t.Fatalf("%s waited %v of an %v caller budget; an in-transaction point-in-time read must "+
					"fail on its own acquire deadline rather than block until the caller gives up",
					name, elapsed, callerBudget)
			}
			t.Logf("%s reported storage-unavailable after %v (acquire deadline %v)", name, elapsed, acquire)
		})
	}
}

// TestPITAcquire_InTxIterate_SurvivesPastTheAcquireDeadline is the test that
// pins acquire-not-use. The iterator already holds its connection; the acquire
// window is long closed. An implementation that bounded the whole operation —
// by handing the deadline context to the query instead of to the acquire — kills
// this scan the moment the window shuts, and no ordinary test would notice
// because ordinary tests finish in milliseconds. Hence the sleep: it IS the
// test.
//
// The result set is deliberately far too large to sit in the client's buffers.
// pgx enforces the query context by putting a deadline on the socket, so a small
// result — already read, already in memory — is served to the end no matter what
// context it was fetched under, and a small-result version of this test passes
// against the very implementation it exists to reject. Iteration has to still be
// reading from the connection when it resumes, so the padding is load-bearing.
//
// Two connections: one for the caller's transaction, one for the committed-only
// read it issues.
func TestPITAcquire_InTxIterate_SurvivesPastTheAcquireDeadline(t *testing.T) {
	const acquire = 200 * time.Millisecond
	const seeded = 400
	const padBytes = 4096 // ~1.6MB of result set

	factory, tm, ctx := newPITAcquireFixture(t, 2, acquire, seeded, padBytes)
	holdCtx := beginHoldingTx(t, tm, ctx)

	txStore, err := factory.EntityStore(holdCtx)
	if err != nil {
		t.Fatalf("EntityStore (tx): %v", err)
	}

	asAt := pitFuture()
	it, err := txStore.Iterate(holdCtx, pitAcquireModel, spi.Filter{}, spi.IterateOptions{PointInTime: &asAt})
	if err != nil {
		t.Fatalf("Iterate: %v", err)
	}
	defer it.Close()

	if !it.Next() {
		t.Fatalf("Iterate yielded no rows (err=%v); the scan must reach the seeded entities", it.Err())
	}
	got := []string{it.Entity().Meta.ID}

	// Idle mid-iteration, well past the acquire deadline, with the connection
	// already in hand.
	const idle = 5 * acquire
	time.Sleep(idle)

	for it.Next() {
		got = append(got, it.Entity().Meta.ID)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iteration died %v into the scan on a %v acquire deadline: %v — the deadline must bound "+
			"getting the connection, not using it", idle, acquire, err)
	}
	if len(got) != seeded {
		t.Fatalf("iterated %d entities, want %d — the scan must resume where it paused and run to the end",
			len(got), seeded)
	}
}

// TestPITAcquire_OutsideTx_NotBoundedByTheAcquireDeadline guards the other
// direction. A point-in-time read outside a transaction holds no other
// connection, so it is not hold-and-wait and there is nothing to break the
// deadlock on: bounding it would convert ordinary pool contention — a busy
// server — into spurious retryable failures.
//
// The only connection is held for five times the acquire deadline by a
// transaction belonging to someone else, then released. The read must wait it
// out and succeed.
func TestPITAcquire_OutsideTx_NotBoundedByTheAcquireDeadline(t *testing.T) {
	const acquire = 200 * time.Millisecond
	const hold = 5 * acquire

	factory, tm, ctx := newPITAcquireFixture(t, 1, acquire, 1, 0)

	holdID, holdCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("hold begin: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(hold)
		_ = tm.Rollback(holdCtx, holdID)
	}()
	// Registered after the fixture's schema-drop cleanup, so it runs first: the
	// drop acquires from this same pool.
	t.Cleanup(func() { <-released })

	// ctx carries NO transaction — this is the ordinary non-transactional read.
	store, err := factory.EntityStore(ctx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	callerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()
	got, err := store.GetAsAt(callerCtx, pitAcquireID(0), pitFuture())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("non-tx point-in-time read failed after %v on a pool whose connection frees at %v: %v — "+
			"a read that holds no other connection must not carry the acquire bound", elapsed, hold, err)
	}
	if elapsed < acquire {
		t.Fatalf("non-tx read returned after %v, before the %v acquire deadline had even elapsed; the "+
			"fixture did not actually make it wait, so it proves nothing", elapsed, acquire)
	}
	if string(got.Data) != `{"v":"committed"}` {
		t.Errorf("non-tx GetAsAt = %s, want {\"v\":\"committed\"}", got.Data)
	}
}
