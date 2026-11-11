package httpmw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/go-chi/chi/v5"

	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/domain/txjoin"
	"github.com/cyoda-platform/cyoda-go/internal/txcoord"
)

// captureSlog redirects the default logger into a buffer for the duration of
// the test, restoring the previous default on cleanup. DEBUG and above are
// captured so a test can assert a log line was (or was not) emitted at DEBUG.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// testResponseMax is the joined-answer ceiling these tests build a Joiner with.
// It is the shipped default's shape at a size a test writes past in one line,
// so the ceiling's behaviour is pinned without allocating megabytes to do it.
const testResponseMax = 64 << 10

// testMaxWaiters is the queue cap these tests build a Joiner with — the shipped
// default of CYODA_CALLOUT_JOINED_MAX_WAITERS, high enough that no test reaches
// it by accident. The test that means to reach it says so.
const testMaxWaiters = 128

// fakeJoinTM satisfies spi.TransactionManager by embedding the interface and
// overriding Join and the savepoint calls a joined request's scope makes, which
// it takes and ends without doing anything. Other methods panic if
// unexpectedly called.
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

func (fakeJoinTM) Savepoint(context.Context, string) (string, error)         { return "sp", nil }
func (fakeJoinTM) ReleaseSavepoint(context.Context, string, string) error    { return nil }
func (fakeJoinTM) RollbackToSavepoint(context.Context, string, string) error { return nil }
func (fakeJoinTM) LostRace(context.Context, string) (bool, error)            { return false, nil }

// make32 returns a deterministic 32-byte HMAC secret for tests.
func make32(t *testing.T) []byte {
	t.Helper()
	return []byte("test-secret-key-at-least-32byte!")
}

// liveCoord returns a coordination on which calloutID is in progress on txID
// at major 1, and the pass claims that name it.
func liveCoord(t *testing.T, calloutID, txID string) (txcoord.Coordination, token.Claims) {
	t.Helper()
	c := txcoord.New(fakeJoinTM{})
	_, end := c.BeginCallout(context.Background(), txID, calloutID)
	t.Cleanup(end)
	c.Advance(calloutID, 1)
	return c, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
}

// joinerOver builds the Joiner the middleware takes, over signer, txMgr and
// the coordination c. A nil meter (the no-op meter).
func joinerOver(t *testing.T, s *token.Signer, txMgr spi.TransactionManager, c txcoord.Coordination) *txjoin.Joiner {
	t.Helper()
	return joinerCapped(t, s, txMgr, c, testMaxWaiters)
}

// joinerCapped is joinerOver with the queue cap the test wants to reach.
func joinerCapped(t *testing.T, s *token.Signer, txMgr spi.TransactionManager, c txcoord.Coordination, maxWaiters int) *txjoin.Joiner {
	t.Helper()
	j, err := txjoin.NewJoiner(s, txMgr, c, testResponseMax, maxWaiters, nil)
	if err != nil {
		t.Fatalf("NewJoiner: %v", err)
	}
	return j
}

// noCalloutJoiner returns a Joiner over a coordination that knows no callout:
// every pass it is given is refused.
func noCalloutJoiner(t *testing.T, s *token.Signer, txMgr spi.TransactionManager) *txjoin.Joiner {
	t.Helper()
	return joinerOver(t, s, txMgr, txcoord.New(fakeJoinTM{}))
}

// liveJoiner returns a Joiner whose coordination has the callout of txID in
// progress at major 1, that coordination, and the pass that names it.
func liveJoiner(t *testing.T, txID string) (*txjoin.Joiner, txcoord.Coordination, string) {
	t.Helper()
	return liveJoinerCapped(t, txID, testMaxWaiters)
}

// liveJoinerCapped is liveJoiner with the queue cap the test wants to reach.
func liveJoinerCapped(t *testing.T, txID string, maxWaiters int) (*txjoin.Joiner, txcoord.Coordination, string) {
	t.Helper()
	s, err := token.NewSigner(make32(t))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	c, claims := liveCoord(t, "req-"+txID, txID)
	pass, err := s.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return joinerCapped(t, s, fakeJoinTM{}, c, maxWaiters), c, pass
}

