package httpmw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/fence"
	"github.com/cyoda-platform/cyoda-go/internal/txgate"
)

// fakeJoinTM satisfies spi.TransactionManager by embedding the interface and
// overriding only Join. Unimplemented methods panic if unexpectedly called.
type fakeJoinTM struct {
	spi.TransactionManager
	joinErr error
}

func (f fakeJoinTM) Join(ctx context.Context, txID string) (context.Context, error) {
	if f.joinErr != nil {
		return nil, f.joinErr
	}
	return spi.WithTransaction(ctx, &spi.TransactionState{ID: txID}), nil
}

// make32 returns a deterministic 32-byte HMAC secret for tests.
func make32(t *testing.T) []byte {
	t.Helper()
	return []byte("test-secret-key-at-least-32byte!")
}

// gatedFence returns a fence and the gate registry its wait takes: the join
// layer must take the lock the fence waits on, so the two share one registry.
func gatedFence() (*fence.Fence, *txgate.Registry) {
	gate := txgate.New()
	return fence.New(gate), gate
}

// liveFence returns a fence on which calloutID is in progress on txID at
// major 1, the gate its wait takes, and the pass claims that name it.
func liveFence(t *testing.T, calloutID, txID string) (*fence.Fence, *txgate.Registry, token.Claims) {
	t.Helper()
	f, gate := gatedFence()
	_, end := f.Begin(context.Background(), calloutID, txID, nil)
	t.Cleanup(end)
	f.Advance(calloutID, 1)
	return f, gate, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
}

// joinerOver builds the Joiner the middleware takes, over signer, txMgr and a
// fence whose wait takes gate's locks. A nil meter (the no-op meter).
func joinerOver(t *testing.T, s *token.Signer, txMgr spi.TransactionManager, f *fence.Fence, gate *txgate.Registry) *txjoin.Joiner {
	t.Helper()
	j, err := txjoin.NewJoiner(s, txMgr, f, gate, nil)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}
	return j
}

// noCalloutJoiner returns a Joiner over a fence that knows no callout: every
// pass it is given is refused.
func noCalloutJoiner(t *testing.T, s *token.Signer, txMgr spi.TransactionManager) *txjoin.Joiner {
	t.Helper()
	f, gate := gatedFence()
	return joinerOver(t, s, txMgr, f, gate)
}

// liveJoiner returns a Joiner whose fence has the callout of txID in progress at
// major 1, the gate its lock is taken from, and the pass that names it.
func liveJoiner(t *testing.T, txID string) (*txjoin.Joiner, *txgate.Registry, string) {
	t.Helper()
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	f, gate, claims := liveFence(t, "req-"+txID, txID)
	pass, err := s.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return joinerOver(t, s, fakeJoinTM{}, f, gate), gate, pass
}

// withUserCtx attaches a minimal UserContext to the request so TxJoin's
// downstream calls (e.g. txMgr.Join tenant checks) have an authenticated identity.
func withUserCtx(r *http.Request) *http.Request {
	uc := &spi.UserContext{
		UserID:   "test-user",
		UserName: "test",
		Tenant:   spi.Tenant{ID: "local", Name: "local"},
	}
	return r.WithContext(spi.WithUserContext(r.Context(), uc))
}

// okHandler returns a simple 200 OK handler.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestTxJoin_JoinsAndPassesCtx(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, gate, claims := liveFence(t, "req-tx-1", "tx-1")
	tok, _ := s.Issue(claims)
	var sawTx string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tx := spi.GetTransaction(r.Context()); tx != nil {
			sawTx = tx.ID
		}
		w.WriteHeader(200)
	})
	h := TxJoin(joinerOver(t, s, fakeJoinTM{}, f, gate))(next)
	req := httptest.NewRequest("POST", "/entity", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	req = withUserCtx(req)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if sawTx != "tx-1" {
		t.Fatalf("expected joined tx-1, got %q", sawTx)
	}
}

