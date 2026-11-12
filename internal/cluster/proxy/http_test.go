package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/api/middleware"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/proxy"
	"github.com/cyoda-platform/cyoda-go/internal/cluster/token"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/contract"
	"github.com/cyoda-platform/cyoda-go/internal/health"
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

func (r *fakeRegistry) Changed() <-chan struct{} { return nil }

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

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
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

	tok, err := signer.Issue(token.Claims{NodeID: "node-1", TxRef: "tx-123", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-123", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
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

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-456", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-456", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
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

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-789", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-789", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

// A token for a node that is not live is one log line, at WARN, naming the
// node: the refusal's own line, not a WARN and an INFO.
func TestHTTPProxy_TokenForDeadNode_IsOneLogLine(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: "http://localhost:9998", Alive: false},
	)
	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-790", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-790", Major: 1})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	handler := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")(localHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "node-2") {
		t.Errorf("log = %q, want one WARN line naming the node", lines)
	}
}

func TestHTTPProxy_ExpiredToken_Returns410(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true})

	// Issue a token that expired in the past.
	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-expired", ExpiresAt: time.Now().Add(-1 * time.Minute).Unix(), Callout: "req-tx-expired", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusGone {
		t.Fatalf("expected 410, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "TRANSACTION_EXPIRED") {
		t.Errorf("expected TRANSACTION_EXPIRED error code, got: %s", body)
	}
}

func TestHTTPProxy_TamperedToken_Returns401(t *testing.T) {
	// Issue a token with one signer and verify with another → tampered.
	signer1 := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	signer2 := mustNewSigner([]byte("other-secret-key-at-least-32-bytes"))
	reg := newFakeRegistry(contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true})

	tok, err := signer2.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-tampered", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-tampered", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer1, reg, "node-1", 5*time.Second, true, "")
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
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

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-acao-roundtrip", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-acao-roundtrip", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
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

// TestHTTPProxy_SSRFGuard_RejectsLoopback verifies that when allowLoopback=false
// the proxy returns 503 TRANSACTION_NODE_UNAVAILABLE rather than forwarding to a
// loopback target, closing the SSRF pivot on the B→A callback path.
func TestHTTPProxy_SSRFGuard_RejectsLoopback(t *testing.T) {
	// Register node-2 with a loopback addr — simulates a maliciously or
	// accidentally injected registry entry pointing at an internal service.
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://10.0.0.1:8080", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: "http://127.0.0.1:9999", Alive: true},
	)

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-ssrf", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-ssrf", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	// allowLoopback=false — production posture.
	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, false, "")
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from SSRF guard, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, common.ErrCodeTransactionNodeUnavailable) {
		t.Errorf("expected %s in body, got: %s", common.ErrCodeTransactionNodeUnavailable, body)
	}
}

// TestHTTPProxy_SSRFGuard_AllowsLoopbackWhenPermitted verifies that
// allowLoopback=true permits loopback targets (test-fixture posture).
func TestHTTPProxy_SSRFGuard_AllowsLoopbackWhenPermitted(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "reached")
	}))
	defer remote.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://10.0.0.1:8080", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: remote.URL, Alive: true},
	)

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-loopback-ok", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-loopback-ok", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	// allowLoopback=true — test-fixture posture.
	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
	handler := mw(localHandler())

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when loopback allowed, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "reached" {
		t.Fatalf("expected 'reached', got %q", body)
	}
}