// routed serves next behind mw on every route the join layer classifies, the
// way the router does in app: the route it picks is what the request's class
// is read from.
func routed(mw func(http.Handler) http.Handler, next http.Handler) http.Handler {
	r := chi.NewRouter()
	h := mw(next)
	rec := &recordingMux{}
	genapi.HandlerWithOptions(nil, genapi.StdHTTPServerOptions{BaseRouter: rec})
	for _, route := range rec.routes {
		method, pattern, _ := strings.Cut(route, " ")
		r.Method(method, pattern, h)
	}
	return r
}

// gateFree reports whether the transaction's gate is free: a request of the
// live callout of txID (major 1) takes it within 50ms. A request refused after
// it took the gate (a marked transaction) also reads as free; only a request
// still queued when its wait ends reads as held.
func gateFree(c txcoord.Coordination, txID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s, _, err := c.Enter(ctx, txID, []txcoord.Pair{{Callout: "req-" + txID, Major: 1}}, txcoord.Outside, 0)
	if err != nil {
		return !errors.Is(err, common.ErrClientGone)
	}
	s.End(nil)
	return true
}

// holdGate keeps the transaction's gate, as a request of the live callout of
// txID in progress, until the returned func is called.
func holdGate(t *testing.T, c txcoord.Coordination, txID string) func() {
	t.Helper()
	s, _, err := c.Enter(context.Background(), txID, []txcoord.Pair{{Callout: "req-" + txID, Major: 1}}, txcoord.Outside, 0)
	if err != nil {
		t.Fatalf("holdGate: %v", err)
	}
	var once sync.Once
	return func() { once.Do(func() { s.End(nil) }) }
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
	f, claims := liveCoord(t, "req-tx-1", "tx-1")
	tok, _ := s.Issue(claims)
	var sawTx string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tx := spi.GetTransaction(r.Context()); tx != nil {
			sawTx = tx.ID
		}
		w.WriteHeader(200)
	})
	h := routed(TxJoin(joinerOver(t, s, fakeJoinTM{}, f)), next)
	req := httptest.NewRequest("POST", "/entity/JSON", nil)
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
	h := routed(TxJoin(noCalloutJoiner(t, s, fakeJoinTM{joinErr: spi.ErrTxNotFound})), okHandler())
	req := httptest.NewRequest("POST", "/entity/JSON", nil)
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
	h := routed(TxJoin(noCalloutJoiner(t, s, fakeJoinTM{})), okHandler())
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
	h := routed(TxJoin(noCalloutJoiner(t, s, fakeJoinTM{})), okHandler())
	req := httptest.NewRequest("POST", "/entity/JSON", nil)
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
	f := txcoord.New(fakeJoinTM{})
	_, end := f.BeginCallout(context.Background(), "tx-1", "req-1")
	f.Advance("req-1", 1)
	end()
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})

	for _, method := range []string{http.MethodGet, http.MethodPost} { // a read and a write
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })
		req := withUserCtx(httptest.NewRequest(method, "/entity/x", nil))
		req.Header.Set(proxy.TxTokenHeader, tok)
		rec := httptest.NewRecorder()
		routed(TxJoin(joinerOver(t, s, fakeJoinTM{}, f)), next).ServeHTTP(rec, req)
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
	for name, setup := range map[string]func(*testing.T) (txcoord.Coordination, token.Claims){
		"callout current": func(t *testing.T) (txcoord.Coordination, token.Claims) {
			return liveCoord(t, "req-1", "tx-1")
		},
		"callout superseded": func(t *testing.T) (txcoord.Coordination, token.Claims) {
			f, claims := liveCoord(t, "req-1", "tx-1")
			f.Advance("req-1", 2)
			return f, claims
		},
		"callout unknown": func(t *testing.T) (txcoord.Coordination, token.Claims) {
			return txcoord.New(fakeJoinTM{}), token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := token.NewSigner(make32(t))
			f, claims := setup(t)
			tok, _ := s.Issue(claims)
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })
			req := withUserCtx(httptest.NewRequest(http.MethodGet, "/entity/x", nil))
			req.Header.Set(proxy.TxTokenHeader, tok)
			rec := httptest.NewRecorder()
			routed(TxJoin(joinerOver(t, s, fakeJoinTM{joinErr: spi.ErrTxTenantMismatch}, f)), next).ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want 403", rec.Code)
			}
			if code := problemProps(t, rec).ErrorCode; code != "FORBIDDEN" {
				t.Fatalf("errorCode = %q; want FORBIDDEN", code)
			}
		})
	}
}

