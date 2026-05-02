package proxy_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
)

// fakeRegistry is a test double for contract.NodeRegistry that supports multiple
// nodes with configurable alive status.
type fakeRegistry struct {
	nodes map[string]contract.NodeInfo
}

func newFakeRegistry(nodes ...contract.NodeInfo) *fakeRegistry {
	m := make(map[string]contract.NodeInfo, len(nodes))
	for _, n := range nodes {
		m[n.NodeID] = n
	}
	return &fakeRegistry{nodes: m}
}

func (r *fakeRegistry) Register(_ context.Context, nodeID, addr string) error {
	r.nodes[nodeID] = contract.NodeInfo{NodeID: nodeID, Addr: addr, Alive: true}
	return nil
}

func (r *fakeRegistry) Lookup(_ context.Context, nodeID string) (string, bool, error) {
	info, ok := r.nodes[nodeID]
	if !ok {
		return "", false, nil
	}
	return info.Addr, info.Alive, nil
}

func (r *fakeRegistry) List(_ context.Context) ([]contract.NodeInfo, error) {
	out := make([]contract.NodeInfo, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n)
	}
	return out, nil
}

func (r *fakeRegistry) Deregister(_ context.Context, nodeID string) error {
	delete(r.nodes, nodeID)
	return nil
}

// mustNewSigner creates a token.Signer or panics — for use in tests only.
func mustNewSigner(secret []byte) *token.Signer {
	s, err := token.NewSigner(secret)
	if err != nil {
		panic(fmt.Sprintf("mustNewSigner: %v", err))
	}
	return s
}

// localHandler returns a handler that writes "local" to the response body.
func localHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "local")
	})
}

func TestHTTPProxy_NoToken_ServesLocally(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true})

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second)
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "local" {
		t.Fatalf("expected 'local', got %q", rec.Body.String())
	}
}

func TestHTTPProxy_TokenForSelf_ServesLocally(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true})

	tok, err := signer.Issue("node-1", "tx-123", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second)
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "local" {
		t.Fatalf("expected 'local', got %q", rec.Body.String())
	}
}

func TestHTTPProxy_TokenForOtherNode_Proxies(t *testing.T) {
	// Start a fake remote node that responds with "remote".
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "remote")
	}))
	defer remote.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: remote.URL, Alive: true},
	)

	tok, err := signer.Issue("node-2", "tx-456", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second)
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "remote" {
		t.Fatalf("expected 'remote', got %q", body)
	}
}

func TestHTTPProxy_TokenForDeadNode_Returns503(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: "http://localhost:9998", Alive: false},
	)

	tok, err := signer.Issue("node-2", "tx-789", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second)
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHTTPProxy_ExpiredToken_Returns400(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true})

	// Issue a token that expired in the past.
	tok, err := signer.Issue("node-2", "tx-expired", time.Now().Add(-1*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second)
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// TestHTTPProxy_RoundTrip_SingleACAO verifies that when a request with an
// Origin header is proxied from node A to a peer (node B) that itself emits
// Access-Control-Allow-Origin, the final response contains exactly one
// Access-Control-Allow-Origin value. The proxy's director strips Origin from
// the outbound request so the peer never fires its own CORS logic, meaning
// the ACAO seen by the browser is exactly the one set by node A's outermost
// CORS middleware — not a double-valued header that browsers reject.
//
// Placed in http_test.go (black-box package proxy_test) rather than
// director_test.go because it exercises the full HTTPRouting middleware
// surface including the httputil.ReverseProxy round-trip, not just the
// director helper in isolation. The scaffolding (fakeRegistry, mustNewSigner,
// httptest.NewServer) already exists here and maps cleanly onto the scenario.
func TestHTTPProxy_RoundTrip_SingleACAO(t *testing.T) {
	// Fake peer (node B) that emits ACAO as if it ran its own CORS middleware.
	// In production this would never fire because the director strips Origin,
	// but a misbehaving or misconfigured peer might still emit this header.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://emitted-by-peer.example")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "peer-body")
	}))
	defer peer.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: peer.URL, Alive: true},
	)

	tok, err := signer.Issue("node-2", "tx-acao-roundtrip", time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second)
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	req.Header.Set("Origin", "https://browser.example")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Browsers reject responses with more than one Access-Control-Allow-Origin
	// value. The proxy must not create a double-header by letting node B's ACAO
	// pass through alongside any ACAO set by node A's middleware.
	vals := rec.Header().Values("Access-Control-Allow-Origin")
	if len(vals) > 1 {
		t.Errorf("got %d Access-Control-Allow-Origin values %v, want at most 1 (browsers reject multi-valued ACAO)", len(vals), vals)
	}
}
