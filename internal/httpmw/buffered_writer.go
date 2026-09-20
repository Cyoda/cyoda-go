package httpmw

import (
	"bytes"
	"net/http"
)

// bufferedWriter holds a handler's response until flushTo. A joined request's
// handler runs under its transaction's lock; writing to the client there would
// make the lock wait on the client.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
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
	return b.body.Write(p)
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
