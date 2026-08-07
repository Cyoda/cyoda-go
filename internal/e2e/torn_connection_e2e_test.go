package e2e_test

// torn_connection_e2e_test.go — the 503 cell, injected for real.
//
// A torn socket is the one storage failure PostgreSQL's own error reporting
// cannot describe: the session is gone before it can say so. The plugin
// classifies it as transient (ceilings.go's isConnectionTorn → idleInTxAbortError
// → the StorageUnavailable marker), so it is the shape that produces a retryable
// 503 on a read path that never opens a transaction.
//
// It is injected by routing this stack's pool through a TCP proxy the test owns
// and closing the live connections underneath it. The proxy keeps listening, so
// the pool reconnects immediately afterwards and the stack recovers — the fault
// is one torn connection, not an outage of the container every other test shares.
//
// Isolation: the proxy, the stack and the pool are this test's own. Nothing is
// locked and no session belonging to anything else is touched.

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/app"
)

// pgProxy forwards TCP to PostgreSQL and can cut every live connection on
// demand. Cutting closes both halves without a graceful shutdown, so the client
// sees a reset rather than a FATAL ErrorResponse — which is the whole point:
// pg_terminate_backend produces the server's 57P01, a shape the plugin
// deliberately leaves unmarked.
type pgProxy struct {
	ln       net.Listener
	upstream string

	mu    sync.Mutex
	conns []net.Conn
}

func newPGProxy(t *testing.T, upstream string) *pgProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &pgProxy{ln: ln, upstream: upstream}
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	go p.serve()
	return p
}

func (p *pgProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go p.pair(client)
	}
}

// cut tears every connection currently proxied and returns how many pairs it
// tore. The pool notices on its next statement.
//
// SetLinger(0) makes the close send an RST rather than a FIN, because an RST is
// the fault being modelled: it is what a killed server or a dropped link
// produces, and it is the shape isConnectionTorn recognises. It does not avoid
// the reconnect wait described on probeTorn — that is paid either way.
func (p *pgProxy) cut() int {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		if tcp, ok := c.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = c.Close()
	}
	return len(conns) / 2
}

// pair dials upstream for one accepted connection and copies both ways. The
// dial happens here rather than in the accept loop: a dial that stalls must not
// hold up the next connection, and after a cut the pool reconnects immediately.
func (p *pgProxy) pair(client net.Conn) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	p.conns = append(p.conns, client, server)
	p.mu.Unlock()
	go func() { _, _ = io.Copy(server, client) }()
	_, _ = io.Copy(client, server)
}

func (p *pgProxy) addr() string { return p.ln.Addr().String() }

// proxiedPGURL rewrites the DSN TestMain published to point at the proxy,
// keeping every other parameter (credentials, database, sslmode) intact.
func proxiedPGURL(t *testing.T, proxyAddr, appName string) string {
	t.Helper()
	u, err := url.Parse(pgURLFromEnv(t))
	if err != nil {
		t.Fatalf("parse postgres URL: %v", err)
	}
	u.Host = proxyAddr
	q := u.Query()
	q.Set("application_name", appName)
	u.RawQuery = q.Encode()
	return u.String()
}

// tornHarness is a stack whose every statement runs through a proxy the test
// can cut.
type tornHarness struct {
	*callbackHarness
	proxy *pgProxy
}

func newTornHarness(t *testing.T) *tornHarness {
	t.Helper()
	u, err := url.Parse(pgURLFromEnv(t))
	if err != nil {
		t.Fatalf("parse postgres URL: %v", err)
	}
	proxy := newPGProxy(t, u.Host)
	h := newCallbackHarnessConfigured(t, func(cfg *app.Config) {
		t.Setenv("CYODA_POSTGRES_URL", proxiedPGURL(t, proxy.addr(), harnessAppName(t)))
		// Exactly one slot. With a spare, the pool can hand the probe a connection
		// it created after the cut instead of the torn one, and the injection stops
		// being reliable — measured: the results probe returned 200 on all three
		// cycles. A single slot means the connection the probe gets is the
		// connection that was torn.
		t.Setenv("CYODA_POSTGRES_MAX_CONNS", "1")
		t.Setenv("CYODA_POSTGRES_MIN_CONNS", "0")
		// The scan loop would reconnect the pool between the cut and the probe.
		cfg.Scheduler.Enabled = false
		cfg.IAM.TrustedKeyRegistrationEnabled = true
	})
	return &tornHarness{callbackHarness: h, proxy: proxy}
}

// healJobID is a well-formed job UUID that belongs to no job — a warm-up that
// needs no fixture, for a subtest whose own endpoint would not acquire.
const healJobID = "00000000-0000-4000-8000-0000000000aa"

// tornCall issues one request against the harness. It takes the *testing.T of
// whichever subtest is running, so a failure is reported against that subtest
// rather than against the parent from another goroutine.
type tornCall func(t *testing.T) (int, string)