func TestTxJoin_NotFoundReturns404(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-x", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-x", Major: 1})
	h := TxJoin(noCalloutJoiner(t, s, fakeJoinTM{joinErr: spi.ErrTxNotFound}))(okHandler())
	req := httptest.NewRequest("POST", "/entity", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	req = withUserCtx(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestTxJoin_NoToken_Passthrough(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	h := TxJoin(noCalloutJoiner(t, s, fakeJoinTM{}))(okHandler())
	req := httptest.NewRequest("GET", "/entity/123", nil)
	// No X-Tx-Token header set.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 passthrough, got %d", rec.Code)
	}
}

func TestTxJoin_TamperedTokenReturns401(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	// Issue with a different signer so verification fails.
	s2, _ := token.NewSigner([]byte("different-secret-key-at-least-32b!"))
	tok, _ := s2.Issue(token.Claims{NodeID: "local", TxRef: "tx-bad", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-bad", Major: 1})
	h := TxJoin(noCalloutJoiner(t, s, fakeJoinTM{}))(okHandler())
	req := httptest.NewRequest("POST", "/entity", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	req = withUserCtx(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// problemProperties is the RFC 9457 extension block the error writer fills.
type problemProperties struct {
	ErrorCode string `json:"errorCode"`
	Retryable bool   `json:"retryable"`
}

// problemProps decodes the problem body's extension properties.
func problemProps(t *testing.T, rec *httptest.ResponseRecorder) problemProperties {
	t.Helper()
	var pd struct {
		Properties problemProperties `json:"properties"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("body: %v", err)
	}
	return pd.Properties
}

// A callback whose callout has ended is refused 410 CALLOUT_SUPERSEDED before
// the handler runs — on a read and on a write alike.
func TestTxJoin_CalloutEnded_410(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	f, gate := gatedFence()
	_, end := f.Begin(context.Background(), "req-1", "tx-1", nil)
	f.Advance("req-1", 1)
	end()
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})

	for _, method := range []string{http.MethodGet, http.MethodPost} { // a read and a write
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })
		req := withUserCtx(httptest.NewRequest(method, "/entity/x", nil))
		req.Header.Set(proxy.TxTokenHeader, tok)
		rec := httptest.NewRecorder()
		TxJoin(joinerOver(t, s, fakeJoinTM{}, f, gate))(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusGone {
			t.Fatalf("%s: status = %d; want 410", method, rec.Code)
		}
		props := problemProps(t, rec)
		if props.ErrorCode != "CALLOUT_SUPERSEDED" || props.Retryable {
			t.Fatalf("%s: problem = %+v", method, props)
		}
	}
}

// A pass presented by the wrong tenant is answered 403 on this door too,
// whatever the fence knows about the callout it names, and the handler never
// runs.
func TestTxJoin_TenantMismatchIsOneAnswerWhateverTheFenceKnows(t *testing.T) {
	for name, setup := range map[string]func(*testing.T) (*fence.Fence, *txgate.Registry, token.Claims){
		"callout current": func(t *testing.T) (*fence.Fence, *txgate.Registry, token.Claims) {
			return liveFence(t, "req-1", "tx-1")
		},
		"callout superseded": func(t *testing.T) (*fence.Fence, *txgate.Registry, token.Claims) {
			f, gate, claims := liveFence(t, "req-1", "tx-1")
			f.Advance("req-1", 2)
			return f, gate, claims
		},
		"callout unknown": func(t *testing.T) (*fence.Fence, *txgate.Registry, token.Claims) {
			f, gate := gatedFence()
			return f, gate, token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, gate, claims := setup(t)
			tok, _ := s.Issue(claims)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })
			req := withUserCtx(httptest.NewRequest(http.MethodGet, "/entity/x", nil))
			req.Header.Set(proxy.TxTokenHeader, tok)
			rec := httptest.NewRecorder()
			TxJoin(joinerOver(t, s, fakeJoinTM{joinErr: spi.ErrTxTenantMismatch}, f, gate))(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want 403", rec.Code)
			}
			if code := problemProps(t, rec).ErrorCode; code != "FORBIDDEN" {
				t.Fatalf("errorCode = %q; want FORBIDDEN", code)
			}
		})
	}
}

// The response is sent after the lock is released: a cnode that does not read
// its response does not hold the transaction.
func TestTxJoin_ResponseIsSentAfterTheLockIsReleased(t *testing.T) {
	j, gate, pass := liveJoiner(t, "tx-1")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	w := &lockProbeWriter{ResponseRecorder: httptest.NewRecorder(), gate: gate, txID: "tx-1"}
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", strings.NewReader(`{}`)))
	req.Header.Set(proxy.TxTokenHeader, pass)
	TxJoin(j)(next).ServeHTTP(w, req)

	if w.Code != http.StatusCreated || w.Body.String() != `{"ok":true}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w.writes == 0 || w.writesUnderLock != 0 {
		t.Fatalf("%d of %d writes to the client were made while the transaction's lock was held", w.writesUnderLock, w.writes)
	}
}

// lockProbeWriter notes, on every write to the client, whether the
// transaction's lock is free.
type lockProbeWriter struct {
	*httptest.ResponseRecorder
	gate                    *txgate.Registry
	txID                    string
	writes, writesUnderLock int
}

func (w *lockProbeWriter) probe() {
	w.writes++
	free := make(chan struct{})
	go func() { w.gate.Acquire(w.txID)(); close(free) }()
	select {
	case <-free:
	case <-time.After(50 * time.Millisecond):
		w.writesUnderLock++
	}
}
func (w *lockProbeWriter) WriteHeader(code int)        { w.probe(); w.ResponseRecorder.WriteHeader(code) }
func (w *lockProbeWriter) Write(b []byte) (int, error) { w.probe(); return w.ResponseRecorder.Write(b) }

// Every route behind the middleware runs under the lock, whatever it serves —
// model, message, search, statistics, audit are all just `next` to it — and is
// refused once its cnode is replaced.
func TestTxJoin_EveryRouteRunsUnderTheLock(t *testing.T) {
	routes := []string{"/model/export/x/1", "/message/new/s", "/search/direct/x/1", "/entity/stats", "/audit/entity/x"}

	// The same routes, once the cnode that held the callout has been replaced.
	s, _ := token.NewSigner(make32(t))
	replaced, replacedGate := gatedFence()
	_, endReplaced := replaced.Begin(context.Background(), "req-1", "tx-1", nil)
	t.Cleanup(endReplaced)
	replaced.Advance("req-1", 1)
	stale, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1",
		ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	replaced.Advance("req-1", 2)
	for _, route := range routes {
		req := withUserCtx(httptest.NewRequest(http.MethodPost, route, nil))
		req.Header.Set(proxy.TxTokenHeader, stale)
		rec := httptest.NewRecorder()
		TxJoin(joinerOver(t, s, fakeJoinTM{}, replaced, replacedGate))(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatalf("%s: handler must not run", route) }),
		).ServeHTTP(rec, req)
		if rec.Code != http.StatusGone || problemProps(t, rec).ErrorCode != "CALLOUT_SUPERSEDED" {
			t.Fatalf("%s: status = %d, problem = %+v; want 410 CALLOUT_SUPERSEDED", route, rec.Code, problemProps(t, rec))
		}
	}

	j, gate, pass := liveJoiner(t, "tx-1")
	for _, route := range routes {
		held := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			free := make(chan struct{})
			go func() { gate.Acquire("tx-1")(); close(free) }()
			select {
			case <-free:
			case <-time.After(50 * time.Millisecond):
				held = true
			}
		})
		req := withUserCtx(httptest.NewRequest(http.MethodPost, route, nil))
		req.Header.Set(proxy.TxTokenHeader, pass)
		TxJoin(j)(next).ServeHTTP(httptest.NewRecorder(), req)
		if !held {
			t.Fatalf("%s ran without the transaction's lock", route)
		}
	}
}

// The request body is read before the lock is taken: a cnode that trickles its
// body does not hold the transaction.
func TestTxJoin_BodyIsReadBeforeTheLockIsTaken(t *testing.T) {
	j, gate, pass := liveJoiner(t, "tx-1")
	body := newLockProbeBody(strings.NewReader(`{"a":1}`), gate, "tx-1")
	var got string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	})
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", body))
	req.Header.Set(proxy.TxTokenHeader, pass)
	TxJoin(j)(next).ServeHTTP(httptest.NewRecorder(), req)
	if got != `{"a":1}` {
		t.Fatalf("handler read %q", got)
	}
	if body.reads == 0 || body.readsUnderLock != 0 {
		t.Fatalf("%d of %d reads of the client's body were made under the transaction's lock", body.readsUnderLock, body.reads)
	}
}

// lockProbeBody mirrors lockProbeWriter on Read: it notes, on every read of the
// client's body, whether the transaction's lock is free. reading is closed on the
// FIRST read, so a test waiting for the middleware to be inside the body read
// waits for that rather than guessing how long it takes.
type lockProbeBody struct {
	io.Reader
	gate                  *txgate.Registry
	txID                  string
	reading               chan struct{}
	readingOnce           sync.Once
	reads, readsUnderLock int
}

func newLockProbeBody(r io.Reader, gate *txgate.Registry, txID string) *lockProbeBody {
	return &lockProbeBody{Reader: r, gate: gate, txID: txID, reading: make(chan struct{})}
}

func (b *lockProbeBody) Read(p []byte) (int, error) {
	b.readingOnce.Do(func() { close(b.reading) })
	b.reads++
	free := make(chan struct{})
	go func() { b.gate.Acquire(b.txID)(); close(free) }()
	select {
	case <-free:
	case <-time.After(50 * time.Millisecond):
		b.readsUnderLock++
	}
	return b.Reader.Read(p)
}

// A joined request whose client sends its headers and then stalls does not
// hold the transaction's lock: another joined request on the same transaction
// completes meanwhile.
func TestTxJoin_StalledBodyDoesNotHoldTheLock(t *testing.T) {
	j, gate, pass := liveJoiner(t, "tx-1")
	mw := TxJoin(j)

	stalled, unblock := io.Pipe() // headers sent, body never arrives
	body := newLockProbeBody(stalled, gate, "tx-1")
	stalledDone := make(chan struct{})
	go func() {
		defer close(stalledDone)
		req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", body))
		req.Header.Set(proxy.TxTokenHeader, pass)
		mw(okHandler()).ServeHTTP(httptest.NewRecorder(), req)
	}()
	// Wait for the stalled request to be inside the body read, rather than
	// sleeping: a sleep that ran out too early would pass vacuously, before the
	// middleware had reached the read this test is about.
	select {
	case <-body.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled request never reached the body read")
	}

	otherDone := make(chan int, 1)
	go func() {
		req := withUserCtx(httptest.NewRequest(http.MethodGet, "/entity/x", nil))
		req.Header.Set(proxy.TxTokenHeader, pass)
		rec := httptest.NewRecorder()
		mw(okHandler()).ServeHTTP(rec, req)
		otherDone <- rec.Code
	}()
	select {
	case code := <-otherDone:
		if code != http.StatusOK {
			t.Fatalf("the other joined request = %d; want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body held the transaction's lock: the other joined request did not complete")
	}
	_ = unblock.Close()
	<-stalledDone
}

// A body over the join layer's limit is refused before any lock is taken.
func TestTxJoin_OversizeBody_413(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity", io.LimitReader(zeroes{}, maxJoinedBodySize+1)))
	req.Header.Set(proxy.TxTokenHeader, pass)
	rec := httptest.NewRecorder()
	TxJoin(j)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", rec.Code)
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { clear(p); return len(p), nil }
