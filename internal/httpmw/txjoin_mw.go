// Package httpmw provides HTTP middleware for the Cyoda application server.
package httpmw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
)

// maxJoinedBodySize is the largest body any handler behind this middleware
// accepts: entity, grouped-stats, search, workflow, model and message handlers
// cap their own reads at 10 MiB or below, and still do, on the buffered bytes.
// The join layer must never refuse a body a handler would take.
const maxJoinedBodySize = 10 * 1024 * 1024

// TxJoin returns middleware that runs a request carrying a transaction routing
// token as a joined request of that transaction. It must run AFTER auth
// middleware so that the UserContext is available for tenant isolation checks
// inside txMgr.Join.
//
// If the X-Tx-Token header is absent the request passes through unchanged.
// Otherwise the pass itself is verified first — before a byte of the body is
// read, so a forged or expired pass costs no buffer — then the whole request is
// brought into memory, the joiner takes the transaction's lock and checks the
// pass under it, the handler runs on the joined context, and the response is
// sent once the lock has been released, so neither end of the request makes the
// lock wait on the compute node. On an invalid/expired/not-found/superseded
// token the error is rendered via common.WriteError and the next handler never
// runs.
//
// The token value is never logged.
func TxJoin(j *txjoin.Joiner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get(proxy.TxTokenHeader)
			if tok == "" {
				next.ServeHTTP(w, r)
				return
			}
			pass, err := j.Verify(tok)
			if err != nil {
				writeJoinError(w, r, err)
				return
			}
			// Read the body before the lock is taken: what the lock waits on
			// must never be the client. The handler's own MaxBytesReader still
			// applies to what it reads from the buffer.
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJoinedBodySize))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					common.WriteError(w, r, common.Operational(http.StatusRequestEntityTooLarge, common.ErrCodeBadRequest,
						fmt.Sprintf("request body exceeds %d bytes", maxJoinedBodySize)))
					return
				}
				common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeBadRequest, "failed to read request body"))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			buffered := newBufferedWriter()
			err = j.RunVerified(r.Context(), pass, func(ctx context.Context) {
				next.ServeHTTP(buffered, r.WithContext(ctx))
			})
			if err != nil {
				// The client went away while its request queued for the
				// transaction's lock: it touched nothing, and there is nobody
				// left to tell.
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				writeJoinError(w, r, err)
				return
			}
			buffered.flushTo(w)
		})
	}
}

// writeJoinError renders a refusal by the join layer. Anything that is not an
// operational error is the server's own and is ticketed.
func writeJoinError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		appErr = common.Internal("failed to join transaction", err)
	}
	common.WriteError(w, r, appErr)
}
