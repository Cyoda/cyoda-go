package common

import (
	"context"
	"time"
)

// rollbackBudget bounds the Rollback call itself. It does NOT bound the wait to
// reach it: a rollback first acquires the per-tx gate, and txgate.Registry.Acquire
// is a plain sync.Mutex with no context (memory and sqlite additionally take
// tx.OpMu inside Rollback). That wait terminates because the gate is never held
// across a dispatch and every operation it can be waiting on is itself bounded —
// by a callout's response timeout, or by the PostgreSQL statement ceiling. Making
// it a hard bound means giving both mutexes context-aware variants, which is a
// change to the concurrency model of core plus two plugins.
const rollbackBudget = 5 * time.Second

// RollbackContext derives the context a rollback must run on: the caller's
// values without the caller's cancellation, under a bounded deadline.
//
// WithoutCancel is load-bearing, not defensive. Every in-tree plugin's Rollback
// calls verifyTenant, which reads the UserContext off this context; a rollback on
// context.Background() is rejected with ErrTxTenantMismatch and the transaction
// leaks anyway. Dropping cancellation is what lets a timed-out or client-aborted
// request still return its connection.
func RollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackBudget)
}
