package postgres

import (
	"context"
	"testing"

	spi "github.com/cyoda-platform/cyoda-go-spi"
)

// beginGuarded begins a transaction and registers a t.Cleanup that rolls it
// back unconditionally.
//
// It exists because a t.Fatal — or a failing require.* — between Begin and the
// test's own Commit/Rollback leaves the transaction active, and on this backend
// an active transaction is a pooled connection holding row locks. The fixture's
// schema-drop cleanup then queues behind those locks and never returns, so the
// run costs a whole test-binary timeout instead of failing in seconds. That
// never turns a green run red; it only destroys failure diagnosis, which is
// exactly when the suite matters most.
//
// The guard is a safety net, not test semantics: on the happy path the test
// still calls Commit or Rollback itself, and the cleanup's resulting error
// (transaction already terminated) is ignored. Callers must not rely on it to
// end a transaction they are asserting about.
//
// Registering the rollback here, rather than at each call site, is the point:
// the guard cannot be placed below the first fallible call on txCtx, which is
// where a hand-rolled defer leaves everything above it unprotected.
//
// Cleanup ordering: t.Cleanup runs LIFO, and this registration always follows
// the fixture's own (pool close, schema drop), so the rollback runs before
// them. A test that must hold the transaction past its own body — a lock-
// contention hold released by another goroutine — registers its barrier after
// this call, so LIFO runs the barrier first and the hold is never cut short.
//
// It lives in the in-package test file and is re-exported as
// BeginGuardedForTest (export_test.go) so the external postgres_test package
// shares this one implementation rather than keeping a second copy.
func beginGuarded(t *testing.T, tm spi.TransactionManager, ctx context.Context) (txID string, txCtx context.Context) {
	t.Helper()
	txID, txCtx, err := tm.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(func() { _ = tm.Rollback(txCtx, txID) })
	return txID, txCtx
}
