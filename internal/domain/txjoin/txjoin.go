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
)

// JoinFromToken resolves an inbound transaction routing token into a joined
// transaction context.
//
// Empty tok: returns ctx unchanged and nil error — downstream begins a
// standalone (non-tx) operation.
//
// Non-empty tok: verifies the token with signer, then calls txMgr.Join with
// the embedded TxRef. On success, returns the joined context. On failure,
// returns the original ctx alongside a mapped operational error so callers
// always have a valid context regardless of outcome.
//
// Error mapping:
//
//	token.ErrTokenExpired              → 410 TRANSACTION_EXPIRED
//	token.ErrTokenTampered/Invalid     → 401 UNAUTHORIZED
//	spi.ErrTxTenantMismatch            → 403 FORBIDDEN
//	spi.ErrTxNotFound/RolledBack/AlreadyCommitted → 404 TRANSACTION_NOT_FOUND
//
// The token is never logged.
func JoinFromToken(ctx context.Context, signer *token.Signer, txMgr spi.TransactionManager, tok string) (context.Context, error) {
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

	return joined, nil
}
