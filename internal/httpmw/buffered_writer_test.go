package httpmw

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
)

// The writer a handler is given behind TxJoin buffers the whole response, so it
// must not advertise streaming. A handler that flushed or hijacked through it
// would either buffer an unbounded stream in memory or write to the client while
// the transaction's lock is held — the two things the buffering exists to
// prevent. Both are better as a failed type assertion the handler sees than as a
// silent success, so the absence is pinned here rather than left to chance.
func TestTxJoin_TheWriterHandlersGetCannotStream(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")

	var flusher, hijacker bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, flusher = w.(http.Flusher)
		_, hijacker = w.(http.Hijacker)
		w.WriteHeader(http.StatusOK)
	})
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", nil))
	req.Header.Set(proxy.TxTokenHeader, pass)
	TxJoin(j)(next).ServeHTTP(httptest.NewRecorder(), req)

	if flusher {
		t.Error("the buffered writer implements http.Flusher: a streaming handler would buffer its whole stream instead of failing")
	}
	if hijacker {
		t.Error("the buffered writer implements http.Hijacker: a handler could take the connection while the transaction's lock is held")
	}
}
