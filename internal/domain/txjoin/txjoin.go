// Package txjoin turns an inbound transaction routing token into a joined
// transaction context, mapping verify/join failures to reusable operational
// error codes. Transport-agnostic: used by both the gRPC interceptor and the
// HTTP middleware.
package txjoin

import (
	"context"
	"errors"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// JoinFromToken resolves an inbound transaction routing token into a joined
// transaction context.
//
// Empty tok: returns ctx unchanged and nil error — downstream begins a
// standalone (non-tx) operation.
//
// Non-empty tok: verifies the token with signer, calls txMgr.Join with the
// embedded TxRef, and then asks the fence whether the callout the pass names is
// still this compute node's. On success, returns the joined, admitted context.
// On failure, returns the original ctx alongside a mapped operational error so
// callers always have a valid context regardless of outcome — a caller that
// drops the error would otherwise run the request unfenced.
//
// Order: verify the pass → Join, which checks the tenant → the fence. The
// tenant check comes first so that a stolen pass tells another tenant nothing
// about which callouts exist, and a callback that arrives after the TRANSACTION
// has ended is answered TRANSACTION_NOT_FOUND as before; CALLOUT_SUPERSEDED is
// the answer while the transaction is still open.
//
// The returned context is detached from the request's cancellation: a joined
// request runs on the transaction of the operation it belongs to, and a compute
// node that goes away in the middle of a statement must not take that
// operation's connection with it. A joined request ends by finishing or by
// being refused at a check.
//
// Error mapping:
//
//	token.ErrTokenExpired              → 410 TRANSACTION_EXPIRED
//	token.ErrTokenTampered/Invalid     → 401 UNAUTHORIZED
//	spi.ErrTxTenantMismatch            → 403 FORBIDDEN
//	spi.ErrTxNotFound/RolledBack/AlreadyCommitted → 404 TRANSACTION_NOT_FOUND
//	a pass that is no longer current    → 410 CALLOUT_SUPERSEDED
//
// The token is never logged.
func JoinFromToken(ctx context.Context, signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, tok string) (context.Context, error) {
	if tok == "" {
		return ctx, nil
	}

	claims, err := signer.Verify(tok)
	if err != nil {
		switch {
		case errors.Is(err, token.ErrTokenExpired):
			return ctx, common.Operational(http.StatusGone, common.ErrCodeTransactionExpired, "transaction token has expired")
		default: // ErrTokenTampered or ErrTokenInvalid
			return ctx, common.Operational(http.StatusUnauthorized, common.ErrCodeUnauthorized, "invalid transaction token")
		}
	}

	joined, err := txMgr.Join(ctx, claims.TxRef)
	if err != nil {
		switch {
		case errors.Is(err, spi.ErrTxTenantMismatch):
			return ctx, common.Operational(http.StatusForbidden, common.ErrCodeForbidden, "transaction belongs to a different tenant")
		case errors.Is(err, spi.ErrTxNotFound),
			errors.Is(err, spi.ErrTxRolledBack),
			errors.Is(err, spi.ErrTxAlreadyCommitted):
			return ctx, common.Operational(http.StatusNotFound, common.ErrCodeTransactionNotFound, "transaction not found or no longer active")
		default:
			return ctx, common.Internal("failed to join transaction", err)
		}
	}

	pairs := make([]fence.Pair, 0, 1+len(claims.Outer))
	pairs = append(pairs, fence.Pair{Callout: claims.Callout, Major: claims.Major, Minor: claims.Minor})
	pairs = append(pairs, claims.Outer...)
	admitted, err := f.Admit(joined, pairs)
	if err != nil {
		return ctx, err
	}
	return context.WithoutCancel(admitted), nil
}

// Joiner is what a callback door needs to run a request as a joined request of
// the transaction its pass names.
type Joiner struct {
	signer *token.Signer
	txMgr  spi.TransactionManager
	fence  *fence.Fence
	gate   *txgate.Registry
}

func NewJoiner(signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, gate *txgate.Registry) *Joiner {
	return &Joiner{signer: signer, txMgr: txMgr, fence: f, gate: gate}
}

// Run runs handler as a joined request. The storage contract leaves it to the
// application to serialise its own concurrent operations on one transaction, so
// EVERY joined request — a read as much as a write, an entity request or not —
// holds the transaction's lock from before its first store operation until its
// handler returns: during a callout the transaction's users are its current
// compute node's callbacks, one at a time.
//
// Order: verify → Join (tenant) → Admit → take the lock → Check under the lock
// → handler → release. The check under the lock is the one that gives the right
// to touch the transaction: the owner's wait takes the same lock, so a request
// either passed this check before the number rose — and the owner waits for it
// — or is refused here. The engine gives the lock up for the length of a
// callout of the callback's own through the handle installed here
// (txgate.Suspend).
//
// A non-nil error is a refusal: handler did not run. The caller sends its
// response only after Run has returned, so that a compute node that does not
// read its response holds nothing. The caller must also have the whole request
// in memory before it calls Run: what the lock waits on must never be the
// client.
//
// An empty tok is not a joined request: handler runs on ctx as it is.
func (j *Joiner) Run(ctx context.Context, tok string, handler func(ctx context.Context)) error {
	if tok == "" {
		handler(ctx)
		return nil
	}
	joined, err := JoinFromToken(ctx, j.signer, j.txMgr, j.fence, tok)
	if err != nil {
		return err
	}
	txID := spi.GetTransaction(joined).ID

	release := j.gate.Acquire(txID)
	// The closure reads `release` when it runs: a Suspend/resume in between
	// stores the re-acquired lock's release through the pointer below.
	defer func() { release() }()
	joined, _ = txgate.WithHeld(joined, j.gate, txID, &release)

	if err := fence.Check(joined); err != nil {
		return err
	}
	handler(joined)
	return nil
}
