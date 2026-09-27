package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
)

type staleKeyStore struct{ auth.KeyStore }

func (staleKeyStore) Published() ([]*auth.KeyPair, error) { return nil, auth.ErrStoreStale }

func TestJWKS_StaleAnswers503WithRetryAfter(t *testing.T) {
	h := auth.NewJWKSHandler(staleKeyStore{}, 30*time.Second)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "30" {
		t.Fatalf("status %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}

// A sub-second interval still advertises a positive wait: Retry-After is
// rounded up to whole seconds, never 0.
func TestJWKS_StaleRetryAfterRoundsUp(t *testing.T) {
	h := auth.NewJWKSHandler(staleKeyStore{}, 500*time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if got := w.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After %q, want 1", got)
	}
}
