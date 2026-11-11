package httpmw

import (
	"bytes"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
)

// bufferedWriter holds a handler's response until flushTo. A joined request's
// handler runs under its transaction's gate; writing to the client there would
// make the gate wait on the client.
//
// What it holds is bounded by the joiner's own ceiling
// (CYODA_CALLOUT_JOINED_RESPONSE_MAX_BYTES): the owner's next move waits behind
// these bytes. A response past the ceiling is not truncated — the bytes are
// dropped, the writer says so, and the middleware fails the request.
//
// It deliberately implements neither http.Flusher nor http.Hijacker: a handler
// that wants to stream must fail loudly on the type assertion rather than buffer
// its whole stream here without bound.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int
	// over is set once the handler has written past the ceiling. What it wrote
	// is then let go of: nothing of it is sent.
	over bool
}

func newBufferedWriter(limit int) *bufferedWriter {
	return &bufferedWriter{header: make(http.Header), limit: limit}
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	if b.over || b.body.Len()+len(p) > b.limit {
		b.over = true
		b.body.Reset() // held under the transaction's gate: let it go at once
		return 0, txjoin.ErrHeldResponseTooLarge
	}
	return b.body.Write(p)
}

// statusCode is the status the handler wrote: 200 once it wrote a body
// without one, 0 before it wrote anything.
func (b *bufferedWriter) statusCode() int { return b.status }

// tooLarge reports whether the handler's answer passed the ceiling. The
// middleware asks after the handler has returned: a handler that ignores a write
// error, as most do, must not have its answer sent in part.
func (b *bufferedWriter) tooLarge() bool { return b.over }

// outcome is the door's reading of what the handler answered, for an answer
// no error funnel recorded: tooLargeErr when the answer passed the ceiling (it
// is what the door sends), a failure with the buffered status when that is 400
// or above (not retryable: nothing says it is), nil otherwise.
func (b *bufferedWriter) outcome(tooLargeErr *common.AppError) *common.Outcome {
	switch {
	case b.over:
		return &common.Outcome{Status: tooLargeErr.Status, Code: tooLargeErr.Code, Retryable: tooLargeErr.Retryable, Err: tooLargeErr}
	case b.status >= http.StatusBadRequest:
		return &common.Outcome{Status: b.status}
	default:
		return nil
	}
}

// flushTo sends what the handler wrote. A write error means the client has
// gone; there is nobody left to tell.
func (b *bufferedWriter) flushTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
