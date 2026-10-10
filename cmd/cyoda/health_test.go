package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
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
	isolateEnvFiles(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("expected /readyz, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("CYODA_ADMIN_PORT", portFromURL(t, server.URL))

	if code := runHealth(); code != 0 {
		t.Fatalf("expected exit 0 (ready); got %d", code)
	}
}

func TestCyodaHealth_NotReady(t *testing.T) {
	isolateEnvFiles(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "storage unreachable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	t.Setenv("CYODA_ADMIN_PORT", portFromURL(t, server.URL))

	if code := runHealth(); code != 1 {
		t.Fatalf("expected exit 1 (503 → not ready); got %d", code)
	}
}

func TestCyodaHealth_ConnectionRefused(t *testing.T) {
	isolateEnvFiles(t)
	// Bind a server, capture its port, then close immediately. Subsequent
	// connections to that port get refused (no listener).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	port := portFromURL(t, server.URL)
	server.Close()

	t.Setenv("CYODA_ADMIN_PORT", port)

	if code := runHealth(); code != 1 {
		t.Fatalf("expected exit 1 (connection refused → not ready); got %d", code)
	}
}

// TestCyodaHealth_Timeout verifies the client-side 2s timeout fires when the
// server accepts the connection but never writes a response. Without this
// test a regression in the timeout value (raised too high, or removed
// altogether) would let Docker's HEALTHCHECK inherit a deadlock.
func TestCyodaHealth_Timeout(t *testing.T) {
	isolateEnvFiles(t)
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
	code := runHealth()
	elapsed := time.Since(start)

	if code != 1 {
		t.Fatalf("expected exit 1 (timeout → not ready); got %d", code)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("runHealth should time out within ~2s; took %v", elapsed)
	}
}

func TestCyodaHealth_RespectsAdminPort(t *testing.T) {
	isolateEnvFiles(t)
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

	if code := runHealth(); code != 0 {
		t.Fatalf("expected exit 0 when CYODA_ADMIN_PORT points at real server; got %d", code)
	}
}

// TestCyodaHealth_ReadsEnvFiles pins that health probes the admin port the
// server reads, including one set in an env file. The test asserts that the
// configured server was reached, not only the exit code: another server on
// the default port must not make a regression pass.
func TestCyodaHealth_ReadsEnvFiles(t *testing.T) {
	xdg := isolateEnvFiles(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("CYODA_ADMIN_PORT", "") // restores the variable's absence at cleanup
	os.Unsetenv("CYODA_ADMIN_PORT")
	if err := os.MkdirAll(filepath.Join(xdg, "cyoda"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "CYODA_ADMIN_PORT=" + portFromURL(t, server.URL) + "\n"
	if err := os.WriteFile(filepath.Join(xdg, "cyoda", "cyoda.env"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := runHealth(); code != 0 {
		t.Errorf("runHealth exit code = %d; want 0", code)
	}
	if hits.Load() == 0 {
		t.Error("runHealth did not probe the admin port set in the user config")
	}
}

// TestCyodaHealth_RefusesPortThatIsNotANumber pins that the probe only ever
// goes to 127.0.0.1. CYODA_ADMIN_PORT can come from an env file, ./.env in
// the working directory included, and a value such as "9091@host:port" would
// otherwise make the URL's host "host:port" — an outbound call a planted file
// could steer. The test server stands in for that host: it must not be hit.
func TestCyodaHealth_RefusesPortThatIsNotANumber(t *testing.T) {
	isolateEnvFiles(t)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	for _, port := range []string{
		"9091@127.0.0.1:" + portFromURL(t, server.URL),
		"abc",
		"0",
		"65536",
		"-1",
	} {
		t.Run(port, func(t *testing.T) {
			t.Setenv("CYODA_ADMIN_PORT", port)
			if code := runHealth(); code != 1 {
				t.Errorf("runHealth exit code = %d; want 1", code)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("the probe reached another host %d time(s)", hits.Load())
	}
}
