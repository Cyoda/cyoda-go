// Package httpmw provides HTTP middleware for the Cyoda application server.
package httpmw

import (
	"errors"
	"net/http"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
)

// TxJoin returns middleware that joins an inbound transaction routing token
// into the request context. It must run AFTER auth middleware so that the
// UserContext is available for tenant isolation checks inside txMgr.Join.
//
// If the X-Tx-Token header is absent the request passes through unchanged.
// On a valid token the joined context — admitted by the fence and detached from
// the client's cancellation — is propagated to the next handler.
// On an invalid/expired/not-found/superseded token the error is rendered via
// common.WriteError and the next handler never runs.
//
// The token value is never logged.
func TxJoin(signer *token.Signer, txMgr spi.TransactionManager, f *fence.Fence) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get(proxy.TxTokenHeader)
			if tok == "" {
				next.ServeHTTP(w, r)
				return
			}
			ctx, err := txjoin.JoinFromToken(r.Context(), signer, txMgr, f, tok)
			if err != nil {
				var appErr *common.AppError
				if !errors.As(err, &appErr) {
					appErr = common.Internal("failed to join transaction", err)
				}
				common.WriteError(w, r, appErr)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
