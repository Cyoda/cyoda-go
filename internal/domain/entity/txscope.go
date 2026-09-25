package entity

import (
	"context"
	"errors"
	"log/slog"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	"github.com/cyoda-platform/cyoda-go/internal/common"
)

// txScope owns the lifecycle of the transaction an entity write flow runs in.
// One deferred Release replaces the per-error-branch rollback calls a panic
// unwound straight past, leaving the transaction neither committed nor rolled
// back and its pooled connection never returned.
//
// Usage:
//
//	scope, err := h.beginScope(ctx)
//	if err != nil {
//	    return nil, classifyBeginErr(err)
//	}
//	defer scope.Release()
//	...
//	scope.Advance(result.FinalCtx, result.FinalTxID) // first statement after every engine call
//	...
//	err := scope.Commit()
//
// beginScope deliberately does NOT touch the joined gate: a joined request's
// gate is the join layer's, taken before the handler ran and released after it
// returns — hence after Release. The gate is a non-reentrant mutex, so nothing
// in a joined chain may take the transaction's gate again; Release returns
// before the gate for every joined scope.
//
// A joined scope never moves off the transaction it joined: the engine refuses
// the only step that opens a segment (a COMMIT_BEFORE_DISPATCH processor) on a
// joined chain, before anything is written.
type txScope struct {
	h *Handler

	// ctx and txID name the segment currently open. Advance moves them when the
	// engine segments via COMMIT_BEFORE_DISPATCH, which only an owned scope
	// does; Release always targets these.
	ctx  context.Context
	txID string

	owned bool
	done  bool
}

// beginScope begins a transaction, or joins one already on ctx (a routed
// compute-node callback). It performs no gating — see the type comment.
func (h *Handler) beginScope(ctx context.Context) (*txScope, error) {
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, err
	}
	return &txScope{h: h, ctx: txCtx, txID: txID, owned: owned}, nil
}

func (s *txScope) Ctx() context.Context { return s.ctx }
func (s *txScope) TxID() string         { return s.txID }
func (s *txScope) Owned() bool          { return s.owned }

// Advance moves the scope onto whichever segment the engine left open.
//
// It must be the FIRST statement after an engine call's `if err != nil` check —
// it cannot go before it, because the engine returns a nil EngineResult on every
// error path, so reading result.FinalCtx there would nil-dereference. The panic
// window between the call and the advance is therefore not closable here; the
// engine's own guard covers it, which is the correct place since the segment is
// the engine's until it is handed back.
func (s *txScope) Advance(ctx context.Context, txID string) {
	if ctx == nil || txID == "" {
		return
	}
	s.ctx, s.txID = ctx, txID
}

// Commit commits when this flow owns the transaction. Before committing it
// performs the spec-D2 pre-commit check: an expired/cancelled context fails
// closed WITHOUT marking the scope done, so the deferred Release still rolls
// the transaction back — the 408 contract ("nothing committed") depends on
// this ordering. The check applies only to the owned path: a joined callback
// never commits (the owner does), so its ctx state is irrelevant here.
//
// The commit itself runs on a common.CommitContext (WithoutCancel + its own
// bounded budget) so no deadline or disconnect on the request ctx can
// interrupt a commit in flight — an interrupted commit is an in-doubt
// outcome, not a rollback-able one.
//
// After a commit ATTEMPT the scope is done regardless of outcome: the commit
// may be partially applied, and aborting one another goroutine is running
// trips memory's ErrTxCommitInProgress path.
func (s *txScope) Commit() error {
	if s.owned {
		if err := s.ctx.Err(); err != nil {
			return err
		}
	}
	s.done = true
	return s.h.commitOwned(s.ctx, s.txID, s.owned)
}

// Release rolls back the segment currently open, unless the scope is already
// done or the transaction belongs to somebody else.
//
// A joined callback never rolls back its owner's transaction — an error on the
// joined path surfaces to the owner, which decides its fate.
func (s *txScope) Release() {
	if s.done {
		return
	}
	s.done = true
	if s.txID == "" || !s.owned {
		return
	}

	// Acquire the per-tx gate so the rollback is mutually exclusive with any
	// joined callback's access to the same transaction handle. No self-deadlock:
	// every `defer h.gate.Acquire(...)()` site in this package is inside an IIFE,
	// so the gate is free by outer-defer time, and an owned chain holds no gate
	// of the join layer's.
	//
	// What this does NOT preserve is failed-Save-then-rollback as one atomic
	// gated section: a joined callback can win the gate in the window between an
	// IIFE releasing it and Release re-acquiring, Save successfully, return 200
	// to its caller, and then have its write discarded by this rollback. That is
	// strictly better than the alternative it replaces — a leaked transaction —
	// and the joined caller's write was doomed either way once the owner failed.
	defer s.h.gate.Acquire(s.txID)()

	// Derive the budget only once the gate is held. RollbackContext bounds the
	// Rollback call, not the queue in front of it; charging the gate wait
	// against it would hand txMgr.Rollback an already-expired context, which on
	// postgres fails the rollback and destroys the pooled connection instead of
	// returning it — the outcome this scope exists to prevent.
	rbCtx, cancel := common.RollbackContext(s.ctx)
	defer cancel()

	if err := s.h.txMgr.Rollback(rbCtx, s.txID); err != nil && !errors.Is(err, spi.ErrTxNotFound) {
		slog.Warn("failed to roll back transaction", "pkg", "entity", "txID", s.txID, "err", err)
	}
}
