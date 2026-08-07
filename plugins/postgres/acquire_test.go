package postgres_test

// acquire_test.go — the connection-acquire deadline.
//
// Three properties, in the order they matter: the deadline bounds the acquire
// and nothing else; a saturated pool fails fast and says why; and a caller who
// gave up first is not reported as a server-side outage.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestBegin_AcquireDeadlineDoesNotLeakIntoTheTransaction is the most important
// test in this file. Begin returns a context derived from the CALLER's, which
// then carries the transaction for its whole life. If the deadline were applied
// to that input context instead of to a short-lived acquire context, every later
// operation on the transaction would inherit it and the transaction would die
// the moment the acquire window closed — which ordinary tests, all of which
// finish in milliseconds, would never notice. Hence the sleep: it is the point
// of the test, not padding.
func TestBegin_AcquireDeadlineDoesNotLeakIntoTheTransaction(t *testing.T) {
	tm, _ := newTestTxManager(t, withAcquireTimeout(200*time.Millisecond))
	ctx := ctxWithTenant("acquire-tenant")

	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	const idle = 500 * time.Millisecond // well past the acquire deadline
	time.Sleep(idle)

	if err := txCtx.Err(); err != nil {
		t.Fatalf("transaction context expired with the acquire deadline: %v", err)
	}
	start := time.Now()
	if err := tm.Commit(txCtx, txID); err != nil {
		t.Fatalf("transaction unusable %v past the acquire timeout: %v", idle, err)
	}
	t.Logf("transaction committed %v after Begin, on a 200ms acquire deadline (commit took %v)",
		idle, time.Since(start))
}

// TestBegin_PoolSaturated_ReportsStorageUnavailable — a write that cannot get a
// connection fails fast and carries the marker the application layer turns into
// a retryable 503, rather than queueing behind the saturated pool.
func TestBegin_PoolSaturated_ReportsStorageUnavailable(t *testing.T) {
	tm, _ := newTestTxManager(t, withMaxConns(1), withAcquireTimeout(200*time.Millisecond))
	ctx := ctxWithTenant("acquire-tenant")

	holdID, holdCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	// Hand the only connection back before the fixture's schema cleanup runs:
	// it acquires from this same pool and would otherwise block forever.
	defer func() { _ = tm.Rollback(holdCtx, holdID) }()

	start := time.Now()
	_, _, err = tm.Begin(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("second begin succeeded on a one-connection pool")
	}
	var su interface{ StorageUnavailable() bool }
	if !errors.As(err, &su) || !su.StorageUnavailable() {
		t.Fatalf("pool exhaustion not marked storage-unavailable: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("waited %v; the write should fail fast rather than queue", elapsed)
	}
	t.Logf("saturated pool reported storage-unavailable after %v: %v", elapsed, err)
}

// TestBegin_CallerCancelled_IsNotStorageUnavailable — pool.BeginTx surfaces a
// context error both when OUR acquire wait expired and when the CALLER's request
// context ended. Mislabelling a client timeout as a retryable server 503 is
// wrong, so the plugin must distinguish them rather than classify on the
// sentinel alone.
func TestBegin_CallerCancelled_IsNotStorageUnavailable(t *testing.T) {
	tm, _ := newTestTxManager(t)
	ctx, cancel := context.WithCancel(ctxWithTenant("acquire-tenant"))
	cancel()

	_, _, err := tm.Begin(ctx)
	if err == nil {
		t.Fatal("begin on a cancelled context succeeded")
	}
	var su interface{ StorageUnavailable() bool }
	if errors.As(err, &su) && su.StorageUnavailable() {
		t.Fatal("a client-cancelled request was reported as a retryable server condition")
	}
}

// TestBegin_CallerDeadlineOnSaturatedPool_IsNotStorageUnavailable is the case a
// classifier keyed on context.DeadlineExceeded alone would get wrong: the pool
// genuinely IS saturated, but what expired first is the caller's own deadline.
// The server is not the reason this request failed, so it must not claim to be.
func TestBegin_CallerDeadlineOnSaturatedPool_IsNotStorageUnavailable(t *testing.T) {
	// The manager's own deadline is long; the caller's is short.
	tm, _ := newTestTxManager(t, withMaxConns(1), withAcquireTimeout(30*time.Second))
	ctx := ctxWithTenant("acquire-tenant")

	holdID, holdCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("first begin: %v", err)
	}
	defer func() { _ = tm.Rollback(holdCtx, holdID) }()

	callerCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	_, _, err = tm.Begin(callerCtx)
	if err == nil {
		t.Fatal("second begin succeeded on a one-connection pool")
	}
	var su interface{ StorageUnavailable() bool }
	if errors.As(err, &su) && su.StorageUnavailable() {
		t.Fatalf("the caller's own deadline was reported as a server-side outage: %v", err)
	}
}
