package postgres_test

// self_wrap_acquire_test.go — the two acquires that are NOT
// TransactionManager.Begin.
//
// Three code paths in this plugin open a transaction of their own on the pool:
// Begin, ExtendSchema's self-wrap, and the async-search scan's own-ceiling
// transaction. All three contend for the same connections, so all three can
// fail for the same transient reason — and a client must be told the same thing
// about it whichever one it reached. Begin has been tested since the ceiling
// work landed (acquire_test.go); these are the other two.
//
// Each is tested in both directions, because the interesting half is the one a
// classifier keyed on context.DeadlineExceeded alone would get wrong: a caller
// who gave up first is not a server-side outage.

import (
	"context"
	"errors"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/plugins/postgres"
)

// storageUnavailable reports whether err carries the marker the application
// layer turns into a retryable 503. Matched on the interface, exactly as
// common.StorageUnavailable does.
func storageUnavailable(err error) bool {
	var su interface{ StorageUnavailable() bool }
	return errors.As(err, &su) && su.StorageUnavailable()
}

// newSaturatedFactory returns a factory on a one-connection pool with a short
// acquire deadline, plus the transaction holding that single connection. The
// caller gets a pool from which no further connection can be acquired.
func newSaturatedFactory(t *testing.T, acquire time.Duration) (*postgres.StoreFactory, context.Context) {
	t.Helper()
	tm, pool := newTestTxManager(t, withMaxConns(1), withAcquireTimeout(acquire))
	ctx := ctxWithTenant("self-wrap-tenant")

	holdID, holdCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("hold begin: %v", err)
	}
	// Hand the only connection back before the fixture's schema cleanup runs;
	// that cleanup acquires from this same pool and would otherwise block.
	t.Cleanup(func() { _ = tm.Rollback(holdCtx, holdID) })

	return postgres.NewStoreFactoryWithAcquireTimeoutForTest(pool, acquire), ctx
}

// --- ExtendSchema's self-wrap ------------------------------------------------

// extendSchemaOnSaturatedPool drives ExtendSchema with NO ambient transaction on
// ctx, which is the branch that self-wraps a pgx.Tx off the pool. The delta is
// never applied — the acquire fails first — so no schema state is needed.
func extendSchemaOnSaturatedPool(t *testing.T, callerCtx context.Context, f *postgres.StoreFactory) error {
	t.Helper()
	ms, err := f.ModelStore(callerCtx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	if spi.GetTransaction(callerCtx) != nil {
		t.Fatal("caller ctx carries a transaction; ExtendSchema would not self-wrap")
	}
	return ms.ExtendSchema(callerCtx, spi.ModelRef{EntityName: "Widget", ModelVersion: "1"},
		spi.SchemaDelta(`{"op":"add","path":"/properties/x"}`))
}

// A schema extension that cannot get a connection is transient contention on the
// shared pool, exactly like the sibling acquire in Begin. Telling the client
// "server error, here is a ticket" for a condition the next attempt would likely
// survive is the wrong answer, and it differs from what the same saturation
// reports through every other door.
func TestExtendSchema_PoolSaturated_ReportsStorageUnavailable(t *testing.T) {
	f, ctx := newSaturatedFactory(t, 200*time.Millisecond)

	start := time.Now()
	err := extendSchemaOnSaturatedPool(t, ctx, f)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ExtendSchema succeeded on a one-connection pool with the only connection held")
	}
	if !storageUnavailable(err) {
		t.Fatalf("acquire failure not marked storage-unavailable: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("waited %v; the acquire should fail fast rather than queue", elapsed)
	}
}

// The other direction. pool.Begin surfaces a context error both when OUR acquire
// deadline expired and when the CALLER's did; reporting the caller's own timeout
// as a retryable server condition would be a lie about who failed.
func TestExtendSchema_CallerDeadlineOnSaturatedPool_IsNotStorageUnavailable(t *testing.T) {
	// The plugin's own deadline is long; the caller's is short, so the caller's
	// is what expires even though the pool genuinely is saturated.
	f, ctx := newSaturatedFactory(t, 30*time.Second)
	callerCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err := extendSchemaOnSaturatedPool(t, callerCtx, f)
	if err == nil {
		t.Fatal("ExtendSchema succeeded on a one-connection pool with the only connection held")
	}
	if storageUnavailable(err) {
		t.Fatalf("the caller's own deadline was reported as a server-side outage: %v", err)
	}
}

// --- the async-search scan's own-ceiling transaction -------------------------

// asyncScanSearch runs a search on a context marked as an async-search scan,
// which is the branch that opens its own transaction to apply
// SET LOCAL statement_timeout. Everything else about the search is irrelevant
// here: the acquire fails before any SQL runs.
func asyncScanSearch(t *testing.T, callerCtx context.Context, f *postgres.StoreFactory) error {
	t.Helper()
	as, err := f.AsyncSearchStore(callerCtx)
	if err != nil {
		t.Fatalf("AsyncSearchStore: %v", err)
	}
	scoper, ok := as.(interface {
		AsyncScanContext(context.Context) context.Context
	})
	if !ok {
		t.Fatalf("async search store does not scope a scan context: %T", as)
	}
	es, err := f.EntityStore(callerCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	searcher, ok := es.(spi.Searcher)
	if !ok {
		t.Fatalf("entity store is not a Searcher: %T", es)
	}
	_, err = searcher.Search(scoper.AsyncScanContext(callerCtx), spi.Filter{}, spi.SearchOptions{
		ModelName: "Widget", ModelVersion: "1",
	})
	return err
}

// An async scan that cannot get a connection has failed for the same transient
// reason as any other acquire. It has no HTTP response to carry a status, but the
// job record it writes should say the storage was unavailable rather than record
// an unclassified failure an operator has to decode.
func TestAsyncScanSearch_PoolSaturated_ReportsStorageUnavailable(t *testing.T) {
	f, ctx := newSaturatedFactory(t, 200*time.Millisecond)

	err := asyncScanSearch(t, ctx, f)
	if err == nil {
		t.Fatal("async scan succeeded on a one-connection pool with the only connection held")
	}
	if !storageUnavailable(err) {
		t.Fatalf("acquire failure not marked storage-unavailable: %v", err)
	}
}

func TestAsyncScanSearch_CallerDeadlineOnSaturatedPool_IsNotStorageUnavailable(t *testing.T) {
	f, ctx := newSaturatedFactory(t, 30*time.Second)
	callerCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err := asyncScanSearch(t, callerCtx, f)
	if err == nil {
		t.Fatal("async scan succeeded on a one-connection pool with the only connection held")
	}
	if storageUnavailable(err) {
		t.Fatalf("the caller's own deadline was reported as a server-side outage: %v", err)
	}
}
