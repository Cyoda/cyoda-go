package openapivalidator

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
)

// teeWriter wraps an http.ResponseWriter, forwarding writes to the underlying
// writer while also capturing the response status and body for validation.
//
// It implements http.Flusher directly, delegating to the underlying writer
// when it supports flushing. It conditionally delegates http.Hijacker and
// io.ReaderFrom to the underlying writer when supported.
//
// http.Pusher and http.CloseNotifier are intentionally NOT delegated:
//   - Pusher (HTTP/2 server push) is unused in cyoda-go.
//   - CloseNotifier is deprecated.
type teeWriter struct {
	w        http.ResponseWriter
	captured bytes.Buffer
	status   int
	written  bool
}

// newTeeWriter wraps w and returns the narrowest concrete type that satisfies
// all optional interfaces supported by w.
//
// http.Flusher is implemented on *teeWriter itself (delegating when the
// underlying writer supports it, no-op otherwise), so no Flusher-only variant
// is needed. Variant structs are only created for http.Hijacker and
// io.ReaderFrom.
func newTeeWriter(w http.ResponseWriter) http.ResponseWriter {
	t := &teeWriter{w: w, status: http.StatusOK}
	_, isHijacker := w.(http.Hijacker)
	_, isReaderFrom := w.(io.ReaderFrom)
	switch {
	case isHijacker && isReaderFrom:
		return &teeHR{teeWriter: t}
	case isHijacker:
		return &teeH{teeWriter: t}
	case isReaderFrom:
		return &teeR{teeWriter: t}
	default:
		return t
	}
}

func (t *teeWriter) Header() http.Header { return t.w.Header() }

func (t *teeWriter) Write(p []byte) (int, error) {
	t.captured.Write(p)
	return t.w.Write(p)
}

func (t *teeWriter) WriteHeader(code int) {
	t.status = code
	t.written = true
	t.w.WriteHeader(code)
}

// Flush implements http.Flusher. It delegates to the underlying writer when it
// supports flushing and is a no-op otherwise.
func (t *teeWriter) Flush() {
	if f, ok := t.w.(http.Flusher); ok {
		f.Flush()
	}
}

// captureBytes / captureStatus accessors used by the middleware (Task 1.6).
// Defined here so all variant structs (which embed *teeWriter) inherit them.
func (t *teeWriter) captureBytes() []byte { return t.captured.Bytes() }
func (t *teeWriter) captureStatus() int   { return t.status }

// Hijacker / ReaderFrom variants. Each embeds *teeWriter, inheriting all base
// methods including Flush.

type teeH struct{ *teeWriter }

func (t *teeH) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return t.w.(http.Hijacker).Hijack()
}

type teeR struct{ *teeWriter }

func (t *teeR) ReadFrom(src io.Reader) (int64, error) {
	// Tee while delegating: pipe through an io.TeeReader so captured grows.
	return t.w.(io.ReaderFrom).ReadFrom(io.TeeReader(src, &t.captured))
}

type teeHR struct{ *teeWriter }

func (t *teeHR) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return t.w.(http.Hijacker).Hijack()
}
func (t *teeHR) ReadFrom(src io.Reader) (int64, error) {
	return t.w.(io.ReaderFrom).ReadFrom(io.TeeReader(src, &t.captured))
}