// TestProxy_PreservesQueryString is a characterization test pinning
// load-bearing behavior for spec D7: transaction-control query params
// (e.g. transactionTimeoutMillis, transactionSize) must reach the executing
// peer verbatim so that peer's own validation can reject them with 400. If
// the proxy's director ever started rewriting or dropping the query string,
// that rejection would silently stop happening. It should pass immediately —
// that is expected and acceptable for a characterization test guarding a
// contract, not driving new behavior.
func TestProxy_PreservesQueryString(t *testing.T) {
	var gotRawQuery string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "remote")
	}))
	defer remote.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: remote.URL, Alive: true},
	)

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-query-string", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-query-string", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	mw := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")
	handler := mw(localHandler())

	const rawQuery = "transactionTimeoutMillis=5000&transactionSize=2&x=%20y"
	req := httptest.NewRequest(http.MethodGet, "/api/test?"+rawQuery, nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if gotRawQuery != rawQuery {
		t.Fatalf("query string not preserved across the forwarded hop: got %q, want %q", gotRawQuery, rawQuery)
	}
}

// An upstream that hangs up mid-body makes ReverseProxy panic with
// http.ErrAbortHandler. Under Recovery that must stay a silent abort: the
// node's health latch is untouched.
func TestHTTPProxy_UpstreamHangupMidBody_DoesNotLatchHealth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // closes the upstream connection short
	}))
	defer upstream.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: upstream.URL, Alive: true},
	)

	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-hangup", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-hangup", Major: 1})
	if err != nil {
		t.Fatal(err)
	}
	healthLatch := health.NewLatch()
	h := middleware.Recovery(healthLatch)(proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")(localHandler()))
	srv := httptest.NewServer(h) // a real server, so the re-raised sentinel reaches net/http
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if healthLatch.Tripped() {
		t.Fatal("a proxied client hang-up latched the node unhealthy")
	}
}

