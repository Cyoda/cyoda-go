package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
)

// A joined response is held in memory while the transaction's lock is held, so
// it has a ceiling of its own. Past it the request fails with a ticketed 5xx:
// the alternative would be to send a truncated answer, which is a wrong answer.
func TestTxJoin_AResponseOverTheCeilingFails_AndIsNotTruncated(t *testing.T) {
	j, gate, pass := liveJoiner(t, "tx-1")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		chunk := make([]byte, 1<<20)
		for written := 0; written <= txjoin.MaxHeldResponseBytes; written += len(chunk) {
			// The writer reports the refusal; a handler that ignores it, as most
			// do on a write error, must still not have its answer truncated.
			_, _ = w.Write(chunk)
		}
	})
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", nil))
	req.Header.Set(proxy.TxTokenHeader, pass)
	rec := httptest.NewRecorder()

	TxJoin(j)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want a ticketed 500", rec.Code)
	}
	if props := problemProps(t, rec); props.ErrorCode != "SERVER_ERROR" {
		t.Errorf("errorCode = %q; want SERVER_ERROR", props.ErrorCode)
	}
	if rec.Body.Len() > 4096 {
		t.Errorf("the client got %d bytes: a refused response must not be sent at all", rec.Body.Len())
	}
	// The lock is given back, whatever the answer was.
	free := make(chan struct{})
	go func() { gate.Acquire("tx-1")(); close(free) }()
	select {
	case <-free:
	case <-time.After(5 * time.Second):
		t.Fatal("the transaction's lock was not released")
	}
}

// A response at the ceiling is answered whole.
func TestTxJoin_AResponseAtTheCeilingIsSentWhole(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")
	body := make([]byte, txjoin.MaxHeldResponseBytes)
	for i := range body {
		body[i] = 'a'
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			t.Errorf("a response of exactly the ceiling was refused: %v", err)
		}
	})
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", nil))
	req.Header.Set(proxy.TxTokenHeader, pass)
	rec := httptest.NewRecorder()

	TxJoin(j)(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.Len() != len(body) {
		t.Fatalf("status = %d, %d bytes; want 200 and all %d", rec.Code, rec.Body.Len(), len(body))
	}
}

// The writer a handler is given behind TxJoin buffers the whole response, so it
// must not advertise streaming. A handler that flushed or hijacked through it
// would either buffer an unbounded stream in memory or write to the client while
// the transaction's lock is held — the two things the buffering exists to
// prevent. Both are better as a failed type assertion the handler sees than as a
// silent success, so the absence is pinned here rather than left to chance.
func TestTxJoin_TheWriterHandlersGetCannotStream(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")

	var ran, flusher, hijacker bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		_, flusher = w.(http.Flusher)
		_, hijacker = w.(http.Hijacker)
		w.WriteHeader(http.StatusOK)
	})
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", nil))
	req.Header.Set(proxy.TxTokenHeader, pass)
	TxJoin(j)(next).ServeHTTP(httptest.NewRecorder(), req)

	if !ran {
		t.Fatal("the handler never ran: flusher/hijacker default to false, which would pass this test vacuously")
	}
	if flusher {
		t.Error("the buffered writer implements http.Flusher: a streaming handler would buffer its whole stream instead of failing")
	}
	if hijacker {
		t.Error("the buffered writer implements http.Hijacker: a handler could take the connection while the transaction's lock is held")
	}
}
