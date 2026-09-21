package httpmw

import (
	"bytes"
	"net/http"

	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
)

// bufferedWriter holds a handler's response until flushTo. A joined request's
// handler runs under its transaction's lock; writing to the client there would
// make the lock wait on the client.
//
// What it holds is bounded by txjoin.MaxHeldResponseBytes: the owner's next
// move waits behind these bytes. A response past the ceiling is not truncated —
// the bytes are dropped, the writer says so, and the middleware fails the
// request.
//
// It deliberately implements neither http.Flusher nor http.Hijacker: a handler
// that wants to stream must fail loudly on the type assertion rather than buffer
// its whole stream here without bound.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	// over is set once the handler has written past the ceiling. What it wrote
	// is then let go of: nothing of it is sent.
	over bool
}

func newBufferedWriter() *bufferedWriter { return &bufferedWriter{header: make(http.Header)} }

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
	if b.over || b.body.Len()+len(p) > txjoin.MaxHeldResponseBytes {
		b.over = true
		b.body.Reset() // held under the transaction's lock: let it go at once
		return 0, txjoin.ErrHeldResponseTooLarge
	}
	return b.body.Write(p)
}

// tooLarge reports whether the handler's answer passed the ceiling. The
// middleware asks after the handler has returned: a handler that ignores a write
// error, as most do, must not have its answer sent in part.
func (b *bufferedWriter) tooLarge() bool { return b.over }

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