// The response is sent after the gate is released: a cnode that does not read
// its response does not hold the transaction.
func TestTxJoin_ResponseIsSentAfterTheGateIsReleased(t *testing.T) {
	j, c, pass := liveJoiner(t, "tx-1")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	w := &lockProbeWriter{ResponseRecorder: httptest.NewRecorder(), coord: c, txID: "tx-1"}
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity/JSON", strings.NewReader(`{}`)))
	req.Header.Set(proxy.TxTokenHeader, pass)
	routed(TxJoin(j), next).ServeHTTP(w, req)

	if w.Code != http.StatusCreated || w.Body.String() != `{"ok":true}` || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w.writes == 0 || w.writesUnderLock != 0 {
		t.Fatalf("%d of %d writes to the client were made while the transaction's gate was held", w.writesUnderLock, w.writes)
	}
}

// lockProbeWriter notes, on every write to the client, whether the
// transaction's gate is free.
type lockProbeWriter struct {
	*httptest.ResponseRecorder
	coord                   txcoord.Coordination
	txID                    string
	writes, writesUnderLock int
}

func (w *lockProbeWriter) probe() {
	w.writes++
	if !gateFree(w.coord, w.txID) {
		w.writesUnderLock++
	}
}
func (w *lockProbeWriter) WriteHeader(code int)        { w.probe(); w.ResponseRecorder.WriteHeader(code) }
func (w *lockProbeWriter) Write(b []byte) (int, error) { w.probe(); return w.ResponseRecorder.Write(b) }

// Every route behind the middleware runs under the gate, whatever it serves —
// model, message, search, statistics, audit are all just `next` to it — and is
// refused once its cnode is replaced.
func TestTxJoin_EveryRouteRunsUnderTheGate(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodGet, "/model/export/SIMPLE_VIEW/x/1"},
		{http.MethodPost, "/message/new/s"},
		{http.MethodPost, "/search/direct/x/1"},
		{http.MethodGet, "/entity/stats"},
		{http.MethodGet, "/audit/entity/x"},
	}

	// The same routes, once the cnode that held the callout has been replaced.
	s, _ := token.NewSigner(make32(t))
	replaced, _ := liveCoord(t, "req-1", "tx-1")
	stale, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1",
		ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})
	replaced.Advance("req-1", 2)
	for _, route := range routes {
		req := withUserCtx(httptest.NewRequest(route.method, route.path, nil))
		req.Header.Set(proxy.TxTokenHeader, stale)
		rec := httptest.NewRecorder()
		routed(TxJoin(joinerOver(t, s, fakeJoinTM{}, replaced)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatalf("%s: handler must not run", route.path) })).ServeHTTP(rec, req)
		if rec.Code != http.StatusGone || problemProps(t, rec).ErrorCode != "CALLOUT_SUPERSEDED" {
			t.Fatalf("%s: status = %d, problem = %+v; want 410 CALLOUT_SUPERSEDED", route.path, rec.Code, problemProps(t, rec))
		}
	}

	j, c, pass := liveJoiner(t, "tx-1")
	for _, route := range routes {
		held := false
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { held = !gateFree(c, "tx-1") })
		req := withUserCtx(httptest.NewRequest(route.method, route.path, nil))
		req.Header.Set(proxy.TxTokenHeader, pass)
		routed(TxJoin(j), next).ServeHTTP(httptest.NewRecorder(), req)
		if !held {
			t.Fatalf("%s ran without the transaction's gate", route.path)
		}
	}
}