// Model and workflow administration never runs inside a transaction, and the
// refusal comes before this layer verifies or forwards anything: a request for
// an administration route that carries a token is answered 400
// MODEL_ADMIN_IN_JOINED_TRANSACTION here — tampered, valid for this node, or
// valid for a peer alike — and is neither served locally nor proxied. The
// layer sits above the context-path strip, so it matches with the context
// path applied.
func TestHTTPProxy_Administration_RefusedBeforeVerifyOrProxy(t *testing.T) {
	remoteHits := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	other := mustNewSigner([]byte("other-secret-key-at-least-32-bytes"))
	reg := newFakeRegistry(contract.NodeInfo{NodeID: "node-2", Addr: remote.URL, Alive: true})
	claims := func(node string) token.Claims {
		return token.Claims{NodeID: node, TxRef: "tx-adm", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-adm", Major: 1}
	}
	selfTok, _ := signer.Issue(claims("node-1"))
	peerTok, _ := signer.Issue(claims("node-2"))
	tamperedTok, _ := other.Issue(claims("node-1"))

	for _, tc := range []struct{ name, tok string }{
		{"tampered", tamperedTok}, {"valid-for-self", selfTok}, {"valid-for-peer", peerTok},
	} {
		t.Run(tc.name, func(t *testing.T) {
			localHits := 0
			local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { localHits++; w.WriteHeader(http.StatusOK) })
			handler := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "/api")(local)

			req := httptest.NewRequest(http.MethodPut, "/api/model/order/1/lock", nil)
			req.Header.Set(proxy.TxTokenHeader, tc.tok)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"errorCode":"MODEL_ADMIN_IN_JOINED_TRANSACTION"`) {
				t.Fatalf("status=%d body=%s; want 400 MODEL_ADMIN_IN_JOINED_TRANSACTION", rec.Code, rec.Body.String())
			}
			if localHits != 0 || remoteHits != 0 {
				t.Fatalf("local=%d remote=%d handler hits; want none", localHits, remoteHits)
			}

			// The same token on an entity route is routed as before: served
			// locally or proxied, verified first.
			req = httptest.NewRequest(http.MethodPut, "/api/entity/JSON/order/1", nil)
			req.Header.Set(proxy.TxTokenHeader, tc.tok)
			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			switch tc.name {
			case "tampered":
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("entity route, tampered: status=%d, want 401", rec.Code)
				}
			case "valid-for-self":
				if localHits != 1 {
					t.Fatalf("entity route, self: local hits=%d, want 1", localHits)
				}
			case "valid-for-peer":
				if remoteHits != 1 {
					t.Fatalf("entity route, peer: remote hits=%d, want 1", remoteHits)
				}
			}
		})
	}
}

// A caller that goes away while its request is proxied is not an outage of the
// owner: the failed proxy is not logged at ERROR.
func TestHTTPProxy_CallerGoneDuringProxyIsNotAnOutage(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer owner.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: owner.URL, Alive: true},
	)
	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-gone", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-gone", Major: 1})
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")(localHandler())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil).WithContext(ctx)
	req.Header.Set(proxy.TxTokenHeader, tok)
	time.AfterFunc(50*time.Millisecond, cancel)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if strings.Contains(logBuf.String(), "level=ERROR") {
		t.Fatalf("a departed caller was logged as an error:\n%s", logBuf.String())
	}
}

// A request forwarded to its transaction's owner gets the owner's warnings: the
// proxy relays each Cyoda-Warning field line unchanged and in order.
func TestHTTPProxy_RelaysTheOwnersWarnings(t *testing.T) {
	want := []string{"first", "no workflows imported for model %E2%80%94 using default workflow", "further warnings were omitted"}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, v := range want {
			w.Header().Add(common.WarningHeader, v)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer remote.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: remote.URL, Alive: true},
	)
	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-w", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-w", Major: 1})
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")(localHandler())

	req := httptest.NewRequest(http.MethodPost, "/entity/JSON/m/1", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Result().Header.Values(common.WarningHeader)
	if len(got) != len(want) {
		t.Fatalf("%s = %q; want %q", common.WarningHeader, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %q; want %q", common.WarningHeader, got, want)
		}
	}
}

// --- retryable only with proof that nothing was sent -------------------------

// routedTo runs one GET under a token for node-2 at addr through a routing
// layer built with proxyTimeout and allowLoopback, and returns the answer.
func routedTo(t *testing.T, addr string, alive bool, proxyTimeout time.Duration, allowLoopback bool) *httptest.ResponseRecorder {
	t.Helper()
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://10.0.0.1:8080", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: addr, Alive: alive},
	)
	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-retry", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-retry", Major: 1})
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.HTTPRouting(signer, reg, "node-1", proxyTimeout, allowLoopback, "")(localHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// nodeUnavailableRetryable asserts the answer is 503
// TRANSACTION_NODE_UNAVAILABLE and returns whether it advertises
// properties.retryable=true. A body without the property is not retryable.
func nodeUnavailableRetryable(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body: %s)", rec.Code, rec.Body.String())
	}
	var pd map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode body: %v (body: %s)", err, rec.Body.String())
	}
	props, _ := pd["properties"].(map[string]any)
	if props == nil || props["errorCode"] != common.ErrCodeTransactionNodeUnavailable {
		t.Fatalf("body does not carry %s: %s", common.ErrCodeTransactionNodeUnavailable, rec.Body.String())
	}
	v, present := props["retryable"]
	if !present {
		return false
	}
	b, ok := v.(bool)
	if !ok || !b {
		t.Fatalf("properties.retryable = %v; want true or absent", v)
	}
	return true
}

// closedAddr returns a loopback address on which nothing listens.
func closedAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// The registry says the owner is not live: nothing is sent, so a retry cannot
// repeat anything.
func TestHTTPRouting_OwnerNotLive_Retryable(t *testing.T) {
	rec := routedTo(t, "http://10.0.0.2:8080", false, 5*time.Second, true)
	if !nodeUnavailableRetryable(t, rec) {
		t.Fatalf("owner not live must be retryable; body: %s", rec.Body.String())
	}
}

// The peer address guard refuses the owner's address before any dial.
func TestHTTPRouting_ForbiddenPeerAddress_Retryable(t *testing.T) {
	rec := routedTo(t, "http://127.0.0.1:9999", true, 5*time.Second, false)
	if !nodeUnavailableRetryable(t, rec) {
		t.Fatalf("an address-guard refusal must be retryable; body: %s", rec.Body.String())
	}
}

// The dial to the owner is refused: no connection, so nothing was sent.
func TestHTTPRouting_DialRefused_Retryable(t *testing.T) {
	rec := routedTo(t, "http://"+closedAddr(t), true, 5*time.Second, true)
	if !nodeUnavailableRetryable(t, rec) {
		t.Fatalf("a refused dial must be retryable; body: %s", rec.Body.String())
	}
}

// The owner received the request and did not begin to answer within the proxy
// timeout: it may have run the request, so a retry could repeat a kept write.
func TestHTTPRouting_OwnerTimesOutAfterSend_NotRetryable(t *testing.T) {
	release := make(chan struct{})
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer owner.Close()
	defer close(release)

	rec := routedTo(t, owner.URL, true, 100*time.Millisecond, true)
	if nodeUnavailableRetryable(t, rec) {
		t.Fatalf("a timeout after the request was sent must not be retryable; body: %s", rec.Body.String())
	}
}

// The transport sends a GET again on a new connection when a reused one breaks
// after the request was written. If the owner went down in between, that second
// attempt fails to dial: a dial error, but the owner received the first
// attempt and may have run it. Not retryable.
func TestHTTPRouting_DialRefusedAfterSend_NotRetryable(t *testing.T) {
	var owner *httptest.Server
	owner = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warm" {
			return
		}
		// The owner took the request, then went down: no further dial
		// succeeds, and this connection ends without an answer.
		_ = owner.Listener.Close()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer owner.Close()

	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://10.0.0.1:8080", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: owner.URL, Alive: true},
	)
	tok, err := signer.Issue(token.Claims{NodeID: "node-2", TxRef: "tx-resent", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-resent", Major: 1})
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	handler := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")(localHandler())
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(proxy.TxTokenHeader, tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	// A first request leaves a kept-alive connection to the owner, which the
	// next request reuses.
	if rec := get("/warm"); rec.Code != http.StatusOK {
		t.Fatalf("warm-up: status %d, body %s", rec.Code, rec.Body.String())
	}
	rec := get("/api/test")
	// The answer must come from the second attempt's failed dial. Without the
	// reused connection there is no second attempt, the error is the broken
	// connection's, and the test would pass without reaching its branch.
	if logged := logBuf.String(); !strings.Contains(logged, "connect: connection refused") {
		t.Fatalf("the proxied request did not end in a refused dial; log:\n%s", logged)
	}
	if nodeUnavailableRetryable(t, rec) {
		t.Fatalf("a dial error after the owner received the request must not be retryable; body: %s", rec.Body.String())
	}
}

// TestHTTPRouting_PassWithoutOwnerServedLocally: a pass that names no owner is
// one of a location-independent transaction, which any pnode serves. It is
// served where it lands, never proxied and never refused as a node that is
// not live.
func TestHTTPRouting_PassWithoutOwnerServedLocally(t *testing.T) {
	signer := mustNewSigner([]byte("test-secret-key-at-least-32-bytes!"))
	reg := newFakeRegistry(
		contract.NodeInfo{NodeID: "node-1", Addr: "http://localhost:9999", Alive: true},
		contract.NodeInfo{NodeID: "node-2", Addr: "http://localhost:8888", Alive: true},
	)
	tok, err := signer.Issue(token.Claims{TxRef: "tx-li", ExpiresAt: time.Now().Add(5 * time.Minute).Unix(), Callout: "req-tx-li", Major: 1})
	if err != nil {
		t.Fatal(err)
	}

	handler := proxy.HTTPRouting(signer, reg, "node-1", 5*time.Second, true, "")(localHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set(proxy.TxTokenHeader, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "local" {
		t.Fatalf("got %d %q; want 200 \"local\"", rec.Code, rec.Body.String())
	}
}
