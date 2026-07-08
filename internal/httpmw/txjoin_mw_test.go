package httpmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
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
	tok, _ := s.Issue("local", "tx-1", time.Now().Add(time.Minute))
	var sawTx string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tx := spi.GetTransaction(r.Context()); tx != nil {
			sawTx = tx.ID
		}
		w.WriteHeader(200)
	})
	h := TxJoin(s, fakeJoinTM{})(next)
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
	tok, _ := s.Issue("local", "tx-x", time.Now().Add(time.Minute))
	h := TxJoin(s, fakeJoinTM{joinErr: spi.ErrTxNotFound})(okHandler())
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
	h := TxJoin(s, fakeJoinTM{})(okHandler())
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
	tok, _ := s2.Issue("local", "tx-bad", time.Now().Add(time.Minute))
	h := TxJoin(s, fakeJoinTM{})(okHandler())
	req := httptest.NewRequest("POST", "/entity", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	req = withUserCtx(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
