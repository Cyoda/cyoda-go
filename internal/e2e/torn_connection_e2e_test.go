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
// SetLinger(0) makes the close send an RST rather than a FIN. A FIN leaves the
// socket half-open: the peer's writes still succeed locally, so pgconn's Close
// sends its Terminate message and then waits its full fifteen-second deadline
// for a peer that will never close. An RST is also the truer fault — it is what
// a killed server or a dropped link produces, and it is the shape isConnectionTorn
// recognises.
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
	upstream := u.Host
	u.Host = proxyAddr
	q := u.Query()
	q.Set("application_name", appName)
	u.RawQuery = q.Encode()
	_ = upstream
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
		t.Setenv("CYODA_POSTGRES_MAX_CONNS", "1")
		t.Setenv("CYODA_POSTGRES_MIN_CONNS", "0")
		// The scan loop would reconnect the pool between the cut and the probe.
		cfg.Scheduler.Enabled = false
		cfg.IAM.TrustedKeyRegistrationEnabled = true
	})
	return &tornHarness{callbackHarness: h, proxy: proxy}
}

// probeTorn runs warm → cut → probe until it observes a failure, and asserts the
// failure is the retryable 503 the marker mints. Cycles exist because pgx pings
// a connection idle for more than a second and would transparently replace it;
// the probe follows the cut by microseconds, so one cycle normally suffices.
func probeTorn(t *testing.T, h *tornHarness, warm, probe func() (int, string)) {
	t.Helper()
	for i := 0; i < 3; i++ {
		// The warm-up after a previous cut can take ~15s: pgx destroys the torn
		// connection on the next acquire, and pgconn.Close waits its own deadline
		// for a peer that RST'd rather than closing politely. It is inside the
		// driver, not this stack — a server-initiated close recovers in
		// milliseconds — and it is paid at most once per cut.
		if status, body := warm(); status >= 500 {
			t.Fatalf("cycle %d: warm-up failed: %d %s", i, status, body)
		}
		if torn := h.proxy.cut(); torn == 0 {
			t.Fatalf("cycle %d: no live connection to tear", i)
		}
		status, body := probe()
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

	get := func(path string) func() (int, string) {
		return func() (int, string) {
			resp := h.DoAuth(t, http.MethodGet, path, "", "")
			return resp.StatusCode, h.readBody(t, resp)
		}
	}
	warm := get("/api/search/async/" + jobID + "/status")

	t.Run("status", func(t *testing.T) {
		probeTorn(t, h, warm, get("/api/search/async/"+jobID+"/status"))
	})
	t.Run("results", func(t *testing.T) {
		probeTorn(t, h, warm, get("/api/search/async/"+jobID))
	})
	t.Run("cancel", func(t *testing.T) {
		probeTorn(t, h, warm, func() (int, string) {
			resp := h.DoAuth(t, http.MethodPut, "/api/search/async/"+jobID+"/cancel", "", "")
			return resp.StatusCode, h.readBody(t, resp)
		})
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
	auditPath := fmt.Sprintf("/api/audit/entity/%s/workflow/%s/finished", entityID, txID)
	auditCall := func() (int, string) {
		resp := h.DoAuth(t, http.MethodGet, auditPath, "", "")
		return resp.StatusCode, h.readBody(t, resp)
	}
	t.Run("finishedEvent", func(t *testing.T) {
		probeTorn(t, h, auditCall, auditCall)
	})

	const kid = "torn-trusted-key"
	registerTrustedTestKey(t, h.callbackHarness, kid)
	base := "/api/oauth/keys/trusted/" + kid
	reactivateBody := `{"validTo":"` + time.Now().Add(24*time.Hour).Format(time.RFC3339) + `"}`
	post := func(path, body string) func() (int, string) {
		return func() (int, string) {
			resp := h.DoAuth(t, http.MethodPost, path, body, "")
			return resp.StatusCode, h.readBody(t, resp)
		}
	}
	t.Run("invalidate", func(t *testing.T) {
		probeTorn(t, h, post(base+"/invalidate", ""), post(base+"/invalidate", ""))
	})
	t.Run("reactivate", func(t *testing.T) {
		probeTorn(t, h, post(base+"/reactivate", reactivateBody), post(base+"/reactivate", reactivateBody))
	})
	t.Run("delete", func(t *testing.T) {
		const deleteKid = "torn-trusted-delete"
		registerTrustedTestKey(t, h.callbackHarness, deleteKid)
		list := func() (int, string) {
			resp := h.DoAuth(t, http.MethodGet, "/api/oauth/keys/trusted", "", "")
			return resp.StatusCode, h.readBody(t, resp)
		}
		probeTorn(t, h, list, func() (int, string) {
			resp := h.DoAuth(t, http.MethodDelete, "/api/oauth/keys/trusted/"+deleteKid, "", "")
			return resp.StatusCode, h.readBody(t, resp)
		})
	})
}
