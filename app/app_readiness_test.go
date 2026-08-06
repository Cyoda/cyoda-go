package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/admin"
	"github.com/cyoda-platform/cyoda-go/internal/api/middleware"
)

// readinessTestApp builds a fully wired app on the default (memory) backend —
// the same constructor cmd/cyoda uses, so the health flag under test is the
// one the production request doors were handed.
func readinessTestApp(t *testing.T) *App {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ContextPath = ""
	a := New(cfg)
	t.Cleanup(func() {
		a.Shutdown()
		_ = a.Close()
	})
	return a
}

// probeReadyz drives /readyz through the real admin handler wired the way
// cmd/cyoda/run.go wires it, so the test covers the handler mapping as well
// as the check itself.
func probeReadyz(t *testing.T, a *App) *httptest.ResponseRecorder {
	t.Helper()
	h := admin.NewHandler(admin.Options{Readiness: a.ReadinessCheck})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec
}

// recoverPanicOnce drives a panic through the production recovery middleware
// holding the app's own health flag — exactly what happens when any HTTP route
// or gRPC method panics.
func recoverPanicOnce(a *App) {
	h := middleware.Recovery(a.healthFlag)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panics", nil))
}

// TestReadinessCheck_HealthyNodeIsReady is the other direction of the pair
// below: a node that has not panicked stays ready. Without it, an always-fail
// readiness check would pass the 503 test.
func TestReadinessCheck_HealthyNodeIsReady(t *testing.T) {
	a := readinessTestApp(t)

	if err := a.ReadinessCheck(); err != nil {
		t.Fatalf("healthy node: ReadinessCheck() = %v, want nil", err)
	}
	rec := probeReadyz(t, a)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy node: /readyz = %d, want 200", rec.Code)
	}
}

// TestReadinessCheck_AfterRecoveredPanicIsNotReady is the defect: a recovered
// panic marks the node unhealthy, but nothing that decides whether the node
// receives traffic read that flag. /livez is unconditional and /readyz looked
// only at the store factory, so a node with unverified state kept serving.
func TestReadinessCheck_AfterRecoveredPanicIsNotReady(t *testing.T) {
	a := readinessTestApp(t)
	if err := a.ReadinessCheck(); err != nil {
		t.Fatalf("precondition: fresh node not ready: %v", err)
	}

	recoverPanicOnce(a)

	if err := a.ReadinessCheck(); err == nil {
		t.Fatal("after a recovered panic: ReadinessCheck() = nil, want an error")
	}
	rec := probeReadyz(t, a)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after a recovered panic: /readyz = %d, want 503", rec.Code)
	}
	// Gate 3: the probe body stays generic — no panic value, no stack, no
	// internal state.
	if body := rec.Body.String(); strings.Contains(body, "boom") || strings.Contains(body, "panic") {
		t.Errorf("/readyz body leaked internal state: %q", body)
	}
}

// TestReadinessCheck_UsesTheSameFlagTheRequestDoorsMark proves the wiring
// rather than an accessor: the flag flipped by the recovery middleware is the
// one app.New registered on /health, so readiness and the health endpoint
// cannot drift apart.
func TestReadinessCheck_UsesTheSameFlagTheRequestDoorsMark(t *testing.T) {
	a := readinessTestApp(t)

	recoverPanicOnce(a)

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/health = %d, want 503 — the flag readiness reads is not the one the app wired", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /health body: %v", err)
	}
	if body["status"] != "DOWN" {
		t.Fatalf("/health status = %q, want DOWN", body["status"])
	}
}

// TestReadinessCheck_ReasonsAreDistinguishable keeps the two failure
// conditions independent and separately identifiable. The admin handler logs
// the returned error server-side and answers the client generically, so the
// error text is what an operator has to tell a storage fault from a recovered
// panic.
func TestReadinessCheck_ReasonsAreDistinguishable(t *testing.T) {
	storageDown := readinessTestApp(t)
	storageDown.storeFactory = nil
	storageErr := storageDown.ReadinessCheck()
	if storageErr == nil {
		t.Fatal("nil store factory: ReadinessCheck() = nil, want an error")
	}

	panicked := readinessTestApp(t)
	recoverPanicOnce(panicked)
	panicErr := panicked.ReadinessCheck()
	if panicErr == nil {
		t.Fatal("recovered panic: ReadinessCheck() = nil, want an error")
	}

	if storageErr.Error() == panicErr.Error() {
		t.Fatalf("both reasons report %q — an operator cannot tell them apart", storageErr)
	}
	if !strings.Contains(storageErr.Error(), "storage") {
		t.Errorf("storage reason = %q, want it to name storage", storageErr)
	}
	if !strings.Contains(panicErr.Error(), "panic") {
		t.Errorf("panic reason = %q, want it to name the recovered panic", panicErr)
	}
}
