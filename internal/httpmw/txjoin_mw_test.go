package httpmw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
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

// noCalloutFence returns a fence that knows no callout: every pass is refused.
func noCalloutFence() *fence.Fence { return fence.New(txgate.New()) }

// liveFence returns a fence on which calloutID is in progress on txID at
// major 1, and the pass claims that name it.
func liveFence(t *testing.T, calloutID, txID string) (*fence.Fence, token.Claims) {
	t.Helper()
	f := fence.New(txgate.New())
	_, end := f.Begin(context.Background(), calloutID, txID, nil)
	t.Cleanup(end)
	f.Advance(calloutID, 1)
	return f, token.Claims{NodeID: "local", TxRef: txID, ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: calloutID, Major: 1}
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
	f, claims := liveFence(t, "req-tx-1", "tx-1")
	tok, _ := s.Issue(claims)
	var sawTx string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tx := spi.GetTransaction(r.Context()); tx != nil {
			sawTx = tx.ID
		}
		w.WriteHeader(200)
	})
	h := TxJoin(s, fakeJoinTM{}, f)(next)
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
	h := TxJoin(s, fakeJoinTM{joinErr: spi.ErrTxNotFound}, noCalloutFence())(okHandler())
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
	h := TxJoin(s, fakeJoinTM{}, noCalloutFence())(okHandler())
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
	h := TxJoin(s, fakeJoinTM{}, noCalloutFence())(okHandler())
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
	f := fence.New(txgate.New())
	_, end := f.Begin(context.Background(), "req-1", "tx-1", nil)
	f.Advance("req-1", 1)
	end()
	tok, _ := s.Issue(token.Claims{NodeID: "local", TxRef: "tx-1", ExpiresAt: time.Now().Add(time.Minute).Unix(), Callout: "req-1", Major: 1})

	for _, method := range []string{http.MethodGet, http.MethodPost} { // a read and a write
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") })
		req := withUserCtx(httptest.NewRequest(method, "/entity/x", nil))
		req.Header.Set(proxy.TxTokenHeader, tok)
		rec := httptest.NewRecorder()
		TxJoin(s, fakeJoinTM{}, f)(next).ServeHTTP(rec, req)
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
	for name, setup := range map[string]func(*testing.T) (*fence.Fence, token.Claims){
		"callout current": func(t *testing.T) (*fence.Fence, token.Claims) {
			return liveFence(t, "req-1", "tx-1")
		},
		"callout superseded": func(t *testing.T) (*fence.Fence, token.Claims) {
			f, claims := liveFence(t, "req-1", "tx-1")
			f.Advance("req-1", 2)
			return f, claims
		},
		"callout unknown": func(t *testing.T) (*fence.Fence, token.Claims) {
			_, claims := liveFence(t, "req-1", "tx-1")
			return noCalloutFence(), claims
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
			TxJoin(s, fakeJoinTM{joinErr: spi.ErrTxTenantMismatch}, f)(next).ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want 403", rec.Code)
			}
			if code := problemProps(t, rec).ErrorCode; code != "FORBIDDEN" {
				t.Fatalf("errorCode = %q; want FORBIDDEN", code)
			}
		})
	}
}