func (h *tornHarness) get(path string) tornCall {
	return func(t *testing.T) (int, string) {
		t.Helper()
		resp := h.DoAuth(t, http.MethodGet, path, "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
}

func (h *tornHarness) post(path, body string) tornCall {
	return func(t *testing.T) (int, string) {
		t.Helper()
		resp := h.DoAuth(t, http.MethodPost, path, body, "")
		return resp.StatusCode, h.readBody(t, resp)
	}
}

func (h *tornHarness) do(method, path string) tornCall {
	return func(t *testing.T) (int, string) {
		t.Helper()
		resp := h.DoAuth(t, method, path, "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
}

// probeTorn runs warm → cut → probe until it observes a failure, and asserts the
// failure is the retryable 503 the marker mints. Cycles exist because pgx pings
// a connection idle for more than a second and would transparently replace it;
// the probe follows the cut by microseconds, so one cycle normally suffices.
//
// The first acquire after a cut costs ~15s: pgx destroys the torn connection
// then, and pgconn.Close waits its own deadline for a peer that RST'd rather
// than closing politely. It is inside the driver, not this stack — a
// server-initiated close recovers in milliseconds — and it is fixed wall clock,
// not load-dependent. It is paid, not hidden: moving it to a background
// goroutine changes nothing against a single-connection pool (measured), and
// widening the pool so it could overlap costs the injection its reliability.
func probeTorn(t *testing.T, h *tornHarness, warm, probe tornCall) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if status, body := warm(t); status >= 500 {
			t.Fatalf("cycle %d: warm-up failed: %d %s", i, status, body)
		}
		if torn := h.proxy.cut(); torn == 0 {
			t.Fatalf("cycle %d: no live connection to tear", i)
		}
		status, body := probe(t)
		t.Logf("cycle %d: status=%d body=%s", i, status, body)
		if status < 400 {
			continue // the pool replaced the connection before the probe landed
		}
		assertRetryable503(t, status, body)
		return
	}
	t.Fatal("no probe reached the torn connection in 3 cycles; the fault was never injected")
}

func assertRetryable503(t *testing.T, status int, body string) {
	t.Helper()
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 STORAGE_UNAVAILABLE; body: %s", status, body)
	}
	var pd struct {
		Detail string         `json:"detail"`
		Props  map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(body), &pd); err != nil {
		t.Fatalf("problem detail is not JSON: %v; body: %s", err, body)
	}
	if code, _ := pd.Props["errorCode"].(string); code != "STORAGE_UNAVAILABLE" {
		t.Errorf("errorCode = %q, want STORAGE_UNAVAILABLE; body: %s", code, body)
	}
	if retryable, _ := pd.Props["retryable"].(bool); !retryable {
		t.Errorf("the 503 is not advertised as retryable; body: %s", body)
	}
	for _, leak := range []string{"postgres://", "password", "dbname=", "127.0.0.1", "econnreset"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("client-facing body leaks infrastructure (%q): %s", leak, body)
		}
	}
}

// TestE2E_TornConnection_AsyncSearchLookups503 covers getAsyncSearchStatus,
// getAsyncSearchResults and cancelAsyncSearch — the three operations whose store
// is the one in the factory built on the bare pool.
func TestE2E_TornConnection_AsyncSearchLookups503(t *testing.T) {
	h := newTornHarness(t)
	model := storageCeilingModel(t, "torn")
	h.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)
	jobID := settledJob(t, h.callbackHarness, model)

	warm := h.get("/api/search/async/" + jobID + "/status")

	t.Run("status", func(t *testing.T) {
		probeTorn(t, h, warm, h.get("/api/search/async/"+jobID+"/status"))
	})
	t.Run("results", func(t *testing.T) {
		probeTorn(t, h, warm, h.get("/api/search/async/"+jobID))
	})
	t.Run("cancel", func(t *testing.T) {
		probeTorn(t, h, warm, h.do(http.MethodPut, "/api/search/async/"+jobID+"/cancel"))
	})
}

// TestE2E_TornConnection_AuditAndTrustedKeys503 covers the other three:
// getStateMachineFinishedEvent and the trusted-key mutations. Their stores are
// built on the context-resolving querier, so they reach the classifier already.
func TestE2E_TornConnection_AuditAndTrustedKeys503(t *testing.T) {
	h := newTornHarness(t)
	model := storageCeilingModel(t, "torn")
	h.setupModelSampleWithWorkflow(t, model, `{"name":"Alice","amount":1,"status":"new"}`, secondaryWorkflow)

	entityID, status, body := h.CreateEntity(t, model, 1, `{"name":"Alice","amount":1,"status":"new"}`)
	if status != http.StatusOK {
		t.Fatalf("create entity: %d %s", status, body)
	}
	var txID string
	for _, ev := range h.GetSMAuditEvents(t, entityID) {
		if id, _ := ev["transactionId"].(string); id != "" {
			txID = id
			break
		}
	}
	if txID == "" {
		t.Fatal("no state-machine event carried a transaction id")
	}
	auditCall := h.get(fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, txID))
	t.Run("finishedEvent", func(t *testing.T) {
		probeTorn(t, h, auditCall, auditCall)
	})

	const kid = "torn-trusted-key"
	registerTrustedTestKey(t, h.callbackHarness, kid)
	base := "/api/oauth/keys/trusted/" + kid
	reactivateBody := `{"validTo":"` + time.Now().Add(24*time.Hour).Format(time.RFC3339) + `"}`
	t.Run("invalidate", func(t *testing.T) {
		probeTorn(t, h, h.post(base+"/invalidate", ""), h.post(base+"/invalidate", ""))
	})
	t.Run("reactivate", func(t *testing.T) {
		probeTorn(t, h, h.post(base+"/reactivate", reactivateBody), h.post(base+"/reactivate", reactivateBody))
	})
	t.Run("delete", func(t *testing.T) {
		const deleteKid = "torn-trusted-delete"
		registerTrustedTestKey(t, h.callbackHarness, deleteKid)
		// Listing trusted keys is served from the in-memory cache and would put
		// nothing on the wire; the warm-up has to be a request that acquires.
		probeTorn(t, h,
			h.get("/api/search/async/"+healJobID+"/status"),
			h.do(http.MethodDelete, "/api/oauth/keys/trusted/"+deleteKid))
	})
}
