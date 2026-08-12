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

// commitBudget bounds the commit call itself once a flow decides to commit.
// Sibling of rollbackBudget with the same shape and rationale: the commit must
// not be cancellable by the request's deadline or disconnect (an interrupted
// commit is an in-doubt outcome — spec D2), but it must not hang forever
// either. 30s comfortably covers a large flush; PostgreSQL's own statement
// ceiling still applies underneath.
const commitBudget = 30 * time.Second

// CommitContext derives the context a commit runs on: the caller's values
// without the caller's cancellation, under a bounded deadline. WithoutCancel
// is load-bearing for the same reason as RollbackContext (tenant checks read
// UserContext off the ctx). It also clears the request-timeout marker so that
// a commit which overruns its own commitBudget is never misclassified as the
// client's 408 — a shielded-commit failure keeps its existing classification.
func CommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return commitContextWithBudget(ctx, commitBudget)
}

// commitContextWithBudget is CommitContext with an injectable budget —
// factored out so ShieldedCommitWithBudget (reqtimeout.go) can give tests a
// way to observe a commit-context expiry for real without waiting out the
// production 30s commitBudget. CommitContext itself always uses the fixed
// production budget; only tests should need the budget-injectable form.
func commitContextWithBudget(ctx context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	cleared := context.WithValue(context.WithoutCancel(ctx), reqTimeoutKey{}, nil)
	return context.WithTimeout(cleared, budget)
}
