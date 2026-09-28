package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
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
	// The body is the generic problem JSON; the store's own error stays in
	// the server log.
	if strings.Contains(w.Body.String(), auth.ErrStoreStale.Error()) {
		t.Errorf("response leaks the store error: %s", w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeStorageUnavailable)
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
