package postgres_test

// self_wrap_acquire_test.go — the acquires that are NOT
// TransactionManager.Begin.
//
// Several code paths in this plugin open a transaction of their own: Begin,
// a schema extension (on the model pool), the async-search scan's own-ceiling
// transaction, and the transaction a non-transactional CompareAndSave takes its
// row lock in. Each can fail to get a connection for the same transient reason
// — and a client must be told the same thing about it whichever one it
// reached. Begin has been tested since the ceiling work landed
// (acquire_test.go); these are the others.
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

	// The guard hands the only connection back before the fixture's schema
	// cleanup runs; that cleanup acquires from this same pool and would
	// otherwise block.
	_, _ = beginGuarded(t, tm, ctx)

	return postgres.NewStoreFactoryWithAcquireTimeoutForTest(pool, acquire), ctx
}

// --- a schema extension's connection from the model pool ---------------------

// newSaturatedModelPoolFactory returns a factory with a short acquire deadline
// whose model pool has every connection held, so a schema extension gets its
// slot but no connection.
func newSaturatedModelPoolFactory(t *testing.T, acquire time.Duration) (*postgres.StoreFactory, context.Context) {
	t.Helper()
	f := postgres.NewStoreFactoryWithAcquireTimeoutForTest(newTestPool(t), acquire)
	f.SetApplyFunc(func(base []byte, _ spi.SchemaDelta) ([]byte, error) { return base, nil })
	t.Cleanup(func() { postgres.CloseModelPoolForTest(f) })

	models := postgres.ModelPoolForTest(t, f)
	for i := int32(0); i < models.Config().MaxConns; i++ {
		conn, err := models.Acquire(context.Background())
		if err != nil {
			t.Fatalf("hold model pool connection %d: %v", i, err)
		}
		t.Cleanup(conn.Release) // before the pool closes (LIFO)
	}
	return f, ctxWithTenant("self-wrap-tenant")
}

// extendSchemaOnSaturatedModelPool drives ExtendSchema against the saturated
// model pool. The delta is never applied — the acquire fails first — so no
// schema state is needed.
func extendSchemaOnSaturatedModelPool(t *testing.T, callerCtx context.Context, f *postgres.StoreFactory) error {
	t.Helper()
	ms, err := f.ModelStore(callerCtx)
	if err != nil {
		t.Fatalf("ModelStore: %v", err)
	}
	return ms.ExtendSchema(callerCtx, spi.ModelRef{EntityName: "Widget", ModelVersion: "1"},
		spi.SchemaDelta(`{"op":"add","path":"/properties/x"}`))
}

// A schema extension that cannot get a connection is transient contention on
// the model pool, exactly like the sibling acquire in Begin. Telling the client
// "server error, here is a ticket" for a condition the next attempt would likely
// survive is the wrong answer, and it differs from what the same saturation
// reports through every other door.
func TestExtendSchema_PoolSaturated_ReportsStorageUnavailable(t *testing.T) {
	f, ctx := newSaturatedModelPoolFactory(t, 200*time.Millisecond)

	start := time.Now()
	err := extendSchemaOnSaturatedModelPool(t, ctx, f)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("ExtendSchema succeeded with every model pool connection held")
	}
	if !storageUnavailable(err) {
		t.Fatalf("acquire failure not marked storage-unavailable: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("waited %v; the acquire should fail fast rather than queue", elapsed)
	}
}

// The other direction. The acquire surfaces a context error both when OUR
// acquire deadline expired and when the CALLER's did; reporting the caller's own
// timeout as a retryable server condition would be a lie about who failed.
func TestExtendSchema_CallerDeadlineOnSaturatedPool_IsNotStorageUnavailable(t *testing.T) {
	// The plugin's own deadline is long; the caller's is short, so the caller's
	// is what expires even though the pool genuinely is saturated.
	f, ctx := newSaturatedModelPoolFactory(t, 30*time.Second)
	callerCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err := extendSchemaOnSaturatedModelPool(t, callerCtx, f)
	if err == nil {
		t.Fatal("ExtendSchema succeeded with every model pool connection held")
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
	searcher := es
	_, err = searcher.Search(scoper.AsyncScanContext(callerCtx), spi.Filter{}, spi.SearchOptions{
		ModelName: "Widget", ModelVersion: "1",
		// Limit must be >= 1 (Search is bounded-or-fail) so this reaches the
		// acquire-saturation branch under test — a rejected Limit would
		// short-circuit before any pooling logic runs at all, and the value
		// itself is otherwise irrelevant here (the acquire fails before any
		// SQL, let alone a bound check on rows, runs).
		Limit: 1,
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

// --- CompareAndSave's own transaction ----------------------------------------

// compareAndSaveOnSaturatedPool drives CompareAndSave with NO ambient
// transaction on ctx, which is the branch that opens a transaction of its own
// to take the row lock. No entity is needed: the acquire fails before any SQL.
func compareAndSaveOnSaturatedPool(t *testing.T, callerCtx context.Context, f *postgres.StoreFactory) error {
	t.Helper()
	es, err := f.EntityStore(callerCtx)
	if err != nil {
		t.Fatalf("EntityStore: %v", err)
	}
	if spi.GetTransaction(callerCtx) != nil {
		t.Fatal("caller ctx carries a transaction; CompareAndSave would not open one of its own")
	}
	_, err = es.CompareAndSave(callerCtx, &spi.Entity{
		Meta: spi.EntityMeta{
			ID: "e-acquire", ModelRef: spi.ModelRef{EntityName: "Widget", ModelVersion: "1"},
			State: "open", TransactionID: "tx-writer",
		},
		Data: []byte(`{}`),
	}, "tx-expected")
	return err
}

// A compare-and-save that cannot get a connection is transient contention on
// the shared pool, exactly like every sibling acquire. Left unbounded it waits
// on the pool for as long as the caller's context allows and then reports a
// ticketed server error, for a condition the next attempt would likely survive.
func TestNonTxCompareAndSave_PoolSaturated_ReportsStorageUnavailable(t *testing.T) {
	f, ctx := newSaturatedFactory(t, 200*time.Millisecond)

	start := time.Now()
	err := compareAndSaveOnSaturatedPool(t, ctx, f)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("CompareAndSave succeeded on a one-connection pool with the only connection held")
	}
	if !storageUnavailable(err) {
		t.Fatalf("acquire failure not marked storage-unavailable: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("waited %v; the acquire should fail fast rather than queue", elapsed)
	}
}

// The other direction: the caller's own deadline is not a server-side outage.
func TestNonTxCompareAndSave_CallerDeadlineOnSaturatedPool_IsNotStorageUnavailable(t *testing.T) {
	f, ctx := newSaturatedFactory(t, 30*time.Second)
	callerCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err := compareAndSaveOnSaturatedPool(t, callerCtx, f)
	if err == nil {
		t.Fatal("CompareAndSave succeeded on a one-connection pool with the only connection held")
	}
	if storageUnavailable(err) {
		t.Fatalf("the caller's own deadline was reported as a server-side outage: %v", err)
	}
}
