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
//	if err := scope.Seal(); err != nil {
//	    return nil, classifyCommitErr(err)
//	}
//	... // the final writes
//	err := scope.Commit()
//
// An owned scope seals before it commits or rolls back (txcoord's Seal): the
// seal waits for the callback scopes of the transaction still in progress,
// refuses every later one, and reports a transaction a failed joined write made
// rollback-only. A flow seals before its final writes, so nothing joined runs
// beside them; Commit and Release seal too, and a second seal is the first
// one's answer. The coordination forgets the transaction once the commit or
// rollback is done.
//
// A joined scope neither seals nor commits nor rolls back: the transaction is
// the owner's, and the join layer's scope decides what becomes of the
// request's writes.
//
// A joined scope never moves off the transaction it joined: the engine refuses
// the only step that opens a segment (a COMMIT_BEFORE_DISPATCH processor) on a
// joined chain, before the transaction is flushed or committed.
type txScope struct {
	h *Handler

	// ctx and txID name the segment currently open. Advance moves them when the
	// engine segments via COMMIT_BEFORE_DISPATCH, which only an owned scope
	// does; Release always targets these.
	ctx  context.Context
	txID string

	owned bool
	done  bool

	// finish forgets the sealed transaction in the coordination; nil until
	// the scope seals. sealErr is the seal's answer.
	finish  func()
	sealErr error
}

// beginScope begins a transaction, or joins one already on ctx (a routed
// compute-node callback).
func (h *Handler) beginScope(ctx context.Context) (*txScope, error) {
	txID, txCtx, owned, err := h.beginOrJoin(ctx)
	if err != nil {
		return nil, err
	}
	return &txScope{h: h, ctx: txCtx, txID: txID, owned: owned}, nil
}

func (s *txScope) Ctx() context.Context { return s.ctx }
func (s *txScope) TxID() string         { return s.txID }

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

// Seal cuts the owned transaction off before the flow's final writes: it
// waits for the callback scopes in progress and refuses every later joined
// request. It reports the lost write race (spi.ErrTxAborted), the cause that
// broke the transaction, or common.TransactionRollbackOnly when a joined write
// failed; the flow then fails, and Release rolls back. A second call returns
// the first one's answer. A joined scope does not seal: nil.
func (s *txScope) Seal() error {
	if !s.owned || s.txID == "" {
		return nil
	}
	if s.finish == nil {
		s.finish, s.sealErr = s.h.coord.Seal(s.ctx, s.txID)
	}
	return s.sealErr
}

// Commit commits when this flow owns the transaction. It seals first — a flow
// that sealed before its final writes gets that seal's answer — and a refused
// seal leaves the scope for Release to roll back. Before committing it
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
// trips memory's ErrTxCommitInProgress path. The coordination forgets the
// transaction after the attempt.
func (s *txScope) Commit() error {
	if s.owned {
		if err := s.Seal(); err != nil {
			return err
		}
		if err := s.ctx.Err(); err != nil {
			return err
		}
	}
	s.done = true
	if s.finish != nil {
		defer s.finish()
	}
	return s.h.commitOwned(s.ctx, s.txID, s.owned)
}

// Release rolls back the segment currently open, unless the scope is already
// done or the transaction belongs to somebody else. It seals first, ignoring
// the answer — a rollback is what a refused seal asks for — and forgets the
// transaction after the rollback.
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
	_ = s.Seal()
	defer s.finish()

	// Derive the budget only once the seal is done. RollbackContext bounds the
	// Rollback call, not the wait in front of it; charging the seal's wait
	// against it would hand txMgr.Rollback an already-expired context, which on
	// postgres fails the rollback and destroys the pooled connection instead of
	// returning it — the outcome this scope exists to prevent.
	rbCtx, cancel := common.RollbackContext(s.ctx)
	defer cancel()

	if err := s.h.txMgr.Rollback(rbCtx, s.txID); err != nil && !errors.Is(err, spi.ErrTxNotFound) {
		slog.Warn("failed to roll back transaction", "pkg", "entity", "txID", s.txID, "err", err)
	}
}
