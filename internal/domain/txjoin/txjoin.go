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
