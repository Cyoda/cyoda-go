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
// Otherwise the pass itself is verified first, and the transaction's queue is
// asked whether it has room — both before a byte of the body is read, so a
// forged or expired pass and a callback past the queue's cap each cost no
// buffer. Then the whole request is brought into memory, the joiner takes the
// transaction's lock and checks the pass under it, the handler runs on the
// joined context, and the response is sent once the lock has been released, so
// neither end of the request makes the lock wait on the compute node. On an
// invalid/expired/not-found/superseded token, or a full queue, the error is
// rendered via common.WriteError and the next handler never runs.
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
			// How many callbacks may queue for one transaction is bounded, and
			// the bound is read before the body is: a compute member firing
			// callbacks at one transaction costs this node no buffer per
			// refusal. The gate applies the same bound again when the lock is
			// taken, and that answer is the binding one.
			if err := j.CheckRoom(pass); err != nil {
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

			buffered := newBufferedWriter(j.MaxResponseBytes())
			err = j.RunVerified(r.Context(), pass, func(ctx context.Context) {
				next.ServeHTTP(buffered, r.WithContext(ctx))
			})
			if err != nil {
				writeJoinError(w, r, err)
				return
			}
			if buffered.tooLarge() {
				// Fail closed: the answer is not sent in part. The lock has
				// already been given back.
				common.WriteError(w, r, j.ResponseTooLargeError())
				return
			}
			buffered.flushTo(w)
		})
	}
}

// writeJoinError renders a refusal by the join layer. Every refusal on the pass
// is operational and carries its own status; the one error that is not is the
// request's own context ending while it queued for the transaction's lock — a
// request that touched nothing and whose client has gone. That is answered the
// way the entity service answers any cancellation that is not a domain failure:
// a ticketed 5xx with the cause kept on the chain. It is answered, rather than
// returned from silently, because net/http would otherwise send an implicit 200
// for a request that never ran.
func writeJoinError(w http.ResponseWriter, r *http.Request, err error) {
	var appErr *common.AppError
	if !errors.As(err, &appErr) {
		appErr = common.Internal("joined request ended before it took the transaction's lock", err)
	}
	common.WriteError(w, r, appErr)
}
