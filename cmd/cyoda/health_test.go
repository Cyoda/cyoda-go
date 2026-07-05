package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// portFromURL extracts the port segment from a server URL like "http://127.0.0.1:12345".
func portFromURL(t *testing.T, u string) string {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestCyodaHealth_Ready(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("expected /readyz, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("CYODA_ADMIN_PORT", portFromURL(t, server.URL))

	if code := runHealth(nil); code != 0 {
		t.Fatalf("expected exit 0 (ready); got %d", code)
	}
}

func TestCyodaHealth_NotReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "storage unreachable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	t.Setenv("CYODA_ADMIN_PORT", portFromURL(t, server.URL))

	if code := runHealth(nil); code != 1 {
		t.Fatalf("expected exit 1 (503 → not ready); got %d", code)
	}
}

func TestCyodaHealth_ConnectionRefused(t *testing.T) {
	// Bind a server, capture its port, then close immediately. Subsequent
	// connections to that port get refused (no listener).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	port := portFromURL(t, server.URL)
	server.Close()

	t.Setenv("CYODA_ADMIN_PORT", port)

	if code := runHealth(nil); code != 1 {
		t.Fatalf("expected exit 1 (connection refused → not ready); got %d", code)
	}
}

// TestCyodaHealth_Timeout verifies the client-side 2s timeout fires when the
// server accepts the connection but never writes a response. Without this
// test a regression in the timeout value (raised too high, or removed
// altogether) would let Docker's HEALTHCHECK inherit a deadlock.
func TestCyodaHealth_Timeout(t *testing.T) {
	done := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Hold the connection open beyond the client's 2s timeout but respect
		// test shutdown so the goroutine doesn't leak.
		select {
		case <-time.After(10 * time.Second):
		case <-done:
		}
	}))
	defer server.Close()
	defer close(done) // registered after server.Close so it fires first (LIFO); unblocks the handler before server tries to drain.

	t.Setenv("CYODA_ADMIN_PORT", portFromURL(t, server.URL))

	start := time.Now()
	code := runHealth(nil)
	elapsed := time.Since(start)

	if code != 1 {
		t.Fatalf("expected exit 1 (timeout → not ready); got %d", code)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("runHealth should time out within ~2s; took %v", elapsed)
	}
}

func TestCyodaHealth_RespectsAdminPort(t *testing.T) {
	// httptest picks a random non-default port; setting CYODA_ADMIN_PORT to
	// it is the only way this test can pass, so success = port respected.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	port := portFromURL(t, server.URL)
	if port == "9091" {
		t.Skip("httptest picked the default port; rerun")
	}
	t.Setenv("CYODA_ADMIN_PORT", port)

	if code := runHealth(nil); code != 0 {
		t.Fatalf("expected exit 0 when CYODA_ADMIN_PORT points at real server; got %d", code)
	}
}