// The request body is read before the gate is taken: a cnode that trickles its
// body does not hold the transaction.
func TestTxJoin_BodyIsReadBeforeTheGateIsTaken(t *testing.T) {
	j, c, pass := liveJoiner(t, "tx-1")
	body := newLockProbeBody(strings.NewReader(`{"a":1}`), c, "tx-1")
	var got string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
	})
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity/JSON", body))
	req.Header.Set(proxy.TxTokenHeader, pass)
	routed(TxJoin(j), next).ServeHTTP(httptest.NewRecorder(), req)
	if got != `{"a":1}` {
		t.Fatalf("handler read %q", got)
	}
	if body.reads == 0 || body.readsUnderLock != 0 {
		t.Fatalf("%d of %d reads of the client's body were made under the transaction's gate", body.readsUnderLock, body.reads)
	}
}

// lockProbeBody mirrors lockProbeWriter on Read: it notes, on every read of the
// client's body, whether the transaction's gate is free. reading is closed on the
// FIRST read, so a test waiting for the middleware to be inside the body read
// waits for that rather than guessing how long it takes.
type lockProbeBody struct {
	io.Reader
	coord                 txcoord.Coordination
	txID                  string
	reading               chan struct{}
	readingOnce           sync.Once
	reads, readsUnderLock int
}

func newLockProbeBody(r io.Reader, c txcoord.Coordination, txID string) *lockProbeBody {
	return &lockProbeBody{Reader: r, coord: c, txID: txID, reading: make(chan struct{})}
}

func (b *lockProbeBody) Read(p []byte) (int, error) {
	b.readingOnce.Do(func() { close(b.reading) })
	b.reads++
	if !gateFree(b.coord, b.txID) {
		b.readsUnderLock++
	}
	return b.Reader.Read(p)
}

// A joined request whose client sends its headers and then stalls does not
// hold the transaction's gate: another joined request on the same transaction
// completes meanwhile.
func TestTxJoin_StalledBodyDoesNotHoldTheGate(t *testing.T) {
	j, c, pass := liveJoiner(t, "tx-1")
	mw := TxJoin(j)

	stalled, unblock := io.Pipe() // headers sent, body never arrives
	body := newLockProbeBody(stalled, c, "tx-1")
	stalledDone := make(chan struct{})
	go func() {
		defer close(stalledDone)
		req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity/JSON", body))
		req.Header.Set(proxy.TxTokenHeader, pass)
		routed(mw, okHandler()).ServeHTTP(httptest.NewRecorder(), req)
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
		routed(mw, okHandler()).ServeHTTP(rec, req)
		otherDone <- rec.Code
	}()
	select {
	case code := <-otherDone:
		if code != http.StatusOK {
			t.Fatalf("the other joined request = %d; want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled body held the transaction's gate: the other joined request did not complete")
	}
	_ = unblock.Close()
	<-stalledDone
}

// countingBody is a request body that records how often it was read, and is
// empty: a door that reads it at all is reading it too early.
type countingBody struct{ reads int }

func (b *countingBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }

// A pass this door refuses on the pass alone costs it no buffer: the signature,
// the shape and the expiry are judged before a byte of the body is read.
func TestTxJoin_BadPassIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	other, _ := token.NewSigner([]byte("different-secret-key-at-least-32b!"))
	forged, _ := other.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-1", Major: 1})
	expired, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(-time.Second).Unix(), Callout: "req-tx-1", Major: 1})
	shapeless, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})

	for name, tc := range map[string]struct {
		tok    string
		status int
		code   string
	}{
		"forged":            {forged, http.StatusUnauthorized, "UNAUTHORIZED"},
		"expired":           {expired, http.StatusGone, "TRANSACTION_EXPIRED"},
		"naming no callout": {shapeless, http.StatusUnauthorized, "UNAUTHORIZED"},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := liveCoord(t, "req-tx-1", "tx-1")
			body := &countingBody{}
			req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity/JSON", body))
			req.Header.Set(proxy.TxTokenHeader, tc.tok)
			rec := httptest.NewRecorder()

			routed(TxJoin(joinerOver(t, s, fakeJoinTM{}, f)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })).ServeHTTP(rec, req)

			if rec.Code != tc.status || problemProps(t, rec).ErrorCode != tc.code {
				t.Fatalf("status = %d, problem = %+v; want %d %s", rec.Code, problemProps(t, rec), tc.status, tc.code)
			}
			if body.reads != 0 {
				t.Errorf("the body was read %d times for a pass that was refused", body.reads)
			}
		})
	}
}

