package account_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spi "github.com/cyoda-platform/cyoda-go-spi"
	genapi "github.com/cyoda-platform/cyoda-go/api"
	"github.com/cyoda-platform/cyoda-go/internal/auth"
	"github.com/cyoda-platform/cyoda-go/internal/common"
	"github.com/cyoda-platform/cyoda-go/internal/common/commontest"
	"github.com/cyoda-platform/cyoda-go/internal/domain/account"
)

// kvOutageDSN stands in for the connection detail a real KV backend error
// carries. Synthetic — it gives the Gate-3 leak assertion something to look for.
const kvOutageDSN = "postgres://u:p@db/cyoda"

type kvOutageErr struct{}

func (kvOutageErr) Error() string            { return "acquire timed out: " + kvOutageDSN }
func (kvOutageErr) StorageUnavailable() bool { return true }

// outageTrustedKeyStore fails every mutation the way KVTrustedKeyStore does when
// the KV write itself fails: the store already wraps the cause, it was the
// handler that threw it away.
type outageTrustedKeyStore struct {
	auth.TrustedKeyStore
}

func (outageTrustedKeyStore) Register(context.Context, *auth.TrustedKey, auth.RotateOptions) error {
	return fmt.Errorf("failed to persist trusted key: %w", kvOutageErr{})
}

func (outageTrustedKeyStore) Delete(context.Context, spi.TenantID, string) error {
	return fmt.Errorf("failed to delete trusted key from KV store: %w", kvOutageErr{})
}

func (outageTrustedKeyStore) Invalidate(context.Context, spi.TenantID, string, int64) error {
	return fmt.Errorf("failed to persist invalidation: %w", kvOutageErr{})
}

func (outageTrustedKeyStore) Reactivate(context.Context, spi.TenantID, string, time.Time, time.Time) error {
	return fmt.Errorf("failed to persist reactivation: %w", kvOutageErr{})
}

func handlerWithTrustedStore(store auth.TrustedKeyStore) *account.Handler {
	feats := auth.DefaultIAMFeatures()
	feats.TrustedKeyRegistrationEnabled = true
	return account.New(nil, nil, auth.NewInMemoryKeyStore(), store, nil, feats)
}

// trustedKeyMutations drives the three handlers whose only failure answer was
// "trusted key not found".
func trustedKeyMutations(h *account.Handler) map[string]func(*testing.T) *httptest.ResponseRecorder {
	reactivateBody, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
	return map[string]func(*testing.T) *httptest.ResponseRecorder{
		"Delete": func(t *testing.T) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			h.DeleteTrustedKey(w, adminReq(t, http.MethodDelete, "/oauth/keys/trusted/k1", nil), "k1")
			return w
		},
		"Invalidate": func(t *testing.T) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			h.InvalidateTrustedKey(w, adminReq(t, http.MethodPut, "/oauth/keys/trusted/k1/invalidate", nil), "k1")
			return w
		},
		"Reactivate": func(t *testing.T) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			h.ReactivateTrustedKey(w, adminReq(t, http.MethodPut, "/oauth/keys/trusted/k1/reactivate", reactivateBody), "k1")
			return w
		},
	}
}

// A KV write that failed because storage was unavailable is not a missing key.
// Answering 404 tells the admin the key is gone — during an outage, when it is
// not — and a client that believes that stops retrying.
func TestTrustedKeyMutations_StorageOutage_Return503(t *testing.T) {
	h := handlerWithTrustedStore(outageTrustedKeyStore{})
	for name, call := range trustedKeyMutations(h) {
		t.Run(name, func(t *testing.T) {
			w := call(t)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeStorageUnavailable)
			var pd struct {
				Properties map[string]any `json:"properties"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
				t.Fatalf("decode problem detail: %v; body: %s", err, w.Body.String())
			}
			if r, _ := pd.Properties["retryable"].(bool); !r {
				t.Errorf("503 is not advertised as retryable; body: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), kvOutageDSN) {
				t.Errorf("response leaked storage internals: %s", w.Body.String())
			}
		})
	}
}

// The other direction: a key that genuinely is not registered still answers
// 404 TRUSTED_KEY_NOT_FOUND. The in-memory store is the real one here — no
// stubbing — so this asserts the shipped behaviour, not a double's.
func TestTrustedKeyMutations_UnknownKey_Still404(t *testing.T) {
	h := handlerWithTrustedStore(auth.NewInMemoryTrustedKeyStore())
	for name, call := range trustedKeyMutations(h) {
		t.Run(name, func(t *testing.T) {
			w := call(t)
			if w.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
			}
			commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeTrustedKeyNotFound)
		})
	}
}

// Register goes through the same trustedKeyMutationError routing as the
// other four handlers: a KV write failure is a 503, not a 500 that leaks
// storage internals, and not misread as some domain 4xx.
func TestRegisterTrustedKey_StorageOutage_Return503(t *testing.T) {
	h := handlerWithTrustedStore(outageTrustedKeyStore{})
	body, _ := json.Marshal(genapi.RegisterTrustedKeyRequestDto{KeyId: "k1", Jwk: rsaJWK(t, "k1"), Audience: "human"})
	w := httptest.NewRecorder()
	h.RegisterTrustedKey(w, adminReq(t, http.MethodPost, "/oauth/keys/trusted", body))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeStorageUnavailable)
	var pd struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &pd); err != nil {
		t.Fatalf("decode problem detail: %v; body: %s", err, w.Body.String())
	}
	if r, _ := pd.Properties["retryable"].(bool); !r {
		t.Errorf("503 is not advertised as retryable; body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), kvOutageDSN) {
		t.Errorf("response leaked storage internals: %s", w.Body.String())
	}
}

// reactivateThenMissingStore simulates Reactivate succeeding on this node
// while the immediate read-back misses the key — e.g. a concurrent delete
// landing on another node between the write and the read. It exercises
// ReactivateTrustedKey's post-Reactivate Get: that failure must route
// through trustedKeyMutationError like every other trusted-key store
// failure, not an unconditional 500.
type reactivateThenMissingStore struct {
	auth.TrustedKeyStore
}

func (reactivateThenMissingStore) Reactivate(context.Context, spi.TenantID, string, time.Time, time.Time) error {
	return nil
}

func (reactivateThenMissingStore) Get(context.Context, spi.TenantID, string) (*auth.TrustedKey, error) {
	return nil, fmt.Errorf("%w: k1", auth.ErrTrustedKeyNotFound)
}

func TestReactivateTrustedKey_GetAfterReactivate_NotFound_Returns404(t *testing.T) {
	h := handlerWithTrustedStore(reactivateThenMissingStore{})
	body, _ := json.Marshal(genapi.ReactivateKeyRequestDto{ValidTo: time.Now().Add(24 * time.Hour)})
	w := httptest.NewRecorder()
	h.ReactivateTrustedKey(w, adminReq(t, http.MethodPut, "/oauth/keys/trusted/k1/reactivate", body), "k1")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	commontest.ExpectErrorCode(t, w.Result(), common.ErrCodeTrustedKeyNotFound)
}
