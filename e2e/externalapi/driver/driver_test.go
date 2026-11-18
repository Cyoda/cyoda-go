package driver_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cyoda/cyoda-go/e2e/externalapi/driver"
)

// TestNewRemote_UsesProvidedBaseURLAndToken verifies the remote constructor
// issues requests against the given base URL with the given bearer token —
// no fixture state leaks into the call path.
func TestNewRemote_UsesProvidedBaseURLAndToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	d := driver.NewRemote(t, srv.URL, "remote-jwt-token")
	if err := d.ListModelsDiscard(); err != nil {
		t.Fatalf("ListModelsDiscard: %v", err)
	}
	// Don't echo the captured Authorization header value verbatim — when
	// this test runs against a real tenant JWT (e.g. CI smoke), %q would
	// leak the credential into test output. Report length only.
	if !strings.Contains(gotAuth, "remote-jwt-token") {
		t.Errorf("Authorization header missing expected bearer token (got header length %d, expected to contain test sentinel)", len(gotAuth))
	}
}

func TestNewRemote_NoToken_EmptyBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	d := driver.NewRemote(t, srv.URL, "")
	_ = d.ListModelsDiscard()
	// Report length and shape, not the value.
	if gotAuth != "Bearer " && gotAuth != "" {
		t.Errorf("empty token should produce empty or bare Bearer header (got header of length %d)", len(gotAuth))
	}
	_ = io.Discard
}