// A compute node that goes away while its request queues for the transaction's
// lock: the handler never runs, the door returns at once — and it still answers.
// Leaving without writing would have net/http answer an implicit 200 for a
// request that did nothing, which a later middleware putting a deadline on the
// request context would turn into a lie about a write. Nothing was wrong with
// the server and there is nobody to quote a ticket to, so this is logged at
// DEBUG with no ticket — never the ERROR-per-queued-callback that a failing-over
// compute member would otherwise produce right when an operator most wants a
// clean log.
func TestTxJoin_ClientGoesAwayWhileQueued_HandlerNeverRuns_NeverAnImplicit200(t *testing.T) {
	buf := captureSlog(t)
	j, c, pass := liveJoiner(t, "tx-1")
	holder := holdGate(t, c, "tx-1") // another request of the same compute node
	defer holder()

	ran := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true })
	req := httptest.NewRequest(http.MethodPost, "/entity/JSON", strings.NewReader(`{}`))
	ctx, disconnect := context.WithCancel(req.Context())
	defer disconnect()
	req = withUserCtx(req.WithContext(ctx))
	req.Header.Set(proxy.TxTokenHeader, pass)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		routed(TxJoin(j), next).ServeHTTP(rec, req)
	}()
	waitForCapacity(t, c, "tx-1", 1) // verified and read; now queued for the gate
	disconnect()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the door stayed parked on the gate after its client went away")
	}
	if ran {
		t.Fatal("the handler ran for a client that had gone")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want the same 500 an aborted request gets today — never an implicit 200", rec.Code)
	}
	if code := problemProps(t, rec).ErrorCode; code != "SERVER_ERROR" {
		t.Errorf("errorCode = %q; want SERVER_ERROR", code)
	}
	var pd map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("body: %v", err)
	}
	if pd["ticket"] != nil {
		t.Errorf("a client disconnect must not carry a ticket nobody can be quoted: %v", pd["ticket"])
	}
	if strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Errorf("a client disconnect must not log at ERROR: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"level":"DEBUG"`) {
		t.Errorf("a client disconnect must log at DEBUG: %s", buf.String())
	}
}

// A join that FAILS with an internal error which merely wraps a cancellation is
// not a departed client, even when the client has in fact also gone: the two
// are different events, and only the coordination's own "the request ended
// while it queued for the transaction's gate" is the second. Filing this as a departed
// client would log a genuine fault at DEBUG with no ticket, below the default
// level — the fault would leave no trace at all.
func TestTxJoin_JoinFailureWrappingACancellation_KeepsItsTicket(t *testing.T) {
	buf := captureSlog(t)
	s, _ := token.NewSigner(make32(t))
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-tx-1", Major: 1})
	joinFailed := fmt.Errorf("resolve transaction: %w", context.Canceled)
	h := routed(TxJoin(noCalloutJoiner(t, s, fakeJoinTM{joinErr: joinFailed})),
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the handler of a failed join ran") }))

	req := httptest.NewRequest(http.MethodPost, "/entity/JSON", strings.NewReader(`{}`))
	ctx, disconnect := context.WithCancel(req.Context())
	disconnect() // the client has gone too — but that is not what went wrong
	req = withUserCtx(req.WithContext(ctx))
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	var pd map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("body: %v", err)
	}
	if pd["ticket"] == nil || pd["ticket"] == "" {
		t.Error("a genuine fault must carry a ticket the caller can quote")
	}
	if !strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Errorf("a genuine fault must log at ERROR: %s", buf.String())
	}
}

