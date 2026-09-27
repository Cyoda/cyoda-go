package client_test

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cyoda-platform/cyoda-go/e2e/parity/client"
)

// The key pair a scenario issues must be deleted even when the scenario
// fails, and t.Context() is cancelled before cleanups run — so the cleanup
// request has to reach the server on its own context.
func TestDeleteKeyPairOnCleanup_ReachesTheServer(t *testing.T) {
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/oauth/keys/keypair/k1" {
			deletes.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	c := client.NewClient(srv.URL, "tok")

	t.Run("scenario", func(t *testing.T) {
		c.DeleteKeyPairOnCleanup(t, "k1")
	})
	if got := deletes.Load(); got != 1 {
		t.Fatalf("DELETE requests at the server after cleanup: %d, want 1", got)
	}
}

func TestTokenKID(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	tok := enc([]byte(`{"alg":"RS256","kid":"abc"}`)) + "." + enc([]byte(`{}`)) + ".sig"
	if got := client.TokenKID(tok); got != "abc" {
		t.Fatalf("TokenKID: %q, want abc", got)
	}
	for _, bad := range []string{"", "nodots", "!!!.x.y", enc([]byte(`not json`)) + ".x.y"} {
		if got := client.TokenKID(bad); got != "" {
			t.Errorf("TokenKID(%q) = %q, want empty", bad, got)
		}
	}
}
