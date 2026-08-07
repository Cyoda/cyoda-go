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
// beginScope deliberately does NOT touch the joined gate. Folding it in would
// leave the gate permanently held on the joined path, where Release is a no-op —
// and it is a non-reentrant mutex, so every later joined callback on that txID
// would block forever.
//
// Flows therefore acquire it themselves and register `defer releaseGate()` AFTER
// `defer scope.Release()`, so LIFO frees the gate before Release runs. That
// ordering is lock-order hygiene, NOT a fix for a live deadlock: Release acquires
// nothing on the joined-entry path (it returns early), and on the joined-segment
// path it takes a DIFFERENT txID's gate, so reversing the two defers today merely
// holds both at once. It becomes a self-deadlock the moment Release is hardened to
// gate the entry transaction too — which is why the ordering is pinned by a test
// rather than left to be rediscovered.
type txScope struct {
	h *Handler

	// entryTxID is the transaction beginScope returned. It never changes, and
	// distinguishing it from txID is what lets a joined call release a segment
	// the engine opened without releasing its owner's transaction.
	entryTxID string

	// ctx and txID name the segment currently open. Advance moves them when the
	// engine segments via COMMIT_BEFORE_DISPATCH; Release always targets these,
	// never entryTxID.
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
	return &txScope{h: h, entryTxID: txID, ctx: txCtx, txID: txID, owned: owned}, nil
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

// Commit commits when this flow owns the transaction, and marks the scope done
// regardless of outcome. No path rolls back after a failed commit: the commit
// may be partially applied, and aborting one another goroutine is running trips
// memory's ErrTxCommitInProgress path.
func (s *txScope) Commit() error {
	s.done = true
	return s.h.commitOwned(s.ctx, s.txID, s.owned)
}

// Release rolls back the segment currently open, unless the scope is already
// done or the transaction belongs to somebody else.
//
// A joined callback never rolls back its owner's transaction — an error on the
// joined path surfaces to the owner, which decides its fate. The exception is a
// segment the engine opened during this call: that one is nobody else's, so it
// is released regardless of ownership whenever the scope has advanced past its
// entry txID.
func (s *txScope) Release() {
	if s.done {
		return
	}
	s.done = true
	if s.txID == "" {
		return
	}
	if !s.owned && s.txID == s.entryTxID {
		return
	}

	// Acquire the per-tx gate so the rollback is mutually exclusive with any
	// joined callback's access to the same transaction handle. No self-deadlock:
	// every `defer h.gate.Acquire(...)()` site in this package is inside an IIFE,
	// so the gate is free by outer-defer time.
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