// A body over the join layer's limit is refused before any lock is taken.
func TestTxJoin_OversizeBody_413(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity/JSON", io.LimitReader(zeroes{}, maxJoinedBodySize+1)))
	req.Header.Set(proxy.TxTokenHeader, pass)
	rec := httptest.NewRecorder()
	routed(TxJoin(j), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", rec.Code)
	}
}

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// Callbacks of one transaction are served one at a time, and the queue behind
// the one being served is bounded. Past the cap the callback is refused with a
// retryable 503 — and refused before its body is read, so a member firing
// callbacks at one transaction costs the node no buffer per refusal. The
// callback holding the transaction and the ones already queued are unaffected.
func TestTxJoin_PastTheWaiterCap_503_BeforeTheBodyIsRead(t *testing.T) {
	j, c, pass := liveJoinerCapped(t, "tx-1", 1)
	holder := holdGate(t, c, "tx-1") // a callback already has the transaction
	// Released explicitly below; the defer is so that a failing assertion does
	// not leave the queued callback parked and hang the package.
	defer holder()

	queued := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		routed(TxJoin(j), okHandler()).ServeHTTP(rec, joinedRequest(t, pass, nil))
		queued <- rec.Code
	}()
	waitForCapacity(t, c, "tx-1", 1)

	var bodyRead bool
	rec := httptest.NewRecorder()
	req := joinedRequest(t, pass, readMarker{read: &bodyRead})
	routed(TxJoin(j), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the refused callback's handler ran")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rec.Code)
	}
	props := problemProps(t, rec)
	if props.ErrorCode != "TOO_MANY_JOINED_REQUESTS" {
		t.Errorf("errorCode = %q; want TOO_MANY_JOINED_REQUESTS", props.ErrorCode)
	}
	if !props.Retryable {
		t.Error("the refusal is not retryable; the queue drains, so it is")
	}
	if bodyRead {
		t.Error("the body was read: a callback refused for capacity must cost no buffer")
	}

	holder()
	select {
	case code := <-queued:
		if code != http.StatusOK {
			t.Fatalf("the callback already queued answered %d; want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the callback already queued never ran")
	}
}

// joinedRequest builds a request carrying pass, with body as its body.
func joinedRequest(t *testing.T, pass string, body io.Reader) *http.Request {
	t.Helper()
	req := withUserCtx(httptest.NewRequest(http.MethodPost, "/entity/JSON", body))
	req.Header.Set(proxy.TxTokenHeader, pass)
	return req
}

// readMarker records whether anything read it.
type readMarker struct{ read *bool }

func (m readMarker) Read(p []byte) (int, error) {
	*m.read = true
	return 0, io.EOF
}

// waitForCapacity waits until maxWaiters callers are queued for txID.
func waitForCapacity(t *testing.T, c txcoord.Coordination, txID string, maxWaiters int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.AtCapacity(txID, maxWaiters) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d callers to queue for %s", maxWaiters, txID)
}

// administrationRoutes is every state-changing model and workflow
// administration route, with a method and path a client sends.
var administrationRoutes = []struct{ method, path string }{
	{http.MethodPost, "/model/import/JSON/SAMPLE_DATA/order/1"},
	{http.MethodDelete, "/model/order/1"},
	{http.MethodPost, "/model/order/1/changeLevel/STRUCTURAL"},
	{http.MethodPut, "/model/order/1/lock"},
	{http.MethodPut, "/model/order/1/unlock"},
	{http.MethodPut, "/model/order/1/unique-keys"},
	{http.MethodPost, "/model/order/1/workflow/import"},
}

