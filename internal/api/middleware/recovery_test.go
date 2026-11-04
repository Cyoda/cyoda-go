package middleware_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda-platform/cyoda-go/internal/api/middleware"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/health"
)

func TestRecoveryMiddlewareCatchesPanic(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	healthLatch := health.NewLatch()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something terrible")
	})

	wrapped := middleware.Recovery(healthLatch)(handler)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	wrapped.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Errorf("expected 500, got %d", w.Code)
	}
	if !healthLatch.Tripped() {
		t.Error("expected health latch to be tripped after FATAL")
	}
	var pd map[string]any
	json.NewDecoder(w.Body).Decode(&pd)
	if pd["ticket"] == nil || pd["ticket"] == "" {
		t.Error("expected ticket UUID in panic response")
	}
}

func TestRecoveryMiddlewarePassesThrough(t *testing.T) {
	healthLatch := health.NewLatch()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	wrapped := middleware.Recovery(healthLatch)(handler)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	wrapped.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if healthLatch.Tripped() {
		t.Error("health latch should stay untripped")
	}
}

func TestRecoveryMiddlewareHealthLatchStaysTripped(t *testing.T) {
	healthLatch := health.NewLatch()

	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	// First request panics
	wrapped := middleware.Recovery(healthLatch)(panicHandler)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))

	// The health latch is now tripped
	if !healthLatch.Tripped() {
		t.Fatal("expected unhealthy after panic")
	}

	// Second request is OK but latch stays tripped
	wrapped2 := middleware.Recovery(healthLatch)(okHandler)
	w2 := httptest.NewRecorder()
	wrapped2.ServeHTTP(w2, httptest.NewRequest("GET", "/", nil))

	if !healthLatch.Tripped() {
		t.Error("health latch should stay tripped — no auto-recovery")
	}
}

func TestRecoveryMiddleware_VerboseMode_NoStackTraceInResponse(t *testing.T) {
	common.SetErrorResponseMode("verbose")
	defer common.SetErrorResponseMode("sanitized")

	healthLatch := health.NewLatch()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something terrible")
	})

	wrapped := middleware.Recovery(healthLatch)(handler)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/test", nil)
	wrapped.ServeHTTP(w, r)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d", w.Code)
	}

	body := w.Body.String()
	if strings.Contains(body, "goroutine") {
		t.Error("response body must not contain 'goroutine' (stack trace leaked)")
	}
	if strings.Contains(body, ".go:") {
		t.Error("response body must not contain file paths like '.go:' (stack trace leaked)")
	}
	if strings.Contains(body, "runtime/debug") {
		t.Error("response body must not contain 'runtime/debug' (stack trace leaked)")
	}
}

// TestRecoveryMiddleware_PanicLogCarriesTheClientTicket — the ticket is the only
// thing a client can quote, and the stack is the only thing that says what
// happened. If the two are not joined, an operator handed a ticket finds the
// FATAL line whose detail has been deliberately overwritten with "panic
// recovered; check server logs for details" and has no way to reach the stack
// that sits in a different line. The gRPC door already logs the ticket with the
// panic; this is the HTTP half.
func TestRecoveryMiddleware_PanicLogCarriesTheClientTicket(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	healthLatch := health.NewLatch()
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("something terrible")
	})

	w := httptest.NewRecorder()
	middleware.Recovery(healthLatch)(handler).ServeHTTP(w, httptest.NewRequest("GET", "/test", nil))

	var pd map[string]any
	if err := json.NewDecoder(w.Body).Decode(&pd); err != nil {
		t.Fatalf("decode problem detail: %v", err)
	}
	ticket, _ := pd["ticket"].(string)
	if ticket == "" {
		t.Fatal("no ticket in the panic response")
	}

	var panicLine map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			continue
		}
		if rec["msg"] == "panic recovered" {
			panicLine = rec
		}
	}
	if panicLine == nil {
		t.Fatalf("no \"panic recovered\" line was logged: %s", buf.String())
	}
	if got, _ := panicLine["ticket"].(string); got != ticket {
		t.Errorf("panic log ticket = %q, client was given %q — the stack cannot be joined to the ticket", got, ticket)
	}
	if stack, _ := panicLine["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Errorf("the ticketed line carries no stack, so joining it buys nothing: %v", panicLine["stack"])
	}
}

// net/http uses http.ErrAbortHandler as a sentinel: a handler (notably
// httputil.ReverseProxy when the client hangs up mid-body) panics with it to
// abort the response silently. Recovery must re-raise it, not treat it as a
// defect: no log, no 500, no health latch.
func TestRecoveryMiddlewareReRaisesErrAbortHandler(t *testing.T) {
	healthLatch := health.NewLatch()
	handler := middleware.Recovery(healthLatch)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	srv := httptest.NewServer(handler) // a real server: net/http swallows the sentinel
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected the connection to be aborted, got status %d", resp.StatusCode)
	}
	if healthLatch.Tripped() {
		t.Fatal("ErrAbortHandler must not trip the health latch")
	}
}

// A surface that does no engine or store work on the application's behalf —
// the admin listener's probes and metrics scrape — opts out of the latch by
// passing no latch. Containment still applies: the panic becomes a sanitised
// 500 carrying a ticket. Nothing the process holds is marked unhealthy.
func TestRecoveryMiddleware_NilLatchContainsWithoutTrip(t *testing.T) {
	common.SetErrorResponseMode("sanitized")
	// A latch the caller holds but never hands to Recovery. It stands in for
	// the health latch the admin door deliberately does not pass, and it
	// must come out of the panic untouched.
	otherLatch := health.NewLatch()

	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("probe exploded")
	})
	w := httptest.NewRecorder()
	middleware.Recovery(nil)(handler).ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))

	if w.Code != 500 {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	var pd map[string]any
	if err := json.NewDecoder(w.Body).Decode(&pd); err != nil {
		t.Fatalf("decode problem detail: %v", err)
	}
	if ticket, _ := pd["ticket"].(string); ticket == "" {
		t.Error("expected a ticket UUID in the panic response")
	}
	if otherLatch.Tripped() {
		t.Error("Recovery(nil) must not trip any health latch")
	}
}
