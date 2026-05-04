package openapivalidator

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTee_CapturesBytesAndForwards(t *testing.T) {
	rec := httptest.NewRecorder()
	tee := newTeeWriter(rec).(*teeWriter)

	tee.WriteHeader(201)
	if _, err := tee.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := rec.Code; got != 201 {
		t.Errorf("forwarded status = %d, want 201", got)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("forwarded body = %q, want %q", got, `{"ok":true}`)
	}
	if got := tee.captured.String(); got != `{"ok":true}` {
		t.Errorf("captured body = %q, want %q", got, `{"ok":true}`)
	}
	if got := tee.status; got != 201 {
		t.Errorf("captured status = %d, want 201", got)
	}
}

type flusherRecorder struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (f *flusherRecorder) Flush() { f.flushed = true; f.ResponseRecorder.Flush() }

// TestTee_DelegatesFlusher_WhenUnderlyingFlushable verifies that calling Flush
// on the tee propagates to the underlying writer when it supports flushing.
func TestTee_DelegatesFlusher_WhenUnderlyingFlushable(t *testing.T) {
	rec := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	tee := newTeeWriter(rec)

	flusher, ok := tee.(http.Flusher)
	if !ok {
		t.Fatal("tee does not implement http.Flusher")
	}
	flusher.Flush()
	if !rec.flushed {
		t.Error("Flush did not delegate to underlying writer")
	}
}

// nonFlusherWriter is an http.ResponseWriter that intentionally does not
// implement http.Flusher (or Hijacker, ReaderFrom).
type nonFlusherWriter struct {
	headers http.Header
	body    []byte
	code    int
}

func (n *nonFlusherWriter) Header() http.Header  { return n.headers }
func (n *nonFlusherWriter) Write(p []byte) (int, error) {
	n.body = append(n.body, p...)
	return len(p), nil
}
func (n *nonFlusherWriter) WriteHeader(c int) { n.code = c }

// TestTee_FlushIsNoop_WhenUnderlyingNotFlushable verifies that Flush is safe
// to call even when the underlying writer doesn't support flushing —
// *teeWriter implements http.Flusher unconditionally; the call is a no-op
// in this case.
func TestTee_FlushIsNoop_WhenUnderlyingNotFlushable(t *testing.T) {
	rec := &nonFlusherWriter{headers: http.Header{}}
	tee := newTeeWriter(rec)
	if _, ok := tee.(http.Flusher); !ok {
		t.Fatal("tee should always implement http.Flusher")
	}
	// Should not panic.
	tee.(http.Flusher).Flush()
}

type readerFromRecorder struct {
	*httptest.ResponseRecorder
	readFromCalled bool
}

func (r *readerFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	r.readFromCalled = true
	return io.Copy(r.ResponseRecorder, src)
}

// captureGetter is the accessor interface every tee variant satisfies via
// its embedded *teeWriter.
type captureGetter interface {
	captureBytes() []byte
}

func TestTee_DelegatesReaderFrom(t *testing.T) {
	rec := &readerFromRecorder{ResponseRecorder: httptest.NewRecorder()}
	tee := newTeeWriter(rec)

	rf, ok := tee.(io.ReaderFrom)
	if !ok {
		t.Fatal("tee does not implement io.ReaderFrom when underlying does")
	}
	src := strings.NewReader("hello")
	if _, err := rf.ReadFrom(src); err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if !rec.readFromCalled {
		t.Error("ReadFrom did not delegate to underlying writer")
	}
	cg, ok := tee.(captureGetter)
	if !ok {
		t.Fatalf("tee %T does not implement captureGetter", tee)
	}
	if got := string(cg.captureBytes()); got != "hello" {
		t.Errorf("captured = %q, want %q", got, "hello")
	}
}

type hijackerRecorder struct {
	*httptest.ResponseRecorder
}

func (h *hijackerRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

func TestTee_DelegatesHijacker(t *testing.T) {
	rec := &hijackerRecorder{ResponseRecorder: httptest.NewRecorder()}
	tee := newTeeWriter(rec)

	if _, ok := tee.(http.Hijacker); !ok {
		t.Fatal("tee does not implement http.Hijacker when underlying does")
	}
}

func TestTee_DefaultStatusIs200(t *testing.T) {
	rec := httptest.NewRecorder()
	tee := newTeeWriter(rec).(*teeWriter)
	if _, err := tee.Write([]byte("body")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if tee.status != 200 {
		t.Errorf("default status after implicit WriteHeader = %d, want 200", tee.status)
	}
}