// Model and workflow administration never runs inside a transaction: a
// request for one of those routes that carries a transaction token is refused
// 400 MODEL_ADMIN_IN_JOINED_TRANSACTION before the token is verified — so
// before any transaction gate is taken — and the handler never runs. The
// token here is tampered: were it verified first, the answer would be 401.
func TestTxJoin_Administration_RefusedBeforeVerify(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	s2, _ := token.NewSigner([]byte("different-secret-key-at-least-32b!"))
	tok, _ := s2.Issue(token.Claims{NodeID: "local", TxRef: "tx-adm", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-adm", Major: 1})
	for _, route := range administrationRoutes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })
			req := withUserCtx(httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`)))
			req.Header.Set(proxy.TxTokenHeader, tok)
			rec := httptest.NewRecorder()
			routed(TxJoin(noCalloutJoiner(t, s, fakeJoinTM{})), next).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400: %s", rec.Code, rec.Body.String())
			}
			props := problemProps(t, rec)
			if props.ErrorCode != "MODEL_ADMIN_IN_JOINED_TRANSACTION" || props.Retryable {
				t.Fatalf("problem = %+v", props)
			}
			if !strings.Contains(rec.Body.String(), "cannot run inside a transaction") {
				t.Fatalf("body = %s; want the reason", rec.Body.String())
			}
		})
	}
}

// The same routes without a token pass through to the handler, and the
// read-only model and workflow routes join a transaction like any other
// request: the refusal is keyed to the route, not to the /model prefix.
func TestTxJoin_Administration_NoTokenPassesAndReadsJoin(t *testing.T) {
	s, _ := token.NewSigner(make32(t))
	for _, route := range administrationRoutes {
		ran := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ran = true; w.WriteHeader(200) })
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		routed(TxJoin(noCalloutJoiner(t, s, fakeJoinTM{})), next).ServeHTTP(rec, req)
		if !ran || rec.Code != http.StatusOK {
			t.Fatalf("%s %s without a token: ran=%v status=%d; want the handler to run", route.method, route.path, ran, rec.Code)
		}
	}

	f, claims := liveCoord(t, "req-read", "tx-read")
	tok, _ := s.Issue(claims)
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/model/"},
		{http.MethodGet, "/model/export/SIMPLE_VIEW/order/1"},
		{http.MethodPost, "/model/validate/order/1"},
		{http.MethodGet, "/model/order/1/workflow/export"},
		{http.MethodPost, "/entity/JSON/order/1"},
	} {
		var sawTx string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tx := spi.GetTransaction(r.Context()); tx != nil {
				sawTx = tx.ID
			}
			w.WriteHeader(200)
		})
		req := withUserCtx(httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`)))
		req.Header.Set(proxy.TxTokenHeader, tok)
		rec := httptest.NewRecorder()
		routed(TxJoin(joinerOver(t, s, fakeJoinTM{}, f)), next).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || sawTx != "tx-read" {
			t.Fatalf("%s %s with a token: status=%d joined=%q; want 200 joined to tx-read", route.method, route.path, rec.Code, sawTx)
		}
	}
}

// When the scope's end rewrites the answer — here the callout moved on while
// the handler ran — the door sends the rewritten error and drops what the
// handler wrote: the compute node must not be told its work was kept.
func TestTxJoin_RewriteDropsTheHandlersAnswer(t *testing.T) {
	j, c, pass := liveJoiner(t, "tx-1")
	advanced := make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		go func() { defer close(advanced); c.Advance("req-tx-1", 2) }()
		deadline := time.Now().Add(5 * time.Second)
		for !errors.Is(txcoord.Check(r.Context()), txcoord.ErrSuperseded) {
			if time.Now().After(deadline) {
				t.Error("the pass never read superseded")
				break
			}
			time.Sleep(time.Millisecond)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kept":true}`))
	})
	rec := httptest.NewRecorder()
	routed(TxJoin(j), next).ServeHTTP(rec, joinedRequest(t, pass, nil))
	<-advanced
	if rec.Code != http.StatusGone || problemProps(t, rec).ErrorCode != "CALLOUT_SUPERSEDED" {
		t.Fatalf("answer = %d %+v; want 410 CALLOUT_SUPERSEDED", rec.Code, problemProps(t, rec))
	}
	if strings.Contains(rec.Body.String(), "kept") {
		t.Fatalf("the handler's answer was sent: %s", rec.Body.String())
	}
}

// A joined write the handler answers with a non-retryable failure, through the
// shared error funnel, is discarded and makes the transaction rollback-only: a
// later joined request is refused 409 TRANSACTION_ROLLBACK_ONLY, and the
// failed request itself gets its own answer.
func TestTxJoin_FailedWriteMakesTheTransactionRollbackOnly(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")
	failing := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, "the processor failed"))
	})
	rec := httptest.NewRecorder()
	routed(TxJoin(j), failing).ServeHTTP(rec, joinedRequest(t, pass, nil))
	if rec.Code != http.StatusBadRequest || problemProps(t, rec).ErrorCode != "WORKFLOW_FAILED" {
		t.Fatalf("the failed write = %d %+v; want its own 400 WORKFLOW_FAILED", rec.Code, problemProps(t, rec))
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := withUserCtx(httptest.NewRequest(method, "/entity/JSON", nil))
		if method == http.MethodGet {
			req = withUserCtx(httptest.NewRequest(method, "/entity/x1", nil))
		}
		req.Header.Set(proxy.TxTokenHeader, pass)
		rec := httptest.NewRecorder()
		routed(TxJoin(j), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a request under a marked scope ran") })).ServeHTTP(rec, req)
		props := problemProps(t, rec)
		if rec.Code != http.StatusConflict || props.ErrorCode != "TRANSACTION_ROLLBACK_ONLY" || props.Retryable {
			t.Fatalf("%s after a failed write = %d %+v; want 409 TRANSACTION_ROLLBACK_ONLY, not retryable", method, rec.Code, props)
		}
	}
}

// A handler that started a 2xx answer and then failed through the error
// funnel has its failure recorded: the scope ends as a failed request, so the
// door sends the recorded error, never the 2xx it had buffered.
func TestTxJoin_FailureRecordedAfterA2xxHeader_SendsTheError(t *testing.T) {
	j, _, pass := liveJoiner(t, "tx-1")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":true}`))
		common.WriteError(w, r, common.Operational(http.StatusBadRequest, common.ErrCodeWorkflowFailed, "failed after the header"))
	})
	rec := httptest.NewRecorder()
	routed(TxJoin(j), next).ServeHTTP(rec, joinedRequest(t, pass, nil))
	if rec.Code != http.StatusBadRequest || problemProps(t, rec).ErrorCode != "WORKFLOW_FAILED" {
		t.Fatalf("answer = %d %s; want the recorded 400 WORKFLOW_FAILED", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "partial") {
		t.Fatalf("the buffered 2xx body was sent: %s", rec.Body.String())
	}
}

// The recorded failure the door sends is the one the handler answered: one log
// line, and the ticket the client is given is the one it was logged under.
func TestTxJoin_FailureRecordedAfterA2xxHeader_LoggedOnce(t *testing.T) {
	logs := captureSlog(t)
	j, _, pass := liveJoiner(t, "tx-1")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		common.WriteError(w, r, common.Internal("failed after the header", errors.New("boom")))
	})
	rec := httptest.NewRecorder()
	routed(TxJoin(j), next).ServeHTTP(rec, joinedRequest(t, pass, nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("answer = %d %s; want the recorded 500", rec.Code, rec.Body.String())
	}
	var pd struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil || pd.Ticket == "" {
		t.Fatalf("answer carries no ticket (%v): %s", err, rec.Body.String())
	}
	if n := strings.Count(logs.String(), `"msg":"internal error"`); n != 1 {
		t.Fatalf("%d internal error log lines; want 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), `"ticket":"`+pd.Ticket+`"`) {
		t.Fatalf("the ticket answered (%s) is not the one logged:\n%s", pd.Ticket, logs.String())
	}
}
